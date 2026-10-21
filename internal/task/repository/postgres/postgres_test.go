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

// allTasks is a filter that selects every task with a page large enough for
// these tests, so tests that do not exercise pagination stay readable.
var allTasks = service.Filter{Limit: 1000}

func mustCreateWith(t *testing.T, r *Repository, task domain.Task) domain.Task {
	t.Helper()

	created, err := r.Create(context.Background(), task)
	if err != nil {
		t.Fatalf("Create(%+v) unexpected error: %v", task, err)
	}
	return created
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

	page, err := repo.ListPage(context.Background(), allTasks)
	if err != nil {
		t.Fatalf("ListPage() unexpected error: %v", err)
	}
	if page.Tasks == nil {
		t.Fatal("ListPage() = nil, want an empty non-nil slice")
	}
	if len(page.Tasks) != 0 {
		t.Fatalf("len(tasks) = %d, want 0", len(page.Tasks))
	}
}

func TestListReturnsTasksOrderedByID(t *testing.T) {
	repo, _ := newTestRepository(t)

	first := mustCreate(t, repo, "first")
	second := mustCreate(t, repo, "second")

	page, err := repo.ListPage(context.Background(), allTasks)
	if err != nil {
		t.Fatalf("ListPage() unexpected error: %v", err)
	}
	if len(page.Tasks) != 2 {
		t.Fatalf("page = %+v, want two tasks", page)
	}

	ids := []string{page.Tasks[0].ID, page.Tasks[1].ID}
	if !sort.StringsAreSorted(ids) {
		t.Fatalf("IDs = %v, want ascending identifier order", ids)
	}

	byID := map[string]domain.Task{page.Tasks[0].ID: page.Tasks[0], page.Tasks[1].ID: page.Tasks[1]}
	if byID[first.ID] != first || byID[second.ID] != second {
		t.Fatalf("ListPage() = %+v, want the stored tasks", page.Tasks)
	}
}

func TestCountTasksRejectsNilPool(t *testing.T) {
	if _, err := New(nil).CountTasks(context.Background()); err == nil {
		t.Fatal("CountTasks() with a nil pool: want error, got nil")
	}
}

