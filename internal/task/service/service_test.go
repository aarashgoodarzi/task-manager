package service

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
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

	lastFilter Filter
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

func (f *fakeRepo) ListPage(_ context.Context, filter Filter) (TaskPage, error) {
	f.listCalls++
	f.lastFilter = filter
	if f.listErr != nil {
		return TaskPage{}, f.listErr
	}

	matched := make([]domain.Task, 0, len(f.tasks))
	for _, task := range f.tasks {
		if filter.Status != "" && task.Status != filter.Status {
			continue
		}
		if filter.Assignee != "" && task.Assignee != filter.Assignee {
			continue
		}
		if filter.AfterID != "" && task.ID <= filter.AfterID {
			continue
		}
		matched = append(matched, task)
	}
	sort.Slice(matched, func(i, j int) bool { return matched[i].ID < matched[j].ID })

	if filter.Limit >= 0 && len(matched) > filter.Limit {
		matched = matched[:filter.Limit]
	}
	return TaskPage{Tasks: matched}, nil
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
	result, err := newService(newFakeRepo()).List(context.Background(), ListQuery{})
	if err != nil {
		t.Fatalf("List() unexpected error: %v", err)
	}
	if len(result.Tasks) != 0 {
		t.Fatalf("len(tasks) = %d, want 0", len(result.Tasks))
	}
	if result.PageSize != DefaultPageSize || result.HasMore || result.NextCursor != "" {
		t.Fatalf("result = %+v, want the documented empty defaults", result)
	}
}

func TestListPopulated(t *testing.T) {
	repo := newFakeRepo()
	repo.tasks["id-1"] = domain.Task{ID: "id-1", Title: "first", Status: domain.StatusTodo}
	repo.tasks["id-2"] = domain.Task{ID: "id-2", Title: "second", Status: domain.StatusDone}

	result, err := newService(repo).List(context.Background(), ListQuery{})
	if err != nil {
		t.Fatalf("List() unexpected error: %v", err)
	}
	if len(result.Tasks) != 2 {
		t.Fatalf("len(tasks) = %d, want 2", len(result.Tasks))
	}
	if result.HasMore || result.NextCursor != "" {
		t.Fatalf("result = %+v, want a single complete page", result)
	}
}

func TestListPropagatesRepositoryError(t *testing.T) {
	repo := newFakeRepo()
	repo.listErr = errors.New("storage exploded")

	_, err := newService(repo).List(context.Background(), ListQuery{})
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
	lastGetQuery     ListQuery
	lastSetQuery     ListQuery
	invalidateCtxErr error
}

func (f *fakeCache) Get(_ context.Context, query ListQuery) (ListSnapshot, error) {
	f.getCalls++
	f.lastGetQuery = query
	if f.getErr != nil {
		return ListSnapshot{}, f.getErr
	}
	return f.snapshot, nil
}

