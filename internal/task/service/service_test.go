package service

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/aarashgoodarzi/task-manager/internal/task/domain"
)

// fakeRepo is a hand-written stub that keeps tasks in memory. It records calls
// so tests can assert that validation failures never reach the repository.
type fakeRepo struct {
	tasks map[string]domain.Task
	seq   int

	createErr error
	listErr   error
	getErr    error
	updateErr error
	deleteErr error

	createCalls int
	listCalls   int
	updateCalls int
}

func newFakeRepo() *fakeRepo {
	return &fakeRepo{tasks: make(map[string]domain.Task)}
}

// newService returns a service without a cache; cache behavior has dedicated
// tests below.
func newService(repo Repository) *Service {
	return New(repo, nil, nil)
}

func (f *fakeRepo) Create(_ context.Context, task domain.Task) (domain.Task, error) {
	f.createCalls++
	if f.createErr != nil {
		return domain.Task{}, f.createErr
	}
	f.seq++
	task.ID = fmt.Sprintf("id-%d", f.seq)
	f.tasks[task.ID] = task
	return task, nil
}

func (f *fakeRepo) List(_ context.Context) ([]domain.Task, error) {
	f.listCalls++
	if f.listErr != nil {
		return nil, f.listErr
	}
	tasks := make([]domain.Task, 0, len(f.tasks))
	for _, task := range f.tasks {
		tasks = append(tasks, task)
	}
	return tasks, nil
}

func (f *fakeRepo) Get(_ context.Context, id string) (domain.Task, error) {
	if f.getErr != nil {
		return domain.Task{}, f.getErr
	}
	task, ok := f.tasks[id]
	if !ok {
		return domain.Task{}, ErrNotFound
	}
	return task, nil
}

func (f *fakeRepo) Update(_ context.Context, task domain.Task) (domain.Task, error) {
	f.updateCalls++
	if f.updateErr != nil {
		return domain.Task{}, f.updateErr
	}
	if _, ok := f.tasks[task.ID]; !ok {
		return domain.Task{}, ErrNotFound
	}
	f.tasks[task.ID] = task
	return task, nil
}

func (f *fakeRepo) Delete(_ context.Context, id string) error {
	if f.deleteErr != nil {
		return f.deleteErr
	}
	if _, ok := f.tasks[id]; !ok {
		return ErrNotFound
	}
	delete(f.tasks, id)
	return nil
}

func TestCreateAppliesDefaultsAndTrimsTitle(t *testing.T) {
	repo := newFakeRepo()
	svc := newService(repo)

	task, err := svc.Create(context.Background(), Input{
		Title:       "  write documentation  ",
		Description: "keep the spaces",
	})
	if err != nil {
		t.Fatalf("Create() unexpected error: %v", err)
	}

	if task.Title != "write documentation" {
		t.Fatalf("Title = %q, want trimmed title", task.Title)
	}
	if task.Status != domain.StatusTodo {
		t.Fatalf("Status = %q, want %q", task.Status, domain.StatusTodo)
	}
	if task.Description != "keep the spaces" {
		t.Fatalf("Description = %q, want it preserved", task.Description)
	}
	if task.ID == "" {
		t.Fatal("ID is empty, want repository-assigned identifier")
	}
	if _, ok := repo.tasks[task.ID]; !ok {
		t.Fatal("created task was not stored")
	}
}

func TestCreateAcceptsExplicitStatus(t *testing.T) {
	svc := newService(newFakeRepo())

	task, err := svc.Create(context.Background(), Input{Title: "ship it", Status: "done"})
	if err != nil {
		t.Fatalf("Create() unexpected error: %v", err)
	}
	if task.Status != domain.StatusDone {
		t.Fatalf("Status = %q, want %q", task.Status, domain.StatusDone)
	}
}