// TestCountTasksReflectsPersistedState proves the metric source is the table,
// not an adapter-local counter: deletions and writes from another connection
// are both visible.
func TestCountTasksReflectsPersistedState(t *testing.T) {
	repo, db := newTestRepository(t)
	ctx := context.Background()

	if got, err := repo.CountTasks(ctx); err != nil || got != 0 {
		t.Fatalf("CountTasks() = %d, %v; want 0, nil on an empty table", got, err)
	}

	first := mustCreate(t, repo, "first")
	mustCreate(t, repo, "second")

	if got, err := repo.CountTasks(ctx); err != nil || got != 2 {
		t.Fatalf("CountTasks() = %d, %v; want 2, nil after two creates", got, err)
	}

	// Simulate a writer outside this process, for example a manual statement.
	if _, err := db.ExecContext(ctx,
		`INSERT INTO tasks (id, title, description, status)
		 VALUES ('55555555-5555-4555-8555-555555555555', 'external', '', 'todo')`,
	); err != nil {
		t.Fatalf("insert external task: %v", err)
	}
	if got, err := repo.CountTasks(ctx); err != nil || got != 3 {
		t.Fatalf("CountTasks() = %d, %v; want 3, nil after an external insert", got, err)
	}

	if err := repo.Delete(ctx, first.ID); err != nil {
		t.Fatalf("Delete() unexpected error: %v", err)
	}
	if got, err := repo.CountTasks(ctx); err != nil || got != 2 {
		t.Fatalf("CountTasks() = %d, %v; want 2, nil after a delete", got, err)
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

	page, err := repo.ListPage(context.Background(), allTasks)
	if err != nil {
		t.Fatalf("ListPage() unexpected error: %v", err)
	}
	if len(page.Tasks) != workers {
		t.Fatalf("len(tasks) = %d, want %d", len(page.Tasks), workers)
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
		{
			name: "blank assignee",
			query: `INSERT INTO tasks (id, title, description, status, assignee)
			        VALUES ($1::uuid, 'title', '', 'todo', '   ')`,
			args: []any{"55555555-5555-4555-8555-555555555555"},
		},
		{
			name: "assignee too long",
			query: `INSERT INTO tasks (id, title, description, status, assignee)
			        VALUES ($1::uuid, 'title', '', 'todo', repeat('a', 101))`,
			args: []any{"66666666-6666-4666-8666-666666666666"},
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

func TestCreatePersistsAssignee(t *testing.T) {
	repo, db := newTestRepository(t)

	created := mustCreateWith(t, repo, domain.Task{
		Title: "assigned", Status: domain.StatusTodo, Assignee: "alice",
	})
	if created.Assignee != "alice" {
		t.Fatalf("Assignee = %q, want alice", created.Assignee)
	}

	var assignee sql.NullString
	if err := db.QueryRowContext(context.Background(),
		`SELECT assignee FROM tasks WHERE id = $1::uuid`, created.ID,
	).Scan(&assignee); err != nil {
		t.Fatalf("read stored assignee: %v", err)
	}
	if !assignee.Valid || assignee.String != "alice" {
		t.Fatalf("stored assignee = %+v, want alice", assignee)
	}
}

// TestCreateStoresUnassignedAsNull pins the single representation of "no
// assignee": the API returns "", the row stores NULL.
func TestCreateStoresUnassignedAsNull(t *testing.T) {
	repo, db := newTestRepository(t)

	created := mustCreate(t, repo, "unassigned")
	if created.Assignee != "" {
		t.Fatalf("Assignee = %q, want empty", created.Assignee)
	}

	var assignee sql.NullString
	if err := db.QueryRowContext(context.Background(),
		`SELECT assignee FROM tasks WHERE id = $1::uuid`, created.ID,
	).Scan(&assignee); err != nil {
		t.Fatalf("read stored assignee: %v", err)
	}
	if assignee.Valid {
		t.Fatalf("stored assignee = %q, want NULL", assignee.String)
	}
}

func TestUpdateChangesAssigneeAndCanClearIt(t *testing.T) {
	repo, _ := newTestRepository(t)
	created := mustCreateWith(t, repo, domain.Task{
		Title: "task", Status: domain.StatusTodo, Assignee: "alice",
	})

	updated, err := repo.Update(context.Background(), domain.Task{
		ID: created.ID, Title: "task", Status: domain.StatusTodo, Assignee: "bob",
	})
	if err != nil {
		t.Fatalf("Update() unexpected error: %v", err)
	}
	if updated.Assignee != "bob" {
		t.Fatalf("Assignee = %q, want bob", updated.Assignee)
	}

	cleared, err := repo.Update(context.Background(), domain.Task{
		ID: created.ID, Title: "task", Status: domain.StatusTodo,
	})
	if err != nil {
		t.Fatalf("Update() clearing assignee: %v", err)
	}
	if cleared.Assignee != "" {
		t.Fatalf("Assignee = %q, want it cleared", cleared.Assignee)
	}
}

func TestListFiltersByStatus(t *testing.T) {
	repo, _ := newTestRepository(t)
	todo := mustCreateWith(t, repo, domain.Task{Title: "todo", Status: domain.StatusTodo})
	done := mustCreateWith(t, repo, domain.Task{Title: "done", Status: domain.StatusDone})
	mustCreateWith(t, repo, domain.Task{Title: "busy", Status: domain.StatusInProgress})

	for _, status := range []domain.Status{domain.StatusTodo, domain.StatusDone, domain.StatusInProgress} {
		page, err := repo.ListPage(context.Background(), service.Filter{Status: status, Limit: 100})
		if err != nil {
			t.Fatalf("ListPage(status=%s) unexpected error: %v", status, err)
		}
		if len(page.Tasks) != 1 {
			t.Fatalf("status %s: page = %+v, want exactly one task", status, page)
		}
		if page.Tasks[0].Status != status {
			t.Fatalf("status %s: returned task status %q", status, page.Tasks[0].Status)
		}
	}

	page, err := repo.ListPage(context.Background(), service.Filter{Status: domain.StatusDone, Limit: 100})
	if err != nil {
		t.Fatalf("ListPage(done) unexpected error: %v", err)
	}
	if page.Tasks[0].ID != done.ID || done.ID == todo.ID {
		t.Fatalf("done filter returned %+v, want %s", page.Tasks, done.ID)
	}
}

func TestListFiltersByAssignee(t *testing.T) {
	repo, _ := newTestRepository(t)
	alice := mustCreateWith(t, repo, domain.Task{Title: "alice task", Status: domain.StatusTodo, Assignee: "alice"})
	mustCreateWith(t, repo, domain.Task{Title: "bob task", Status: domain.StatusTodo, Assignee: "bob"})
	mustCreate(t, repo, "unassigned")

	page, err := repo.ListPage(context.Background(), service.Filter{Assignee: "alice", Limit: 100})
	if err != nil {
		t.Fatalf("ListPage() unexpected error: %v", err)
	}
	if len(page.Tasks) != 1 || page.Tasks[0].ID != alice.ID {
		t.Fatalf("page = %+v, want only alice's task", page)
	}
}

func TestListCombinesFilters(t *testing.T) {
	repo, _ := newTestRepository(t)
	mustCreateWith(t, repo, domain.Task{Title: "alice todo", Status: domain.StatusTodo, Assignee: "alice"})
	want := mustCreateWith(t, repo, domain.Task{Title: "alice done", Status: domain.StatusDone, Assignee: "alice"})
	mustCreateWith(t, repo, domain.Task{Title: "bob done", Status: domain.StatusDone, Assignee: "bob"})

	page, err := repo.ListPage(context.Background(), service.Filter{
		Status: domain.StatusDone, Assignee: "alice", Limit: 100,
	})
	if err != nil {
		t.Fatalf("ListPage() unexpected error: %v", err)
	}
	if len(page.Tasks) != 1 || page.Tasks[0].ID != want.ID {
		t.Fatalf("page = %+v, want only alice's done task", page)
	}
}

// TestListPaginatesDeterministically proves keyset pages are ordered by id and
// do not overlap for a fixed data set.
func TestListPaginatesDeterministically(t *testing.T) {
	repo, _ := newTestRepository(t)
	for _, title := range []string{"one", "two", "three", "four", "five"} {
		mustCreate(t, repo, title)
	}

	seen := make(map[string]struct{}, 5)
	var allIDs []string
	afterID := ""
	for pageNumber, wantSize := range []int{2, 2, 1} {
		page, err := repo.ListPage(context.Background(), service.Filter{
			Limit: 2, AfterID: afterID,
		})
		if err != nil {
			t.Fatalf("ListPage(page %d) unexpected error: %v", pageNumber+1, err)
		}
		if len(page.Tasks) != wantSize {
			t.Fatalf("page %d has %d tasks, want %d", pageNumber+1, len(page.Tasks), wantSize)
		}
		for _, task := range page.Tasks {
			if _, duplicate := seen[task.ID]; duplicate {
				t.Fatalf("task %s appeared on two pages", task.ID)
			}
			seen[task.ID] = struct{}{}
			allIDs = append(allIDs, task.ID)
		}
		if len(page.Tasks) > 0 {
			afterID = page.Tasks[len(page.Tasks)-1].ID
		}
	}

	if len(seen) != 5 {
		t.Fatalf("pages covered %d tasks, want 5", len(seen))
	}
	if !sort.StringsAreSorted(allIDs) {
		t.Fatalf("IDs = %v, want ascending identifier order across pages", allIDs)
	}

	empty, err := repo.ListPage(context.Background(), service.Filter{Limit: 2, AfterID: afterID})
	if err != nil {
		t.Fatalf("ListPage(after last) unexpected error: %v", err)
	}
	if empty.Tasks == nil || len(empty.Tasks) != 0 {
		t.Fatalf("after last = %+v, want a non-nil empty page", empty)
	}
}

// TestListCursorAvoidsOffsetShift shows why keyset pagination replaces offsets:
// deleting a prior-page row between requests would make OFFSET skip a live row,
// while continuing after the last seen id still returns that row.
func TestListCursorAvoidsOffsetShift(t *testing.T) {
	repo, db := newTestRepository(t)

	ids := []string{
		"10000000-0000-4000-8000-000000000001",
		"10000000-0000-4000-8000-000000000002",
		"10000000-0000-4000-8000-000000000003",
		"10000000-0000-4000-8000-000000000004",
		"10000000-0000-4000-8000-000000000005",
	}
	for i, id := range ids {
		if _, err := db.ExecContext(context.Background(),
			`INSERT INTO tasks (id, title, description, status, assignee) VALUES ($1::uuid, $2, '', 'todo', NULL)`,
			id, fmt.Sprintf("task-%d", i+1),
		); err != nil {
			t.Fatalf("insert %s: %v", id, err)
		}
	}

	first, err := repo.ListPage(context.Background(), service.Filter{Limit: 2})
	if err != nil {
		t.Fatalf("ListPage(first): %v", err)
	}
	if len(first.Tasks) != 2 || first.Tasks[0].ID != ids[0] || first.Tasks[1].ID != ids[1] {
		t.Fatalf("first = %+v, want the first two ids", first.Tasks)
	}

	if _, err := db.ExecContext(context.Background(), `DELETE FROM tasks WHERE id = $1::uuid`, ids[0]); err != nil {
		t.Fatalf("delete first row: %v", err)
	}

	cursorPage, err := repo.ListPage(context.Background(), service.Filter{Limit: 2, AfterID: ids[1]})
	if err != nil {
		t.Fatalf("ListPage(cursor): %v", err)
	}
	if len(cursorPage.Tasks) != 2 || cursorPage.Tasks[0].ID != ids[2] || cursorPage.Tasks[1].ID != ids[3] {
		t.Fatalf("cursor page = %+v, want ids[2] and ids[3] (no skip)", cursorPage.Tasks)
	}

	var offsetIDs []string
	rows, err := db.QueryContext(context.Background(),
		`SELECT id::text FROM tasks ORDER BY id LIMIT 2 OFFSET 2`)
	if err != nil {
		t.Fatalf("offset query: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatalf("scan offset id: %v", err)
		}
		offsetIDs = append(offsetIDs, id)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("offset rows: %v", err)
	}
	if len(offsetIDs) != 2 || offsetIDs[0] != ids[3] {
		t.Fatalf("offset page = %v, want the classic skip starting at ids[3]", offsetIDs)
	}
	if offsetIDs[0] == cursorPage.Tasks[0].ID {
		t.Fatal("offset and cursor pages unexpectedly agreed after a prior-page delete")
	}
}

func TestListInsertBetweenPagesIsVisibleOnLaterPage(t *testing.T) {
	repo, db := newTestRepository(t)
	ids := []string{
		"20000000-0000-4000-8000-000000000001",
		"20000000-0000-4000-8000-000000000003",
	}
	for _, id := range ids {
		if _, err := db.ExecContext(context.Background(),
			`INSERT INTO tasks (id, title, description, status, assignee) VALUES ($1::uuid, $2, '', 'todo', NULL)`,
			id, id,
		); err != nil {
			t.Fatalf("insert %s: %v", id, err)
		}
	}

	first, err := repo.ListPage(context.Background(), service.Filter{Limit: 1})
	if err != nil {
		t.Fatalf("ListPage(first): %v", err)
	}
	if len(first.Tasks) != 1 || first.Tasks[0].ID != ids[0] {
		t.Fatalf("first = %+v, want %s", first.Tasks, ids[0])
	}

	inserted := "20000000-0000-4000-8000-000000000002"
	if _, err := db.ExecContext(context.Background(),
		`INSERT INTO tasks (id, title, description, status, assignee) VALUES ($1::uuid, 'inserted', '', 'todo', NULL)`,
		inserted,
	); err != nil {
		t.Fatalf("insert between: %v", err)
	}

	second, err := repo.ListPage(context.Background(), service.Filter{Limit: 10, AfterID: first.Tasks[0].ID})
	if err != nil {
		t.Fatalf("ListPage(second): %v", err)
	}
	got := make([]string, 0, len(second.Tasks))
	for _, task := range second.Tasks {
		got = append(got, task.ID)
	}
	if len(got) != 2 || got[0] != inserted || got[1] != ids[1] {
		t.Fatalf("second = %v, want inserted id then the original second row", got)
	}
}

func TestListUpdateLeavingFilterIsNotReturned(t *testing.T) {
	repo, _ := newTestRepository(t)
	keep := mustCreateWith(t, repo, domain.Task{Title: "keep", Status: domain.StatusDone, Assignee: "alice"})
	leave := mustCreateWith(t, repo, domain.Task{Title: "leave", Status: domain.StatusDone, Assignee: "alice"})

	page, err := repo.ListPage(context.Background(), service.Filter{
		Status: domain.StatusDone, Assignee: "alice", Limit: 1,
	})
	if err != nil {
		t.Fatalf("ListPage(first): %v", err)
	}
	if len(page.Tasks) != 1 {
		t.Fatalf("first page = %+v, want one task", page.Tasks)
	}
	afterID := page.Tasks[0].ID

	// Move the other matching row out of the filter between pages.
	other := leave
	if afterID == leave.ID {
		other = keep
	}
	other.Status = domain.StatusTodo
	if _, err := repo.Update(context.Background(), other); err != nil {
		t.Fatalf("Update(): %v", err)
	}

	rest, err := repo.ListPage(context.Background(), service.Filter{
		Status: domain.StatusDone, Assignee: "alice", Limit: 10, AfterID: afterID,
	})
	if err != nil {
		t.Fatalf("ListPage(rest): %v", err)
	}
	if len(rest.Tasks) != 0 {
		t.Fatalf("rest = %+v, want empty after the remaining row left the filter", rest.Tasks)
	}
}

// TestListFilterValuesAreBound proves the filter value is a SQL parameter, not
// concatenated text: an injection-shaped assignee matches nothing.
func TestListFilterValuesAreBound(t *testing.T) {
	repo, _ := newTestRepository(t)
	mustCreateWith(t, repo, domain.Task{Title: "alice", Status: domain.StatusTodo, Assignee: "alice"})

	page, err := repo.ListPage(context.Background(), service.Filter{
		Assignee: "alice' OR '1'='1", Limit: 100,
	})
	if err != nil {
		t.Fatalf("ListPage() unexpected error: %v", err)
	}
	if len(page.Tasks) != 0 {
		t.Fatalf("page = %+v, want no matches for a literal assignee", page)
	}
}
