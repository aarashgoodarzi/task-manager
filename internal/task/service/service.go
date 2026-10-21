// Package service implements the task use cases. It depends on the repository
// port declared below and knows nothing about Gin, HTTP, or SQL.
package service

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/aarashgoodarzi/task-manager/internal/task/domain"
)

// ErrNotFound is returned when a requested task does not exist. Adapters return
// it unchanged, so callers can rely on errors.Is across adapters.
var ErrNotFound = errors.New("task not found")

const (
	// DefaultPageSize is the page size used when a request omits page_size.
	DefaultPageSize = 20

	// MaxPageSize is the largest page a request may ask for. Larger values are
	// rejected rather than clamped, so a client never receives a page different
	// from the one it requested.
	MaxPageSize = 100
)

// Repository is the persistence port the use cases need. It is declared here,
// beside its only consumer, rather than in a shared ports package.
type Repository interface {
	Create(ctx context.Context, task domain.Task) (domain.Task, error)
	// ListPage returns one page of matching tasks ordered by id ascending.
	// When AfterID is set, only rows with id greater than AfterID are returned.
	// Limit is the maximum number of rows to fetch; the use case may request
	// one extra row to detect whether another page exists.
	ListPage(ctx context.Context, filter Filter) (TaskPage, error)
	Get(ctx context.Context, id string) (domain.Task, error)
	Update(ctx context.Context, task domain.Task) (domain.Task, error)
	Delete(ctx context.Context, id string) error
}

// Input carries the mutable fields accepted by create and update. Status and
// Assignee are raw request values; empty values select their defaults.
type Input struct {
	Title       string
	Description string
	Status      string
	Assignee    string
}

// ListQuery is a transport-independent task-list query. The zero value asks
// for the first page with no filters, and Normalize fills in the defaults.
type ListQuery struct {
	// Status limits the result to one status. The zero Status means "any".
	Status domain.Status
	// Assignee limits the result to tasks assigned to exactly this label. An
	// empty value means "any"; unassigned tasks are not selectable in this
	// version.
	Assignee string
	// Cursor is the opaque pagination token from a previous response. Empty
	// means the first page. A malformed or incompatible cursor is rejected.
	Cursor string
	// PageSize is the maximum number of tasks in the page. Zero means
	// DefaultPageSize.
	PageSize int
	// AfterID is the exclusive lower bound derived from Cursor. It is empty
	// for the first page and is set by Normalize; callers should not set it.
	AfterID string
}

// Normalize applies the documented defaults and rejects values outside the
// supported range. It decodes and binds the cursor to this query's filters and
// page size. It is idempotent, so an HTTP adapter may validate a request once
// and the use case may rely on the same rules.
func (q ListQuery) Normalize() (ListQuery, error) {
	if q.Status != "" && !q.Status.Valid() {
		return ListQuery{}, &domain.ValidationError{
			Field:   "status",
			Message: `status must be one of "todo", "in_progress", "done"`,
		}
	}

	assignee := strings.TrimSpace(q.Assignee)
	if utf8.RuneCountInString(assignee) > domain.MaxAssigneeLength {
		return ListQuery{}, &domain.ValidationError{
			Field:   "assignee",
			Message: fmt.Sprintf("assignee must be at most %d characters", domain.MaxAssigneeLength),
		}
	}

	pageSize := q.PageSize
	if pageSize == 0 {
		pageSize = DefaultPageSize
	}
	if pageSize < 0 {
		return ListQuery{}, &domain.ValidationError{Field: "page_size", Message: "page_size must be at least 1"}
	}
	if pageSize > MaxPageSize {
		return ListQuery{}, &domain.ValidationError{
			Field:   "page_size",
			Message: fmt.Sprintf("page_size must be at most %d", MaxPageSize),
		}
	}

	normalized := ListQuery{
		Status:   q.Status,
		Assignee: assignee,
		Cursor:   q.Cursor,
		PageSize: pageSize,
	}

	if q.Cursor != "" {
		afterID, err := DecodeCursor(q.Cursor, normalized)
		if err != nil {
			return ListQuery{}, err
		}
		normalized.AfterID = afterID
	}

	return normalized, nil
}

