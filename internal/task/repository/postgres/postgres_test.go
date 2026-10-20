package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aarashgoodarzi/task-manager/internal/infra/database"
	"github.com/aarashgoodarzi/task-manager/internal/task/domain"
	"github.com/aarashgoodarzi/task-manager/internal/task/service"
)

const unknownID = "11111111-1111-4111-8111-111111111111"

// testDB opens the database named by TEST_DATABASE_URL and applies the schema.
// These are real integration tests: without TEST_DATABASE_URL they skip with an
// explicit message rather than pretending to pass.
func testDB(t *testing.T) *sql.DB {
	t.Helper()

	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL is not set; skipping PostgreSQL integration tests")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	db, err := database.New(database.Config{
		DSN:             dsn,
		MaxOpenConns:    10,
		MaxIdleConns:    5,
		ConnMaxLifetime: time.Minute,
	})
	if err != nil {
		t.Fatalf("open test database: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	if err := db.PingContext(ctx); err != nil {
		t.Fatalf("ping test database: %v", err)
	}

	// The tests truncate the tasks table, so refuse to touch anything that does
	// not look like a dedicated test database.
	var name string
	if err := db.QueryRowContext(ctx, "SELECT current_database()").Scan(&name); err != nil {
		t.Fatalf("read current_database(): %v", err)
	}
	if !strings.Contains(name, "test") {
		t.Fatalf("refusing to run destructive integration tests against database %q: the name must contain \"test\"", name)
	}

	if err := database.Migrate(ctx, db); err != nil {
		t.Fatalf("migrate test database: %v", err)
	}
	return db
}

// newTestRepository returns a repository on an empty tasks table.
func newTestRepository(t *testing.T) (*Repository, *sql.DB) {
	t.Helper()

	db := testDB(t)
	if _, err := db.ExecContext(context.Background(), "TRUNCATE tasks"); err != nil {
		t.Fatalf("truncate tasks: %v", err)
	}
	return New(db), db
}

func mustCreate(t *testing.T, r *Repository, title string) domain.Task {
	t.Helper()

	task, err := r.Create(context.Background(), domain.Task{Title: title, Status: domain.StatusTodo})
	if err != nil {
		t.Fatalf("Create(%q) unexpected error: %v", title, err)
	}
	return task
}

func countTasks(t *testing.T, db *sql.DB) int {
	t.Helper()

	var count int
	if err := db.QueryRowContext(context.Background(), "SELECT count(*) FROM tasks").Scan(&count); err != nil {
		t.Fatalf("count tasks: %v", err)
	}
	return count
}

func TestCreatePersistsTask(t *testing.T) {
	repo, db := newTestRepository(t)

	task, err := repo.Create(context.Background(), domain.Task{
		Title:       "write integration tests",
		Description: "against a real PostgreSQL",
		Status:      domain.StatusInProgress,
	})
	if err != nil {
		t.Fatalf("Create() unexpected error: %v", err)
	}

	if !domain.IsValidID(task.ID) {
		t.Fatalf("ID = %q, want a UUID", task.ID)
	}
	if task.Title != "write integration tests" ||
		task.Description != "against a real PostgreSQL" ||
		task.Status != domain.StatusInProgress {
		t.Fatalf("created task = %+v, want the input values", task)
	}

	// The row exists in the table, not just in the returned value.
	var (
		title  string
		status string
	)
	err = db.QueryRowContext(context.Background(),
		"SELECT title, status FROM tasks WHERE id = $1::uuid", task.ID,
	).Scan(&title, &status)
	if err != nil {
		t.Fatalf("read back created task: %v", err)
	}
	if title != task.Title || status != string(task.Status) {
		t.Fatalf("stored row = (%q, %q), want (%q, %q)", title, status, task.Title, task.Status)
	}

	if got := countTasks(t, db); got != 1 {
		t.Fatalf("row count = %d, want 1", got)
	}
}

func TestCreateAssignsUniqueIDs(t *testing.T) {
	repo, _ := newTestRepository(t)

	seen := make(map[string]struct{}, 50)
	for i := 0; i < 50; i++ {
		task := mustCreate(t, repo, fmt.Sprintf("task %d", i))
		if _, ok := seen[task.ID]; ok {
			t.Fatalf("duplicate ID %q after %d creates", task.ID, i)
		}
		seen[task.ID] = struct{}{}
	}
}

