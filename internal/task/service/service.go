// Package service implements the task use cases. It depends on the repository
// port declared below and knows nothing about Gin, HTTP, or SQL.
package service

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
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
	// ListPage returns one page of matching tasks and the total number of
	// matching tasks before pagination. Both values come from the same filter
	// so the page and its metadata cannot disagree.
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
	// Page is one-based. Zero means the first page.
	Page int
	// PageSize is the maximum number of tasks in the page. Zero means
	// DefaultPageSize.
	PageSize int
}

// Normalize applies the documented defaults and rejects values outside the
// supported range. It is idempotent, so an HTTP adapter may validate a request
// once and the use case may rely on the same rules.
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

	page := q.Page
	if page == 0 {
		page = 1
	}
	if page < 0 {
		return ListQuery{}, &domain.ValidationError{Field: "page", Message: "page must be at least 1"}
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

	// The offset is (page-1)*page_size and must fit in an int64 for the SQL
	// parameter. Rejecting an overflowing page is better than silently
	// wrapping it to a negative offset. The division form cannot overflow.
	if int64(page-1) > math.MaxInt64/int64(pageSize) {
		return ListQuery{}, &domain.ValidationError{Field: "page", Message: "page is too large"}
	}

	return ListQuery{Status: q.Status, Assignee: assignee, Page: page, PageSize: pageSize}, nil
}

// Filter is the paginated query the repository executes. Offset is an int64 so
// a large, validated page number cannot overflow the SQL parameter.
type Filter struct {
	Status   domain.Status
	Assignee string
	Limit    int
	Offset   int64
}

// TaskPage is one page of repository results plus the total number of matching
// tasks before pagination.
type TaskPage struct {
	Tasks []domain.Task
	Total int64
}

// ListResult is the complete list response: the page, the total it belongs to,
// and the pagination metadata the HTTP layer renders.
type ListResult struct {
	Tasks      []domain.Task
	Page       int
	PageSize   int
	Total      int64
	TotalPages int
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

	offset := int64(query.Page-1) * int64(query.PageSize)
	page, err := s.repo.ListPage(ctx, Filter{
		Status:   query.Status,
		Assignee: query.Assignee,
		Limit:    query.PageSize,
		Offset:   offset,
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

	result := ListResult{
		Tasks:      tasks,
		Page:       query.Page,
		PageSize:   query.PageSize,
		Total:      page.Total,
		TotalPages: totalPages(page.Total, query.PageSize),
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

// totalPages is the number of pages a filtered result occupies. An empty
// result has zero pages, which is not the same as an empty page.
func totalPages(total int64, pageSize int) int {
	if total == 0 {
		return 0
	}
	return int((total + int64(pageSize) - 1) / int64(pageSize))
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
