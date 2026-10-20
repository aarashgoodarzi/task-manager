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
	updateCalls int
}

func newFakeRepo() *fakeRepo {
	return &fakeRepo{tasks: make(map[string]domain.Task)}
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
	svc := New(repo)

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
	svc := New(newFakeRepo())

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
			_, err := New(repo).Create(context.Background(), tc.input)

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

	_, err := New(repo).Create(context.Background(), Input{Title: "task"})
	if !errors.Is(err, repo.createErr) {
		t.Fatalf("Create() error = %v, want wrapped repository error", err)
	}
}

func TestListEmpty(t *testing.T) {
	tasks, err := New(newFakeRepo()).List(context.Background())
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

	tasks, err := New(repo).List(context.Background())
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

	_, err := New(repo).List(context.Background())
	if !errors.Is(err, repo.listErr) {
		t.Fatalf("List() error = %v, want wrapped repository error", err)
	}
}

func TestGetExisting(t *testing.T) {
	repo := newFakeRepo()
	repo.tasks["id-1"] = domain.Task{ID: "id-1", Title: "stored", Status: domain.StatusInProgress}

	task, err := New(repo).Get(context.Background(), "id-1")
	if err != nil {
		t.Fatalf("Get() unexpected error: %v", err)
	}
	if task.Title != "stored" {
		t.Fatalf("Title = %q, want %q", task.Title, "stored")
	}
}

func TestGetMissing(t *testing.T) {
	_, err := New(newFakeRepo()).Get(context.Background(), "id-missing")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get() error = %v, want ErrNotFound", err)
	}
}

func TestGetPropagatesRepositoryError(t *testing.T) {
	repo := newFakeRepo()
	repo.getErr = errors.New("storage exploded")

	_, err := New(repo).Get(context.Background(), "id-1")
	if !errors.Is(err, repo.getErr) {
		t.Fatalf("Get() error = %v, want wrapped repository error", err)
	}
}

func TestUpdateReplacesExistingTask(t *testing.T) {
	repo := newFakeRepo()
	repo.tasks["id-1"] = domain.Task{ID: "id-1", Title: "old", Description: "old", Status: domain.StatusTodo}

	updated, err := New(repo).Update(context.Background(), "id-1", Input{
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

	updated, err := New(repo).Update(context.Background(), "id-1", Input{Title: "new"})
	if err != nil {
		t.Fatalf("Update() unexpected error: %v", err)
	}
	if updated.Status != domain.StatusTodo {
		t.Fatalf("Status = %q, want default %q", updated.Status, domain.StatusTodo)
	}
}

func TestUpdateMissing(t *testing.T) {
	_, err := New(newFakeRepo()).Update(context.Background(), "id-missing", Input{Title: "task"})
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("Update() error = %v, want ErrNotFound", err)
	}
}

func TestUpdateRejectsInvalidInputBeforeRepository(t *testing.T) {
	repo := newFakeRepo()
	repo.tasks["id-1"] = domain.Task{ID: "id-1", Title: "old", Status: domain.StatusTodo}

	_, err := New(repo).Update(context.Background(), "id-1", Input{Title: " ", Status: "pending"})

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

	_, err := New(repo).Update(context.Background(), "id-1", Input{Title: "new"})
	if !errors.Is(err, repo.updateErr) {
		t.Fatalf("Update() error = %v, want wrapped repository error", err)
	}
}

func TestDeleteExisting(t *testing.T) {
	repo := newFakeRepo()
	repo.tasks["id-1"] = domain.Task{ID: "id-1", Title: "task", Status: domain.StatusTodo}

	if err := New(repo).Delete(context.Background(), "id-1"); err != nil {
		t.Fatalf("Delete() unexpected error: %v", err)
	}
	if _, ok := repo.tasks["id-1"]; ok {
		t.Fatal("task still stored after Delete")
	}
}

func TestDeleteMissing(t *testing.T) {
	err := New(newFakeRepo()).Delete(context.Background(), "id-missing")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("Delete() error = %v, want ErrNotFound", err)
	}
}

func TestDeletePropagatesRepositoryError(t *testing.T) {
	repo := newFakeRepo()
	repo.deleteErr = errors.New("storage exploded")

	err := New(repo).Delete(context.Background(), "id-1")
	if !errors.Is(err, repo.deleteErr) {
		t.Fatalf("Delete() error = %v, want wrapped repository error", err)
	}
}
