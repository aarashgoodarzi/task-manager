// Package service declares the repository port for task persistence. The use
// cases that consume it are added with the HTTP API; the port is declared here,
// beside its only consumer, rather than in a shared contracts package.
package service

import (
	"context"
	"errors"

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