// Filter is the keyset query the repository executes. AfterID is empty for the
// first page and otherwise the exclusive lower bound on id.
type Filter struct {
	Status   domain.Status
	Assignee string
	Limit    int
	AfterID  string
}

// TaskPage is one page of repository results. Totals are not computed: cursor
// pagination does not need an exact count on every request.
type TaskPage struct {
	Tasks []domain.Task
}

// ListResult is the complete list response: the page, whether another page
// exists, and an opaque cursor to continue when it does.
type ListResult struct {
	Tasks      []domain.Task
	PageSize   int
	NextCursor string
	HasMore    bool
}

// ListSnapshot is the result of a list-cache lookup. Token is an opaque
// generation marker: the cache returns it with a miss so Set can write only
// under the generation the read actually observed. A read that raced with a
// mutation therefore cannot publish its old result into the new generation.
type ListSnapshot struct {
	Result ListResult
	Token  string
	Hit    bool
}

// ListCache is the task-list cache port. It is declared here, beside its only
// consumer, so the Redis adapter imports this package rather than the reverse.
// Cache entries are per query variant and are all discarded together when a
// mutation invalidates the generation. An implementation must be safe for
// concurrent use.
type ListCache interface {
	Get(ctx context.Context, query ListQuery) (ListSnapshot, error)
	Set(ctx context.Context, query ListQuery, snapshot ListSnapshot) error
	Invalidate(ctx context.Context) error
}

// cacheInvalidationTimeout bounds the post-mutation cache work. A committed
// write must not wait indefinitely for Redis.
const cacheInvalidationTimeout = 2 * time.Second

// Service implements task CRUD on top of a Repository. cache is optional: nil
// disables caching without changing behavior.
type Service struct {
	repo  Repository
	cache ListCache
	log   *slog.Logger
}

// New returns a Service backed by repo. cache may be nil; log may be nil, in
// which case cache failures are silent.
func New(repo Repository, cache ListCache, log *slog.Logger) *Service {
	return &Service{repo: repo, cache: cache, log: log}
}

// Create validates the input and stores a new task. The repository assigns the
// identifier.
func (s *Service) Create(ctx context.Context, in Input) (domain.Task, error) {
	task, err := buildTask("", in)
	if err != nil {
		return domain.Task{}, err
	}

	created, err := s.repo.Create(ctx, task)
	if err != nil {
		return domain.Task{}, fmt.Errorf("service: create task: %w", err)
	}

	// Invalidate only after the write committed. A cache failure is logged and
	// never turns a successful create into an API error.
	s.invalidateList(ctx, "create", created.ID)
	return created, nil
}