func (f *fakeCache) Set(_ context.Context, query ListQuery, snapshot ListSnapshot) error {
	f.setCalls++
	f.lastSetQuery = query
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
	cache := &fakeCache{snapshot: ListSnapshot{Result: ListResult{Tasks: cached}, Token: "gen-1", Hit: true}}

	result, err := New(repo, cache, nil).List(context.Background(), ListQuery{})
	if err != nil {
		t.Fatalf("List() unexpected error: %v", err)
	}
	if len(result.Tasks) != 1 || result.Tasks[0].ID != "cached-task" {
		t.Fatalf("tasks = %+v, want the cached list", result.Tasks)
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

	cache := &fakeCache{snapshot: ListSnapshot{Result: ListResult{Tasks: []domain.Task{}}, Token: "gen-1", Hit: true}}

	result, err := New(repo, cache, nil).List(context.Background(), ListQuery{})
	if err != nil {
		t.Fatalf("List() unexpected error: %v", err)
	}
	if result.Tasks == nil || len(result.Tasks) != 0 {
		t.Fatalf("tasks = %#v, want a non-nil empty list", result.Tasks)
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

	result, err := New(repo, cache, nil).List(context.Background(), ListQuery{})
	if err != nil {
		t.Fatalf("List() unexpected error: %v", err)
	}
	if len(result.Tasks) != 2 {
		t.Fatalf("len(tasks) = %d, want 2", len(result.Tasks))
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
	if len(cache.lastSnapshot.Result.Tasks) != 2 {
		t.Fatalf("stored tasks = %+v, want the database list", cache.lastSnapshot.Result.Tasks)
	}
}

func TestListCacheReadFailureFallsBackToRepository(t *testing.T) {
	repo := newFakeRepo()
	repo.tasks["id-1"] = domain.Task{ID: "id-1", Title: "stored", Status: domain.StatusTodo}

	cache := &fakeCache{getErr: errors.New("redis unavailable")}

	result, err := New(repo, cache, nil).List(context.Background(), ListQuery{})
	if err != nil {
		t.Fatalf("List() unexpected error: %v", err)
	}
	if len(result.Tasks) != 1 || result.Tasks[0].ID != "id-1" {
		t.Fatalf("tasks = %+v, want the database list", result.Tasks)
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

	result, err := New(repo, cache, nil).List(context.Background(), ListQuery{})
	if err != nil {
		t.Fatalf("List() unexpected error: %v", err)
	}
	if len(result.Tasks) != 1 || result.Tasks[0].ID != "id-1" {
		t.Fatalf("tasks = %+v, want the database list", result.Tasks)
	}
	if cache.setCalls != 1 {
		t.Fatalf("cache Set called %d times, want 1", cache.setCalls)
	}
}

func TestListRepositoryFailureIsNotCached(t *testing.T) {
	repo := newFakeRepo()
	repo.listErr = errors.New("storage exploded")

	cache := &fakeCache{snapshot: ListSnapshot{Token: "gen-1"}}

	_, err := New(repo, cache, nil).List(context.Background(), ListQuery{})
	if !errors.Is(err, repo.listErr) {
		t.Fatalf("List() error = %v, want wrapped repository error", err)
	}
	if cache.setCalls != 0 {
		t.Fatalf("cache Set called %d times after a repository error, want 0", cache.setCalls)
	}
}

func TestListPoisonCacheMissRefillsFromRepository(t *testing.T) {
	// After the Redis adapter discards an invalid payload it returns a miss
	// carrying the observed generation token. The service must refill so the
	// next reader can hit again.
	repo := newFakeRepo()
	repo.tasks["id-1"] = domain.Task{ID: "id-1", Title: "stored", Status: domain.StatusTodo}

	cache := &fakeCache{snapshot: ListSnapshot{Token: "gen-poison"}}

	result, err := New(repo, cache, nil).List(context.Background(), ListQuery{})
	if err != nil {
		t.Fatalf("List() unexpected error: %v", err)
	}
	if len(result.Tasks) != 1 || result.Tasks[0].ID != "id-1" {
		t.Fatalf("tasks = %+v, want the database list", result.Tasks)
	}
	if cache.setCalls != 1 {
		t.Fatalf("cache Set called %d times, want a refill under the observed token", cache.setCalls)
	}
	if cache.lastSnapshot.Token != "gen-poison" {
		t.Fatalf("stored token = %q, want gen-poison", cache.lastSnapshot.Token)
	}
}

func TestCacheDisabledKeepsListWorking(t *testing.T) {
	repo := newFakeRepo()
	repo.tasks["id-1"] = domain.Task{ID: "id-1", Title: "stored", Status: domain.StatusTodo}

	result, err := New(repo, nil, nil).List(context.Background(), ListQuery{})
	if err != nil {
		t.Fatalf("List() unexpected error: %v", err)
	}
	if len(result.Tasks) != 1 {
		t.Fatalf("len(tasks) = %d, want 1", len(result.Tasks))
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

func TestListAppliesFiltersBeforePagination(t *testing.T) {
	repo := newFakeRepo()
	repo.tasks["00000000-0000-4000-8000-000000000001"] = domain.Task{
		ID: "00000000-0000-4000-8000-000000000001", Title: "todo", Status: domain.StatusTodo, Assignee: "alice",
	}
	repo.tasks["00000000-0000-4000-8000-000000000002"] = domain.Task{
		ID: "00000000-0000-4000-8000-000000000002", Title: "done", Status: domain.StatusDone, Assignee: "alice",
	}
	repo.tasks["00000000-0000-4000-8000-000000000003"] = domain.Task{
		ID: "00000000-0000-4000-8000-000000000003", Title: "other owner", Status: domain.StatusDone, Assignee: "bob",
	}

	result, err := newService(repo).List(context.Background(), ListQuery{
		Status: domain.StatusDone, Assignee: "alice", PageSize: 1,
	})
	if err != nil {
		t.Fatalf("List() unexpected error: %v", err)
	}

	// Limit is page_size+1 so HasMore can be detected without COUNT(*).
	if want := (Filter{Status: domain.StatusDone, Assignee: "alice", Limit: 2}); repo.lastFilter != want {
		t.Fatalf("repository filter = %+v, want %+v", repo.lastFilter, want)
	}
	if result.HasMore || len(result.Tasks) != 1 || result.Tasks[0].ID != "00000000-0000-4000-8000-000000000002" {
		t.Fatalf("result = %+v, want only the matching done task", result)
	}
}

func TestListSecondPageUsesCursor(t *testing.T) {
	repo := newFakeRepo()
	ids := []string{
		"00000000-0000-4000-8000-000000000001",
		"00000000-0000-4000-8000-000000000002",
		"00000000-0000-4000-8000-000000000003",
	}
	for _, id := range ids {
		repo.tasks[id] = domain.Task{ID: id, Title: id, Status: domain.StatusTodo}
	}

	first, err := newService(repo).List(context.Background(), ListQuery{PageSize: 2})
	if err != nil {
		t.Fatalf("List(first) unexpected error: %v", err)
	}
	if !first.HasMore || first.NextCursor == "" || len(first.Tasks) != 2 {
		t.Fatalf("first = %+v, want two tasks and a continuation cursor", first)
	}

	second, err := newService(repo).List(context.Background(), ListQuery{Cursor: first.NextCursor, PageSize: 2})
	if err != nil {
		t.Fatalf("List(second) unexpected error: %v", err)
	}
	if repo.lastFilter.AfterID != ids[1] || repo.lastFilter.Limit != 3 {
		t.Fatalf("filter = %+v, want after %s and limit 3", repo.lastFilter, ids[1])
	}
	if second.HasMore || len(second.Tasks) != 1 || second.Tasks[0].ID != ids[2] {
		t.Fatalf("second = %+v, want the last task and end of list", second)
	}
}

func TestListFinalPageHasNoCursor(t *testing.T) {
	repo := newFakeRepo()
	repo.tasks["00000000-0000-4000-8000-000000000001"] = domain.Task{
		ID: "00000000-0000-4000-8000-000000000001", Title: "only", Status: domain.StatusTodo,
	}

	result, err := newService(repo).List(context.Background(), ListQuery{PageSize: 2})
	if err != nil {
		t.Fatalf("List() unexpected error: %v", err)
	}
	if result.PageSize != 2 || result.HasMore || result.NextCursor != "" {
		t.Fatalf("result = %+v, want a final page with no cursor", result)
	}
	if len(result.Tasks) != 1 {
		t.Fatalf("tasks = %#v, want the single task", result.Tasks)
	}
}

func TestListHasMoreAcrossPages(t *testing.T) {
	repo := newFakeRepo()
	for i := 0; i < 21; i++ {
		id := fmt.Sprintf("00000000-0000-4000-8000-%012d", i)
		repo.tasks[id] = domain.Task{ID: id, Title: id, Status: domain.StatusTodo}
	}

	result, err := newService(repo).List(context.Background(), ListQuery{PageSize: 20})
	if err != nil {
		t.Fatalf("List() unexpected error: %v", err)
	}
	if !result.HasMore || result.NextCursor == "" || len(result.Tasks) != 20 {
		t.Fatalf("result = %+v, want a full page with has_more", result)
	}
}

func TestListNormalizesQueryForCache(t *testing.T) {
	repo := newFakeRepo()
	cache := &fakeCache{snapshot: ListSnapshot{Token: "gen-1"}}

	// Trimmed assignee and explicit defaults must reach the cache in their
	// canonical form, so equivalent requests share one entry.
	if _, err := New(repo, cache, nil).List(context.Background(), ListQuery{
		Assignee: "  alice  ", PageSize: DefaultPageSize,
	}); err != nil {
		t.Fatalf("List() unexpected error: %v", err)
	}

	want := ListQuery{Assignee: "alice", PageSize: DefaultPageSize}
	if cache.lastGetQuery != want {
		t.Fatalf("cache Get query = %+v, want %+v", cache.lastGetQuery, want)
	}
	if cache.lastSetQuery != want {
		t.Fatalf("cache Set query = %+v, want %+v", cache.lastSetQuery, want)
	}
}

func TestListCacheHitReturnsFullMetadata(t *testing.T) {
	repo := newFakeRepo()
	repo.tasks["db-task"] = domain.Task{ID: "db-task", Title: "from database", Status: domain.StatusTodo}

	cached := ListResult{
		Tasks:      []domain.Task{{ID: "cached", Title: "from cache", Status: domain.StatusDone}},
		PageSize:   5,
		NextCursor: "opaque-next",
		HasMore:    true,
	}
	cache := &fakeCache{snapshot: ListSnapshot{Result: cached, Token: "gen-1", Hit: true}}

	result, err := New(repo, cache, nil).List(context.Background(), ListQuery{PageSize: 5})
	if err != nil {
		t.Fatalf("List() unexpected error: %v", err)
	}
	if result.PageSize != 5 || !result.HasMore || result.NextCursor != "opaque-next" {
		t.Fatalf("result = %+v, want the cached pagination metadata", result)
	}
	if repo.listCalls != 0 {
		t.Fatalf("repository was called %d times on a cache hit, want 0", repo.listCalls)
	}
}

func TestListRejectsInvalidQuery(t *testing.T) {
	validCursor, err := EncodeCursor("6f1c2a4e-8d3b-4f5a-9c7e-1a2b3c4d5e6f", ListQuery{PageSize: DefaultPageSize})
	if err != nil {
		t.Fatalf("EncodeCursor() unexpected error: %v", err)
	}

	tests := []struct {
		name  string
		query ListQuery
		field string
	}{
		{name: "unknown status", query: ListQuery{Status: "pending"}, field: "status"},
		{name: "negative page size", query: ListQuery{PageSize: -1}, field: "page_size"},
		{name: "page size above maximum", query: ListQuery{PageSize: MaxPageSize + 1}, field: "page_size"},
		{name: "long assignee", query: ListQuery{Assignee: strings.Repeat("a", domain.MaxAssigneeLength+1)}, field: "assignee"},
		{name: "malformed cursor", query: ListQuery{Cursor: "not-a-cursor"}, field: "cursor"},
		{name: "incompatible cursor", query: ListQuery{Cursor: validCursor, PageSize: 10}, field: "cursor"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			repo := newFakeRepo()
			_, err := newService(repo).List(context.Background(), tc.query)

			var validation *domain.ValidationError
			if !errors.As(err, &validation) {
				t.Fatalf("List() error = %v, want *domain.ValidationError", err)
			}
			if validation.Field != tc.field {
				t.Fatalf("field = %q, want %q", validation.Field, tc.field)
			}
			if repo.listCalls != 0 {
				t.Fatalf("repository was queried %d times for an invalid query", repo.listCalls)
			}
		})
	}
}

func TestCreateTrimsAssigneeAndRejectsLongValues(t *testing.T) {
	repo := newFakeRepo()
	created, err := newService(repo).Create(context.Background(), Input{Title: "task", Assignee: "  alice  "})
	if err != nil {
		t.Fatalf("Create() unexpected error: %v", err)
	}
	if created.Assignee != "alice" {
		t.Fatalf("Assignee = %q, want the trimmed label", created.Assignee)
	}

	_, err = newService(repo).Create(context.Background(), Input{
		Title: "task", Assignee: strings.Repeat("a", domain.MaxAssigneeLength+1),
	})
	var validation *domain.ValidationError
	if !errors.As(err, &validation) || validation.Field != "assignee" {
		t.Fatalf("Create() error = %v, want an assignee validation error", err)
	}
}