func TestCreateRetriesIDCollisions(t *testing.T) {
	repo, _ := newTestRepository(t)

	existing := mustCreate(t, repo, "existing")

	calls := 0
	repo.newID = func() (string, error) {
		calls++
		if calls == 1 {
			return existing.ID, nil
		}
		return "22222222-2222-4222-8222-222222222222", nil
	}

	task := mustCreate(t, repo, "second")
	if task.ID != "22222222-2222-4222-8222-222222222222" {
		t.Fatalf("ID = %q, want the retried identifier", task.ID)
	}
	if calls != 2 {
		t.Fatalf("generator called %d times, want 2", calls)
	}
}

func TestCreateFailsWhenIDsKeepColliding(t *testing.T) {
	repo, _ := newTestRepository(t)

	existing := mustCreate(t, repo, "existing")
	repo.newID = func() (string, error) { return existing.ID, nil }

	_, err := repo.Create(context.Background(), domain.Task{Title: "second", Status: domain.StatusTodo})
	if err == nil {
		t.Fatal("Create() with repeated collisions: want error, got nil")
	}
	if errors.Is(err, service.ErrNotFound) {
		t.Fatalf("Create() error = %v, want an identifier generation error", err)
	}
}

func TestCreatePropagatesIDGenerationError(t *testing.T) {
	repo, _ := newTestRepository(t)

	genErr := errors.New("entropy unavailable")
	repo.newID = func() (string, error) { return "", genErr }

	_, err := repo.Create(context.Background(), domain.Task{Title: "task", Status: domain.StatusTodo})
	if !errors.Is(err, genErr) {
		t.Fatalf("Create() error = %v, want the wrapped generator error", err)
	}
}

func TestListEmptyReturnsNonNilSlice(t *testing.T) {
	repo, _ := newTestRepository(t)

	tasks, err := repo.List(context.Background())
	if err != nil {
		t.Fatalf("List() unexpected error: %v", err)
	}
	if tasks == nil {
		t.Fatal("List() = nil, want an empty non-nil slice")
	}
	if len(tasks) != 0 {
		t.Fatalf("len(tasks) = %d, want 0", len(tasks))
	}
}

func TestListReturnsTasksOrderedByID(t *testing.T) {
	repo, _ := newTestRepository(t)

	first := mustCreate(t, repo, "first")
	second := mustCreate(t, repo, "second")

	tasks, err := repo.List(context.Background())
	if err != nil {
		t.Fatalf("List() unexpected error: %v", err)
	}
	if len(tasks) != 2 {
		t.Fatalf("len(tasks) = %d, want 2", len(tasks))
	}

	ids := []string{tasks[0].ID, tasks[1].ID}
	if !sort.StringsAreSorted(ids) {
		t.Fatalf("IDs = %v, want ascending identifier order", ids)
	}

	byID := map[string]domain.Task{tasks[0].ID: tasks[0], tasks[1].ID: tasks[1]}
	if byID[first.ID] != first || byID[second.ID] != second {
		t.Fatalf("List() = %+v, want the stored tasks", tasks)
	}
}

func TestGetExisting(t *testing.T) {
	repo, _ := newTestRepository(t)
	created := mustCreate(t, repo, "stored")

	task, err := repo.Get(context.Background(), created.ID)
	if err != nil {
		t.Fatalf("Get() unexpected error: %v", err)
	}
	if task != created {
		t.Fatalf("Get() = %+v, want %+v", task, created)
	}
}

func TestGetMissing(t *testing.T) {
	repo, _ := newTestRepository(t)

	_, err := repo.Get(context.Background(), unknownID)
	if !errors.Is(err, service.ErrNotFound) {
		t.Fatalf("Get() error = %v, want ErrNotFound", err)
	}
}

func TestGetRejectsMalformedIDWithoutNotFound(t *testing.T) {
	repo, _ := newTestRepository(t)

	_, err := repo.Get(context.Background(), "not-a-uuid")
	if err == nil {
		t.Fatal("Get() with a malformed id: want error, got nil")
	}
	if errors.Is(err, service.ErrNotFound) {
		t.Fatalf("Get() error = %v, want a decoding error rather than ErrNotFound", err)
	}
}

