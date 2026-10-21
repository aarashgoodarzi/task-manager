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

	"github.com/aarashgoodarzi/task-manager/internal/task/domain"
)

// ErrNotFound is returned when a requested task does not exist. Adapters return
// it unchanged, so callers can rely on errors.Is across adapters.
var ErrNotFound = errors.New("task not found")

// Repository is the persistence port the use cases need. It is declared here,
// beside its only consumer, rather than in a shared ports package.
type Repository interface {
	Create(ctx context.Context, task domain.Task) (domain.Task, error)
	List(ctx context.Context) ([]domain.Task, error)
	Get(ctx context.Context, id string) (domain.Task, error)
	Update(ctx context.Context, task domain.Task) (domain.Task, error)
	Delete(ctx context.Context, id string) error
}

// Input carries the mutable fields accepted by create and update. Status is
// the raw request value; an empty value selects the default status.
type Input struct {
	Title       string
	Description string
	Status      string
}

// ListSnapshot is the result of a list-cache lookup. Token is an opaque
// generation marker: the cache returns it with a miss so Set can write only
// under the generation the read actually observed. A read that raced with a
// mutation therefore cannot publish its old result into the new generation.
type ListSnapshot struct {
	Tasks []domain.Task
	Token string
	Hit   bool
}

// ListCache is the task-list cache port. It is declared here, beside its only
// consumer, so the Redis adapter imports this package rather than the reverse.
// An implementation must be safe for concurrent use.
type ListCache interface {
	Get(ctx context.Context) (ListSnapshot, error)
	Set(ctx context.Context, snapshot ListSnapshot) error
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

// List returns every stored task. The order is defined by the adapter. A
// cached list is returned without touching PostgreSQL; any cache failure falls
// back to the database.
func (s *Service) List(ctx context.Context) ([]domain.Task, error) {
	var snapshot ListSnapshot
	cacheReadable := false

	if s.cache != nil {
		loaded, err := s.cache.Get(ctx)
		switch {
		case err != nil:
			s.logCacheError("read", err)
		case loaded.Hit:
			return loaded.Tasks, nil
		default:
			snapshot = loaded
			cacheReadable = true
		}
	}

	tasks, err := s.repo.List(ctx)
	if err != nil {
		return nil, fmt.Errorf("service: list tasks: %w", err)
	}

	if cacheReadable {
		snapshot.Tasks = tasks
		if err := s.cache.Set(ctx, snapshot); err != nil {
			s.logCacheError("write", err)
		}
	}
	return tasks, nil
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

// invalidateList invalidates the cached list after a committed mutation. The
// request context is deliberately detached from cancellation: once PostgreSQL
// has committed, a disconnected client must not prevent invalidation.
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
	}
	if err := task.Validate(); err != nil {
		return domain.Task{}, err
	}
	return task, nil
}