func TestCreateRejectsInvalidInputBeforeRepository(t *testing.T) {
	tests := []struct {
		name  string
		input Input
		field string
	}{
		{name: "empty title", input: Input{Title: ""}, field: "title"},
		{name: "whitespace title", input: Input{Title: "   "}, field: "title"},
		{name: "unknown status", input: Input{Title: "ok", Status: "pending"}, field: "status"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			repo := newFakeRepo()
			_, err := newService(repo).Create(context.Background(), tc.input)

			var verr *domain.ValidationError
			if !errors.As(err, &verr) {
				t.Fatalf("Create() error = %v, want *domain.ValidationError", err)
			}
			if verr.Field != tc.field {
				t.Fatalf("error field = %q, want %q", verr.Field, tc.field)
			}
			if repo.createCalls != 0 {
				t.Fatalf("repository Create called %d times, want 0", repo.createCalls)
			}
		})
	}
}

func TestCreatePropagatesRepositoryError(t *testing.T) {
	repo := newFakeRepo()
	repo.createErr = errors.New("storage exploded")

	_, err := newService(repo).Create(context.Background(), Input{Title: "task"})
	if !errors.Is(err, repo.createErr) {
		t.Fatalf("Create() error = %v, want wrapped repository error", err)
	}
}

func TestListEmpty(t *testing.T) {
	tasks, err := newService(newFakeRepo()).List(context.Background())
	if err != nil {
		t.Fatalf("List() unexpected error: %v", err)
	}
	if len(tasks) != 0 {
		t.Fatalf("len(tasks) = %d, want 0", len(tasks))
	}
}

func TestListPopulated(t *testing.T) {
	repo := newFakeRepo()
	repo.tasks["id-1"] = domain.Task{ID: "id-1", Title: "first", Status: domain.StatusTodo}
	repo.tasks["id-2"] = domain.Task{ID: "id-2", Title: "second", Status: domain.StatusDone}

	tasks, err := newService(repo).List(context.Background())
	if err != nil {
		t.Fatalf("List() unexpected error: %v", err)
	}
	if len(tasks) != 2 {
		t.Fatalf("len(tasks) = %d, want 2", len(tasks))
	}
}

func TestListPropagatesRepositoryError(t *testing.T) {
	repo := newFakeRepo()
	repo.listErr = errors.New("storage exploded")

	_, err := newService(repo).List(context.Background())
	if !errors.Is(err, repo.listErr) {
		t.Fatalf("List() error = %v, want wrapped repository error", err)
	}
}

func TestGetExisting(t *testing.T) {
	repo := newFakeRepo()
	repo.tasks["id-1"] = domain.Task{ID: "id-1", Title: "stored", Status: domain.StatusInProgress}

	task, err := newService(repo).Get(context.Background(), "id-1")
	if err != nil {
		t.Fatalf("Get() unexpected error: %v", err)
	}
	if task.Title != "stored" {
		t.Fatalf("Title = %q, want %q", task.Title, "stored")
	}
}

func TestGetMissing(t *testing.T) {
	_, err := newService(newFakeRepo()).Get(context.Background(), "id-missing")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get() error = %v, want ErrNotFound", err)
	}
}

func TestGetPropagatesRepositoryError(t *testing.T) {
	repo := newFakeRepo()
	repo.getErr = errors.New("storage exploded")

	_, err := newService(repo).Get(context.Background(), "id-1")
	if !errors.Is(err, repo.getErr) {
		t.Fatalf("Get() error = %v, want wrapped repository error", err)
	}
}

func TestUpdateReplacesExistingTask(t *testing.T) {
	repo := newFakeRepo()
	repo.tasks["id-1"] = domain.Task{ID: "id-1", Title: "old", Description: "old", Status: domain.StatusTodo}

	updated, err := newService(repo).Update(context.Background(), "id-1", Input{
		Title:       "  new title ",
		Description: "new description",
		Status:      "in_progress",
	})
	if err != nil {
		t.Fatalf("Update() unexpected error: %v", err)
	}

	if updated.ID != "id-1" || updated.Title != "new title" ||
		updated.Description != "new description" || updated.Status != domain.StatusInProgress {
		t.Fatalf("updated task = %+v, want fully replaced fields", updated)
	}
	if repo.tasks["id-1"] != updated {
		t.Fatalf("stored task = %+v, want %+v", repo.tasks["id-1"], updated)
	}
}