func TestUpdateReplacesExistingTask(t *testing.T) {
	repo, _ := newTestRepository(t)
	created := mustCreate(t, repo, "old")

	updated, err := repo.Update(context.Background(), domain.Task{
		ID:          created.ID,
		Title:       "new",
		Description: "new description",
		Status:      domain.StatusDone,
	})
	if err != nil {
		t.Fatalf("Update() unexpected error: %v", err)
	}

	want := domain.Task{ID: created.ID, Title: "new", Description: "new description", Status: domain.StatusDone}
	if updated != want {
		t.Fatalf("Update() = %+v, want %+v", updated, want)
	}

	stored, err := repo.Get(context.Background(), created.ID)
	if err != nil {
		t.Fatalf("Get() unexpected error: %v", err)
	}
	if stored != want {
		t.Fatalf("stored task = %+v, want %+v", stored, want)
	}
}

func TestUpdateMissingDoesNotInsert(t *testing.T) {
	repo, db := newTestRepository(t)

	_, err := repo.Update(context.Background(), domain.Task{
		ID:     unknownID,
		Title:  "ghost",
		Status: domain.StatusTodo,
	})
	if !errors.Is(err, service.ErrNotFound) {
		t.Fatalf("Update() error = %v, want ErrNotFound", err)
	}
	if got := countTasks(t, db); got != 0 {
		t.Fatalf("row count = %d, want 0 after a failed update", got)
	}
}

func TestDeleteExisting(t *testing.T) {
	repo, db := newTestRepository(t)
	created := mustCreate(t, repo, "task")

	if err := repo.Delete(context.Background(), created.ID); err != nil {
		t.Fatalf("Delete() unexpected error: %v", err)
	}
	if _, err := repo.Get(context.Background(), created.ID); !errors.Is(err, service.ErrNotFound) {
		t.Fatalf("Get() after delete error = %v, want ErrNotFound", err)
	}
	if got := countTasks(t, db); got != 0 {
		t.Fatalf("row count = %d, want 0 after delete", got)
	}
}

func TestDeleteMissing(t *testing.T) {
	repo, _ := newTestRepository(t)

	err := repo.Delete(context.Background(), unknownID)
	if !errors.Is(err, service.ErrNotFound) {
		t.Fatalf("Delete() error = %v, want ErrNotFound", err)
	}
}

// TestRepositoryInstancesShareState proves persistence lives in PostgreSQL, not
// in the adapter instance.
func TestRepositoryInstancesShareState(t *testing.T) {
	repo, db := newTestRepository(t)
	created := mustCreate(t, repo, "shared")

	other := New(db)
	fetched, err := other.Get(context.Background(), created.ID)
	if err != nil {
		t.Fatalf("Get() through a second repository: %v", err)
	}
	if fetched != created {
		t.Fatalf("fetched task = %+v, want %+v", fetched, created)
	}
}

func TestConcurrentCreates(t *testing.T) {
	repo, _ := newTestRepository(t)
	const workers = 20

	var wg sync.WaitGroup
	ids := make(chan string, workers)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()

			task, err := repo.Create(context.Background(), domain.Task{
				Title:  fmt.Sprintf("concurrent %d", n),
				Status: domain.StatusTodo,
			})
			if err != nil {
				t.Errorf("Create() unexpected error: %v", err)
				return
			}
			ids <- task.ID
		}(i)
	}
	wg.Wait()
	close(ids)

	unique := make(map[string]struct{}, workers)
	for id := range ids {
		if _, ok := unique[id]; ok {
			t.Fatalf("duplicate ID %q", id)
		}
		unique[id] = struct{}{}
	}

	tasks, err := repo.List(context.Background())
	if err != nil {
		t.Fatalf("List() unexpected error: %v", err)
	}
	if len(tasks) != workers {
		t.Fatalf("len(tasks) = %d, want %d", len(tasks), workers)
	}
}

// TestSchemaConstraints rejects rows the domain rejects, proving the database
// enforces the same invariants instead of trusting the adapter alone.
func TestSchemaConstraints(t *testing.T) {
	_, db := newTestRepository(t)

	tests := []struct {
		name   string
		query  string
		args   []any
		status string
	}{
		{
			name: "blank title",
			query: `INSERT INTO tasks (id, title, description, status)
			        VALUES ($1::uuid, '   ', '', 'todo')`,
			args: []any{"33333333-3333-4333-8333-333333333333"},
		},
		{
			name: "unknown status",
			query: `INSERT INTO tasks (id, title, description, status)
			        VALUES ($1::uuid, 'title', '', 'pending')`,
			args: []any{"44444444-4444-4444-8444-444444444444"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := db.ExecContext(context.Background(), tc.query, tc.args...); err == nil {
				t.Fatal("INSERT succeeded, want a constraint violation")
			}
		})
	}
}
