// Package postgres is the PostgreSQL driven adapter for tasks. It implements
// the repository port declared by internal/task/service using explicit SQL over
// a *sql.DB, and knows nothing about HTTP or Gin.
package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/aarashgoodarzi/task-manager/internal/task/domain"
	"github.com/aarashgoodarzi/task-manager/internal/task/service"
)

// maxIDAttempts bounds retries when a generated identifier already exists.
const maxIDAttempts = 5

// The service port is the contract this adapter exists to satisfy.
var _ service.Repository = (*Repository)(nil)

// selectColumns keeps every read and RETURNING clause in one place, so scanning
// stays consistent with the stored column order.
const selectColumns = "id, title, description, status, assignee, created_at"

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
			`INSERT INTO tasks (id, title, description, status, assignee)
			 VALUES ($1::uuid, $2, $3, $4, $5)
			 ON CONFLICT (id) DO NOTHING
			 RETURNING `+selectColumns,
			id, task.Title, task.Description, string(task.Status), nullableAssignee(task.Assignee),
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

// ListPage returns one page of tasks matching filter, ordered FIFO (oldest
// first: created_at ASC, id ASC). When AfterCreatedAt/AfterID are set, only
// rows strictly newer than that keyset position are returned. Filtering,
// ordering, and the limit all happen in PostgreSQL: the adapter never loads
// the collection to slice it in memory.
func (r *Repository) ListPage(ctx context.Context, filter service.Filter) (service.TaskPage, error) {
	if r.db == nil {
		return service.TaskPage{}, fmt.Errorf("postgres: list tasks: nil pool")
	}

	where, args := filterClause(filter)

	// LIMIT is bound as a parameter, like every other value in this adapter,
	// so no user input is ever interpolated into SQL.
	pageArgs := make([]any, 0, len(args)+1)
	pageArgs = append(pageArgs, args...)
	pageArgs = append(pageArgs, filter.Limit)

	rows, err := r.db.QueryContext(ctx,
		`SELECT `+selectColumns+` FROM tasks`+where+
			fmt.Sprintf(` ORDER BY created_at ASC, id ASC LIMIT $%d`, len(args)+1),
		pageArgs...)
	if err != nil {
		return service.TaskPage{}, fmt.Errorf("postgres: list tasks: %w", err)
	}
	defer rows.Close()

	tasks := make([]domain.Task, 0)
	for rows.Next() {
		task, err := scanTask(rows)
		if err != nil {
			return service.TaskPage{}, fmt.Errorf("postgres: list tasks: %w", err)
		}
		tasks = append(tasks, task)
	}
	if err := rows.Err(); err != nil {
		return service.TaskPage{}, fmt.Errorf("postgres: list tasks: %w", err)
	}

	return service.TaskPage{Tasks: tasks}, nil
}

// filterClause builds the WHERE clause for status, assignee, and optional
// FIFO keyset bound. Values are always bound as parameters; the placeholder
// numbers follow the argument order.
func filterClause(filter service.Filter) (string, []any) {
	conditions := make([]string, 0, 3)
	args := make([]any, 0, 4)

	if filter.Status != "" {
		args = append(args, string(filter.Status))
		conditions = append(conditions, fmt.Sprintf("status = $%d", len(args)))
	}
	if filter.Assignee != "" {
		args = append(args, filter.Assignee)
		conditions = append(conditions, fmt.Sprintf("assignee = $%d", len(args)))
	}
	if filter.AfterID != "" {
		// Exclusive lower bound for ASC walk: rows newer than the cursor.
		args = append(args, filter.AfterCreatedAt, filter.AfterID)
		createdAtParam := len(args) - 1
		idParam := len(args)
		conditions = append(conditions, fmt.Sprintf(
			"(created_at, id) > ($%d::timestamptz, $%d::uuid)", createdAtParam, idParam,
		))
	}
	if len(conditions) == 0 {
		return "", args
	}
	return " WHERE " + strings.Join(conditions, " AND "), args
}

// CountTasks returns the number of rows currently in the tasks table. It is a
// full-table count, so the metrics refresher calls it on its own schedule
// rather than the HTTP layer calling it per request. It reads the database, not
// an adapter-local counter, so tasks created or deleted by another process (or
// by direct SQL) are included.
func (r *Repository) CountTasks(ctx context.Context) (int64, error) {
	if r.db == nil {
		return 0, fmt.Errorf("postgres: count tasks: nil pool")
	}

	var count int64
	if err := r.db.QueryRowContext(ctx, `SELECT count(*) FROM tasks`).Scan(&count); err != nil {
		return 0, fmt.Errorf("postgres: count tasks: %w", err)
	}
	return count, nil
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
		 SET title = $2, description = $3, status = $4, assignee = $5
		 WHERE id = $1::uuid
		 RETURNING `+selectColumns,
		task.ID, task.Title, task.Description, string(task.Status), nullableAssignee(task.Assignee),
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

// nullableAssignee maps the domain's "empty means unassigned" rule onto SQL
// NULL, so an unassigned task has exactly one stored representation.
func nullableAssignee(assignee string) any {
	if assignee == "" {
		return nil
	}
	return assignee
}

// scanner is the subset of *sql.Row and *sql.Rows that scanTask needs.
type scanner interface {
	Scan(dest ...any) error
}

// scanTask reads one row in selectColumns order and converts the stored status
// back into the domain type.
func scanTask(row scanner) (domain.Task, error) {
	var (
		task     domain.Task
		status   string
		assignee sql.NullString
	)
	if err := row.Scan(
		&task.ID, &task.Title, &task.Description, &status, &assignee, &task.CreatedAt,
	); err != nil {
		return domain.Task{}, err
	}
	task.Status = domain.Status(status)
	task.Assignee = assignee.String
	return task, nil
}