// List returns one page of tasks matching query. A cached page is returned
// without touching PostgreSQL; any cache failure falls back to the database.
//
// Pagination is keyset-based on id: the repository fetches one extra row so
// HasMore and NextCursor can be computed without a COUNT(*). Concurrent
// inserts, deletes, and updates between requests can still change which rows
// appear on later pages; the cursor only prevents the classic offset-shift
// skip/duplicate pattern, not a multi-request snapshot.
func (s *Service) List(ctx context.Context, query ListQuery) (ListResult, error) {
	query, err := query.Normalize()
	if err != nil {
		return ListResult{}, err
	}

	var snapshot ListSnapshot
	cacheReadable := false

	if s.cache != nil {
		loaded, err := s.cache.Get(ctx, query)
		switch {
		case err != nil:
			s.logCacheError("read", err)
		case loaded.Hit:
			return loaded.Result, nil
		default:
			snapshot = loaded
			cacheReadable = true
		}
	}

	// Fetch one extra row to learn whether another page exists without a
	// separate count query.
	page, err := s.repo.ListPage(ctx, Filter{
		Status:   query.Status,
		Assignee: query.Assignee,
		Limit:    query.PageSize + 1,
		AfterID:  query.AfterID,
	})
	if err != nil {
		return ListResult{}, fmt.Errorf("service: list tasks: %w", err)
	}

	tasks := page.Tasks
	if tasks == nil {
		// An empty page is still a page: keep it as [] rather than nil so the
		// JSON encoder cannot turn it into null.
		tasks = []domain.Task{}
	}

	hasMore := len(tasks) > query.PageSize
	if hasMore {
		tasks = tasks[:query.PageSize]
	}

	nextCursor := ""
	if hasMore {
		encoded, err := EncodeCursor(tasks[len(tasks)-1].ID, query)
		if err != nil {
			return ListResult{}, fmt.Errorf("service: list tasks: %w", err)
		}
		nextCursor = encoded
	}

	result := ListResult{
		Tasks:      tasks,
		PageSize:   query.PageSize,
		NextCursor: nextCursor,
		HasMore:    hasMore,
	}

	if cacheReadable {
		snapshot.Result = result
		if err := s.cache.Set(ctx, query, snapshot); err != nil {
			s.logCacheError("write", err)
		}
	}
	return result, nil
}

// Get returns the task with the given identifier.
func (s *Service) Get(ctx context.Context, id string) (domain.Task, error) {
	task, err := s.repo.Get(ctx, id)
	if err != nil {
		return domain.Task{}, fmt.Errorf("service: get task: %w", err)
	}
	return task, nil
}

// Update replaces the mutable fields of an existing task. A missing task is
// reported as ErrNotFound; it is never created implicitly.
func (s *Service) Update(ctx context.Context, id string, in Input) (domain.Task, error) {
	task, err := buildTask(id, in)
	if err != nil {
		return domain.Task{}, err
	}

	updated, err := s.repo.Update(ctx, task)
	if err != nil {
		return domain.Task{}, fmt.Errorf("service: update task: %w", err)
	}

	s.invalidateList(ctx, "update", updated.ID)
	return updated, nil
}

// Delete removes an existing task.
func (s *Service) Delete(ctx context.Context, id string) error {
	if err := s.repo.Delete(ctx, id); err != nil {
		return fmt.Errorf("service: delete task: %w", err)
	}

	s.invalidateList(ctx, "delete", id)
	return nil
}

// invalidateList invalidates every cached list variant after a committed
// mutation. The request context is deliberately detached from cancellation:
// once PostgreSQL has committed, a disconnected client must not prevent
// invalidation.
func (s *Service) invalidateList(ctx context.Context, operation, taskID string) {
	if s.cache == nil {
		return
	}

	invalidateCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cacheInvalidationTimeout)
	defer cancel()

	if err := s.cache.Invalidate(invalidateCtx); err != nil {
		if s.log != nil {
			s.log.Warn("task list cache invalidation failed",
				slog.String("operation", operation),
				slog.String("task_id", taskID),
				slog.Any("error", err),
			)
		}
	}
}

// logCacheError reports a cache read or write failure without failing the
// request; PostgreSQL remains the response source in both cases.
func (s *Service) logCacheError(operation string, err error) {
	if s.log == nil {
		return
	}
	s.log.Warn("task list cache failure",
		slog.String("operation", operation),
		slog.Any("error", err),
	)
}

// buildTask normalizes input into a validated aggregate. id is empty for
// creation, where the repository generates it.
func buildTask(id string, in Input) (domain.Task, error) {
	status, err := domain.ParseStatus(in.Status)
	if err != nil {
		return domain.Task{}, err
	}

	task := domain.Task{
		ID:          id,
		Title:       strings.TrimSpace(in.Title),
		Description: in.Description,
		Status:      status,
		Assignee:    strings.TrimSpace(in.Assignee),
	}
	if err := task.Validate(); err != nil {
		return domain.Task{}, err
	}
	return task, nil
}