func TestUpdateWithoutStatusAppliesDefault(t *testing.T) {
	repo := newFakeRepo()
	repo.tasks["id-1"] = domain.Task{ID: "id-1", Title: "old", Status: domain.StatusDone}

	updated, err := newService(repo).Update(context.Background(), "id-1", Input{Title: "new"})
	if err != nil {
		t.Fatalf("Update() unexpected error: %v", err)
	}
	if updated.Status != domain.StatusTodo {
		t.Fatalf("Status = %q, want default %q", updated.Status, domain.StatusTodo)
	}
}

func TestUpdateMissing(t *testing.T) {
	_, err := newService(newFakeRepo()).Update(context.Background(), "id-missing", Input{Title: "task"})
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("Update() error = %v, want ErrNotFound", err)
	}
}

func TestUpdateRejectsInvalidInputBeforeRepository(t *testing.T) {
	repo := newFakeRepo()
	repo.tasks["id-1"] = domain.Task{ID: "id-1", Title: "old", Status: domain.StatusTodo}

	_, err := newService(repo).Update(context.Background(), "id-1", Input{Title: " ", Status: "pending"})

	var verr *domain.ValidationError
	if !errors.As(err, &verr) {
		t.Fatalf("Update() error = %v, want *domain.ValidationError", err)
	}
	if repo.updateCalls != 0 {
		t.Fatalf("repository Update called %d times, want 0", repo.updateCalls)
	}
}

func TestUpdatePropagatesRepositoryError(t *testing.T) {
	repo := newFakeRepo()
	repo.tasks["id-1"] = domain.Task{ID: "id-1", Title: "old", Status: domain.StatusTodo}
	repo.updateErr = errors.New("storage exploded")

	_, err := newService(repo).Update(context.Background(), "id-1", Input{Title: "new"})
	if !errors.Is(err, repo.updateErr) {
		t.Fatalf("Update() error = %v, want wrapped repository error", err)
	}
}

func TestDeleteExisting(t *testing.T) {
	repo := newFakeRepo()
	repo.tasks["id-1"] = domain.Task{ID: "id-1", Title: "task", Status: domain.StatusTodo}

	if err := newService(repo).Delete(context.Background(), "id-1"); err != nil {
		t.Fatalf("Delete() unexpected error: %v", err)
	}
	if _, ok := repo.tasks["id-1"]; ok {
		t.Fatal("task still stored after Delete")
	}
}

func TestDeleteMissing(t *testing.T) {
	err := newService(newFakeRepo()).Delete(context.Background(), "id-missing")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("Delete() error = %v, want ErrNotFound", err)
	}
}

func TestDeletePropagatesRepositoryError(t *testing.T) {
	repo := newFakeRepo()
	repo.deleteErr = errors.New("storage exploded")

	err := newService(repo).Delete(context.Background(), "id-1")
	if !errors.Is(err, repo.deleteErr) {
		t.Fatalf("Delete() error = %v, want wrapped repository error", err)
	}
}

// fakeCache is a deterministic cache double. It records calls and the last
// snapshot so tests can assert both the decision and the data written back.
type fakeCache struct {
	snapshot ListSnapshot

	getErr        error
	setErr        error
	invalidateErr error

	getCalls        int
	setCalls        int
	invalidateCalls int

	lastSnapshot     ListSnapshot
	invalidateCtxErr error
}

func (f *fakeCache) Get(context.Context) (ListSnapshot, error) {
	f.getCalls++
	if f.getErr != nil {
		return ListSnapshot{}, f.getErr
	}
	return f.snapshot, nil
}

func (f *fakeCache) Set(_ context.Context, snapshot ListSnapshot) error {
	f.setCalls++
	f.lastSnapshot = snapshot
	return f.setErr
}

func (f *fakeCache) Invalidate(ctx context.Context) error {
	f.invalidateCalls++
	f.invalidateCtxErr = ctx.Err()
	return f.invalidateErr
}

