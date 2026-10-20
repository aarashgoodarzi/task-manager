// Package postgres is the PostgreSQL driven adapter for tasks. It implements
// the repository port declared by internal/task/service using explicit SQL over
// a *sql.DB, and knows nothing about HTTP or Gin.
package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/aarashgoodarzi/task-manager/internal/task/domain"
	"github.com/aarashgoodarzi/task-manager/internal/task/service"
)

// maxIDAttempts bounds retries when a generated identifier already exists.
const maxIDAttempts = 5

// The service port is the contract this adapter exists to satisfy.
var _ service.Repository = (*Repository)(nil)

// selectColumns keeps every read and RETURNING clause in one place, so scanning
// stays consistent with the stored column order.
const selectColumns = "id, title, description, status"

// Repository persists tasks in the tasks table.
type Repository struct {
	db    *sql.DB
	newID func() (string, error)
}

// New returns a Repository backed by db. The pool is owned by the caller;
// closing it is the composition root's responsibility.
func New(db *sql.DB) *Repository {
	return &Repository{db: db, newID: domain.NewID}
}

// Create inserts a task under a freshly generated identifier. The identifier is
// generated here, not by the client, and a collision is retried a bounded
// number of times instead of surfacing as an error.
func (r *Repository) Create(ctx context.Context, task domain.Task) (domain.Task, error) {
	if r.db == nil {
		return domain.Task{}, fmt.Errorf("postgres: create task: nil pool")
	}

	for attempt := 0; attempt < maxIDAttempts; attempt++ {
		id, err := r.newID()
		if err != nil {
			return domain.Task{}, fmt.Errorf("postgres: generate task id: %w", err)
		}

		stored, err := scanTask(r.db.QueryRowContext(ctx,
			`INSERT INTO tasks (id, title, description, status)
			 VALUES ($1::uuid, $2, $3, $4)
			 ON CONFLICT (id) DO NOTHING
			 RETURNING `+selectColumns,
			id, task.Title, task.Description, string(task.Status),
		))
		if errors.Is(err, sql.ErrNoRows) {
			continue // the generated identifier exists; try another one
		}
		if err != nil {
			return domain.Task{}, fmt.Errorf("postgres: create task: %w", err)
		}
		return stored, nil
	}

	return domain.Task{}, fmt.Errorf("postgres: could not generate a unique task id in %d attempts", maxIDAttempts)
}

// List returns every task ordered by identifier, which makes the response
// stable across calls.
func (r *Repository) List(ctx context.Context) ([]domain.Task, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT `+selectColumns+` FROM tasks ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("postgres: list tasks: %w", err)
	}
	defer rows.Close()

	tasks := make([]domain.Task, 0)
	for rows.Next() {
		task, err := scanTask(rows)
		if err != nil {
			return nil, fmt.Errorf("postgres: list tasks: %w", err)
		}
		tasks = append(tasks, task)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("postgres: list tasks: %w", err)
	}
	return tasks, nil
}

// Get returns the task with the given identifier, or service.ErrNotFound.
func (r *Repository) Get(ctx context.Context, id string) (domain.Task, error) {
	task, err := scanTask(r.db.QueryRowContext(ctx,
		`SELECT `+selectColumns+` FROM tasks WHERE id = $1::uuid`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return domain.Task{}, service.ErrNotFound
	}
	if err != nil {
		return domain.Task{}, fmt.Errorf("postgres: get task: %w", err)
	}
	return task, nil
}

// Update replaces every mutable field of an existing task. It never inserts a
// missing task: a missing row is reported as service.ErrNotFound.
func (r *Repository) Update(ctx context.Context, task domain.Task) (domain.Task, error) {
	updated, err := scanTask(r.db.QueryRowContext(ctx,
		`UPDATE tasks
		 SET title = $2, description = $3, status = $4
		 WHERE id = $1::uuid
		 RETURNING `+selectColumns,
		task.ID, task.Title, task.Description, string(task.Status),
	))
	if errors.Is(err, sql.ErrNoRows) {
		return domain.Task{}, service.ErrNotFound
	}
	if err != nil {
		return domain.Task{}, fmt.Errorf("postgres: update task: %w", err)
	}
	return updated, nil
}

// Delete removes an existing task, or returns service.ErrNotFound when no row
// matched the identifier.
func (r *Repository) Delete(ctx context.Context, id string) error {
	result, err := r.db.ExecContext(ctx, `DELETE FROM tasks WHERE id = $1::uuid`, id)
	if err != nil {
		return fmt.Errorf("postgres: delete task: %w", err)
	}

	deleted, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("postgres: delete task: rows affected: %w", err)
	}
	if deleted == 0 {
		return service.ErrNotFound
	}
	return nil
}

// scanner is the subset of *sql.Row and *sql.Rows that scanTask needs.
type scanner interface {
	Scan(dest ...any) error
}

// scanTask reads one row in selectColumns order and converts the stored status
// back into the domain type.
func scanTask(row scanner) (domain.Task, error) {
	var (
		task   domain.Task
		status string
	)
	if err := row.Scan(&task.ID, &task.Title, &task.Description, &status); err != nil {
		return domain.Task{}, err
	}
	task.Status = domain.Status(status)
	return task, nil
}
