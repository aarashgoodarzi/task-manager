// Package service implements the task use cases. It depends on the repository
// port declared below and knows nothing about Gin, HTTP, or SQL.
package service

import (
	"context"
	"errors"
	"fmt"
	"strings"

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

// Service implements task CRUD on top of a Repository.
type Service struct {
	repo Repository
}

// New returns a Service backed by repo.
func New(repo Repository) *Service {
	return &Service{repo: repo}
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
	return created, nil
}

// List returns every stored task. The order is defined by the adapter.
func (s *Service) List(ctx context.Context) ([]domain.Task, error) {
	tasks, err := s.repo.List(ctx)
	if err != nil {
		return nil, fmt.Errorf("service: list tasks: %w", err)
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
	return updated, nil
}

// Delete removes an existing task.
func (s *Service) Delete(ctx context.Context, id string) error {
	if err := s.repo.Delete(ctx, id); err != nil {
		return fmt.Errorf("service: delete task: %w", err)
	}
	return nil
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