func TestListCacheHitReturnsCachedTasksWithoutRepository(t *testing.T) {
	repo := newFakeRepo()
	repo.tasks["db-task"] = domain.Task{ID: "db-task", Title: "from database", Status: domain.StatusTodo}

	cached := []domain.Task{{ID: "cached-task", Title: "from cache", Status: domain.StatusDone}}
	cache := &fakeCache{snapshot: ListSnapshot{Tasks: cached, Token: "gen-1", Hit: true}}

	tasks, err := New(repo, cache, nil).List(context.Background())
	if err != nil {
		t.Fatalf("List() unexpected error: %v", err)
	}
	if len(tasks) != 1 || tasks[0].ID != "cached-task" {
		t.Fatalf("tasks = %+v, want the cached list", tasks)
	}
	if repo.listCalls != 0 {
		t.Fatalf("repository List called %d times, want 0 on a cache hit", repo.listCalls)
	}
	if cache.setCalls != 0 {
		t.Fatalf("cache Set called %d times, want 0 on a hit", cache.setCalls)
	}
}

func TestListCacheHitEmptyListIsAValidHit(t *testing.T) {
	repo := newFakeRepo()
	repo.tasks["db-task"] = domain.Task{ID: "db-task", Title: "not empty", Status: domain.StatusTodo}

	cache := &fakeCache{snapshot: ListSnapshot{Tasks: []domain.Task{}, Token: "gen-1", Hit: true}}

	tasks, err := New(repo, cache, nil).List(context.Background())
	if err != nil {
		t.Fatalf("List() unexpected error: %v", err)
	}
	if tasks == nil || len(tasks) != 0 {
		t.Fatalf("tasks = %#v, want a non-nil empty list", tasks)
	}
	if repo.listCalls != 0 {
		t.Fatalf("repository List called %d times, want 0 for an empty cached list", repo.listCalls)
	}
}

func TestListCacheMissReadsRepositoryAndStoresResult(t *testing.T) {
	repo := newFakeRepo()
	repo.tasks["id-1"] = domain.Task{ID: "id-1", Title: "first", Status: domain.StatusTodo}
	repo.tasks["id-2"] = domain.Task{ID: "id-2", Title: "second", Status: domain.StatusDone}

	cache := &fakeCache{snapshot: ListSnapshot{Token: "gen-1"}}

	tasks, err := New(repo, cache, nil).List(context.Background())
	if err != nil {
		t.Fatalf("List() unexpected error: %v", err)
	}
	if len(tasks) != 2 {
		t.Fatalf("len(tasks) = %d, want 2", len(tasks))
	}
	if repo.listCalls != 1 {
		t.Fatalf("repository List called %d times, want 1", repo.listCalls)
	}
	if cache.setCalls != 1 {
		t.Fatalf("cache Set called %d times, want 1", cache.setCalls)
	}
	if cache.lastSnapshot.Token != "gen-1" {
		t.Fatalf("stored token = %q, want the token returned by Get", cache.lastSnapshot.Token)
	}
	if len(cache.lastSnapshot.Tasks) != 2 {
		t.Fatalf("stored tasks = %+v, want the database list", cache.lastSnapshot.Tasks)
	}
}

func TestListCacheReadFailureFallsBackToRepository(t *testing.T) {
	repo := newFakeRepo()
	repo.tasks["id-1"] = domain.Task{ID: "id-1", Title: "stored", Status: domain.StatusTodo}

	cache := &fakeCache{getErr: errors.New("redis unavailable")}

	tasks, err := New(repo, cache, nil).List(context.Background())
	if err != nil {
		t.Fatalf("List() unexpected error: %v", err)
	}
	if len(tasks) != 1 || tasks[0].ID != "id-1" {
		t.Fatalf("tasks = %+v, want the database list", tasks)
	}
	if repo.listCalls != 1 {
		t.Fatalf("repository List called %d times, want the fallback query", repo.listCalls)
	}
	if cache.setCalls != 0 {
		t.Fatalf("cache Set called %d times after a read failure, want 0", cache.setCalls)
	}
}

func TestListCacheWriteFailureStillReturnsDatabaseResult(t *testing.T) {
	repo := newFakeRepo()
	repo.tasks["id-1"] = domain.Task{ID: "id-1", Title: "stored", Status: domain.StatusTodo}

	cache := &fakeCache{
		snapshot: ListSnapshot{Token: "gen-1"},
		setErr:   errors.New("redis unavailable"),
	}

	tasks, err := New(repo, cache, nil).List(context.Background())
	if err != nil {
		t.Fatalf("List() unexpected error: %v", err)
	}
	if len(tasks) != 1 || tasks[0].ID != "id-1" {
		t.Fatalf("tasks = %+v, want the database list", tasks)
	}
	if cache.setCalls != 1 {
		t.Fatalf("cache Set called %d times, want 1", cache.setCalls)
	}
}

func TestListRepositoryFailureIsNotCached(t *testing.T) {
	repo := newFakeRepo()
	repo.listErr = errors.New("storage exploded")

	cache := &fakeCache{snapshot: ListSnapshot{Token: "gen-1"}}

	_, err := New(repo, cache, nil).List(context.Background())
	if !errors.Is(err, repo.listErr) {
		t.Fatalf("List() error = %v, want wrapped repository error", err)
	}
	if cache.setCalls != 0 {
		t.Fatalf("cache Set called %d times after a repository error, want 0", cache.setCalls)
	}
}

func TestListMalformedCacheFallsBackToRepository(t *testing.T) {
	repo := newFakeRepo()
	repo.tasks["id-1"] = domain.Task{ID: "id-1", Title: "stored", Status: domain.StatusTodo}

	cache := &fakeCache{getErr: errors.New("decode cached list: invalid character")}

	tasks, err := New(repo, cache, nil).List(context.Background())
	if err != nil {
		t.Fatalf("List() unexpected error: %v", err)
	}
	if len(tasks) != 1 || tasks[0].ID != "id-1" {
		t.Fatalf("tasks = %+v, want the database list", tasks)
	}
	if repo.listCalls != 1 {
		t.Fatalf("repository List called %d times, want the fallback query", repo.listCalls)
	}
}

func TestCacheDisabledKeepsListWorking(t *testing.T) {
	repo := newFakeRepo()
	repo.tasks["id-1"] = domain.Task{ID: "id-1", Title: "stored", Status: domain.StatusTodo}

	tasks, err := New(repo, nil, nil).List(context.Background())
	if err != nil {
		t.Fatalf("List() unexpected error: %v", err)
	}
	if len(tasks) != 1 {
		t.Fatalf("len(tasks) = %d, want 1", len(tasks))
	}
}

func TestCreateInvalidatesListCache(t *testing.T) {
	repo := newFakeRepo()
	cache := &fakeCache{}

	if _, err := New(repo, cache, nil).Create(context.Background(), Input{Title: "new"}); err != nil {
		t.Fatalf("Create() unexpected error: %v", err)
	}
	if cache.invalidateCalls != 1 {
		t.Fatalf("cache Invalidate called %d times, want 1 after create", cache.invalidateCalls)
	}
}

func TestUpdateInvalidatesListCache(t *testing.T) {
	repo := newFakeRepo()
	repo.tasks["id-1"] = domain.Task{ID: "id-1", Title: "old", Status: domain.StatusTodo}
	cache := &fakeCache{}

	if _, err := New(repo, cache, nil).Update(context.Background(), "id-1", Input{Title: "new"}); err != nil {
		t.Fatalf("Update() unexpected error: %v", err)
	}
	if cache.invalidateCalls != 1 {
		t.Fatalf("cache Invalidate called %d times, want 1 after update", cache.invalidateCalls)
	}
}

func TestDeleteInvalidatesListCache(t *testing.T) {
	repo := newFakeRepo()
	repo.tasks["id-1"] = domain.Task{ID: "id-1", Title: "task", Status: domain.StatusTodo}
	cache := &fakeCache{}

	if err := New(repo, cache, nil).Delete(context.Background(), "id-1"); err != nil {
		t.Fatalf("Delete() unexpected error: %v", err)
	}
	if cache.invalidateCalls != 1 {
		t.Fatalf("cache Invalidate called %d times, want 1 after delete", cache.invalidateCalls)
	}
}

func TestFailedCreateDoesNotInvalidateListCache(t *testing.T) {
	repo := newFakeRepo()
	repo.createErr = errors.New("storage exploded")
	cache := &fakeCache{}

	if _, err := New(repo, cache, nil).Create(context.Background(), Input{Title: "task"}); err == nil {
		t.Fatal("Create() error = nil, want repository error")
	}
	if cache.invalidateCalls != 0 {
		t.Fatalf("cache Invalidate called %d times, want 0 after a failed create", cache.invalidateCalls)
	}
}

func TestFailedUpdateDoesNotInvalidateListCache(t *testing.T) {
	repo := newFakeRepo()
	repo.tasks["id-1"] = domain.Task{ID: "id-1", Title: "old", Status: domain.StatusTodo}
	repo.updateErr = errors.New("storage exploded")
	cache := &fakeCache{}

	if _, err := New(repo, cache, nil).Update(context.Background(), "id-1", Input{Title: "new"}); err == nil {
		t.Fatal("Update() error = nil, want repository error")
	}
	if cache.invalidateCalls != 0 {
		t.Fatalf("cache Invalidate called %d times, want 0 after a failed update", cache.invalidateCalls)
	}
}

func TestFailedDeleteDoesNotInvalidateListCache(t *testing.T) {
	repo := newFakeRepo()
	repo.deleteErr = errors.New("storage exploded")
	cache := &fakeCache{}

	if err := New(repo, cache, nil).Delete(context.Background(), "id-1"); err == nil {
		t.Fatal("Delete() error = nil, want repository error")
	}
	if cache.invalidateCalls != 0 {
		t.Fatalf("cache Invalidate called %d times, want 0 after a failed delete", cache.invalidateCalls)
	}
}

func TestInvalidationFailureDoesNotFailCreate(t *testing.T) {
	repo := newFakeRepo()
	cache := &fakeCache{invalidateErr: errors.New("redis unavailable")}

	created, err := New(repo, cache, nil).Create(context.Background(), Input{Title: "new"})
	if err != nil {
		t.Fatalf("Create() error = %v, want success despite cache failure", err)
	}
	if created.ID == "" {
		t.Fatal("created task has no id")
	}
	if cache.invalidateCalls != 1 {
		t.Fatalf("cache Invalidate called %d times, want 1", cache.invalidateCalls)
	}
}

func TestInvalidationFailureDoesNotFailUpdate(t *testing.T) {
	repo := newFakeRepo()
	repo.tasks["id-1"] = domain.Task{ID: "id-1", Title: "old", Status: domain.StatusTodo}
	cache := &fakeCache{invalidateErr: errors.New("redis unavailable")}

	if _, err := New(repo, cache, nil).Update(context.Background(), "id-1", Input{Title: "new"}); err != nil {
		t.Fatalf("Update() error = %v, want success despite cache failure", err)
	}
}

func TestInvalidationFailureDoesNotFailDelete(t *testing.T) {
	repo := newFakeRepo()
	repo.tasks["id-1"] = domain.Task{ID: "id-1", Title: "task", Status: domain.StatusTodo}
	cache := &fakeCache{invalidateErr: errors.New("redis unavailable")}

	if err := New(repo, cache, nil).Delete(context.Background(), "id-1"); err != nil {
		t.Fatalf("Delete() error = %v, want success despite cache failure", err)
	}
}

func TestInvalidationSurvivesCanceledRequestContext(t *testing.T) {
	repo := newFakeRepo()
	cache := &fakeCache{}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := New(repo, cache, nil).Create(ctx, Input{Title: "task"}); err != nil {
		t.Fatalf("Create() unexpected error: %v", err)
	}
	if cache.invalidateCalls != 1 {
		t.Fatalf("cache Invalidate called %d times, want 1", cache.invalidateCalls)
	}
	if cache.invalidateCtxErr != nil {
		t.Fatalf("invalidation context error = %v, want a context detached from request cancellation", cache.invalidateCtxErr)
	}
}
