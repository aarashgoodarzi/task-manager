package handler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/gin-gonic/gin"

	infraHandler "github.com/aarashgoodarzi/task-manager/internal/infra/handler"
	"github.com/aarashgoodarzi/task-manager/internal/task/domain"
	"github.com/aarashgoodarzi/task-manager/internal/task/service"
)

const unknownID = "11111111-1111-4111-8111-111111111111"

func init() {
	gin.SetMode(gin.TestMode)
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func newTestRouter(svc TaskService) *gin.Engine {
	r := gin.New()
	New(svc, discardLogger()).RegisterRoutes(r)
	return r
}

// stubRepository is a hand-written test double for the repository port the
// service declares. The HTTP contract must stay testable without PostgreSQL;
// the real SQL adapter is covered by its own integration suite.
type stubRepository struct {
	mu    sync.Mutex
	tasks map[string]domain.Task
	seq   int
}

func newStubRepository() *stubRepository {
	return &stubRepository{tasks: make(map[string]domain.Task)}
}

func (r *stubRepository) Create(_ context.Context, task domain.Task) (domain.Task, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.seq++
	task.ID = fmt.Sprintf("%08x-0000-4000-8000-000000000000", r.seq)
	r.tasks[task.ID] = task
	return task, nil
}

func (r *stubRepository) List(_ context.Context) ([]domain.Task, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	tasks := make([]domain.Task, 0, len(r.tasks))
	for _, task := range r.tasks {
		tasks = append(tasks, task)
	}
	sort.Slice(tasks, func(i, j int) bool { return tasks[i].ID < tasks[j].ID })
	return tasks, nil
}

func (r *stubRepository) Get(_ context.Context, id string) (domain.Task, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	task, ok := r.tasks[id]
	if !ok {
		return domain.Task{}, service.ErrNotFound
	}
	return task, nil
}

func (r *stubRepository) Update(_ context.Context, task domain.Task) (domain.Task, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if _, ok := r.tasks[task.ID]; !ok {
		return domain.Task{}, service.ErrNotFound
	}
	r.tasks[task.ID] = task
	return task, nil
}

func (r *stubRepository) Delete(_ context.Context, id string) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if _, ok := r.tasks[id]; !ok {
		return service.ErrNotFound
	}
	delete(r.tasks, id)
	return nil
}

// newTaskRouter wires the real handler and service with the repository double.
func newTaskRouter() *gin.Engine {
	return newTestRouter(service.New(newStubRepository()))
}

func doRequest(r http.Handler, method, path, body string) *httptest.ResponseRecorder {
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}

	req := httptest.NewRequest(method, path, reader)
	req.Header.Set("Content-Type", "application/json")

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	return rec
}

func decodeTask(t *testing.T, rec *httptest.ResponseRecorder) taskResponse {
	t.Helper()
	var task taskResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &task); err != nil {
		t.Fatalf("decode task response: %v (body = %s)", err, rec.Body.String())
	}
	return task
}

func decodeTasks(t *testing.T, rec *httptest.ResponseRecorder) []taskResponse {
	t.Helper()
	var tasks []taskResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &tasks); err != nil {
		t.Fatalf("decode task list: %v (body = %s)", err, rec.Body.String())
	}
	return tasks
}

func decodeError(t *testing.T, rec *httptest.ResponseRecorder) infraHandler.Response {
	t.Helper()
	var body infraHandler.Response
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode error response: %v (body = %s)", err, rec.Body.String())
	}
	return body
}

func TestCreateTask(t *testing.T) {
	router := newTaskRouter()

	rec := doRequest(router, http.MethodPost, "/tasks",
		`{"title":"  write documentation  ","description":"phase 1.5"}`)

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201, body = %s", rec.Code, rec.Body.String())
	}

	task := decodeTask(t, rec)
	if !domain.IsValidID(task.ID) {
		t.Fatalf("id = %q, want a UUID", task.ID)
	}
	if task.Title != "write documentation" {
		t.Fatalf("title = %q, want trimmed title", task.Title)
	}
	if task.Description != "phase 1.5" {
		t.Fatalf("description = %q, want %q", task.Description, "phase 1.5")
	}
	if task.Status != domain.StatusTodo {
		t.Fatalf("status = %q, want default %q", task.Status, domain.StatusTodo)
	}
	if got, want := rec.Header().Get("Location"), "/tasks/"+task.ID; got != want {
		t.Fatalf("Location = %q, want %q", got, want)
	}

	// The created task is retrievable under the returned identifier.
	got := doRequest(router, http.MethodGet, "/tasks/"+task.ID, "")
	if got.Code != http.StatusOK {
		t.Fatalf("GET created task status = %d, want 200", got.Code)
	}
	if fetched := decodeTask(t, got); fetched != task {
		t.Fatalf("fetched task = %+v, want %+v", fetched, task)
	}
}

func TestCreateTaskWithExplicitStatus(t *testing.T) {
	rec := doRequest(newTaskRouter(), http.MethodPost, "/tasks",
		`{"title":"ship it","status":"in_progress"}`)

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201, body = %s", rec.Code, rec.Body.String())
	}
	if task := decodeTask(t, rec); task.Status != domain.StatusInProgress {
		t.Fatalf("status = %q, want %q", task.Status, domain.StatusInProgress)
	}
}

func TestCreateTaskRejectsMalformedJSON(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{name: "syntax error", body: `{"title":`},
		{name: "wrong field type", body: `{"title":42}`},
		{name: "unknown field", body: `{"title":"a","priority":"high"}`},
		{name: "multiple objects", body: `{"title":"a"}{"title":"b"}`},
		{name: "empty body", body: ""},
		{name: "json array", body: `[]`},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rec := doRequest(newTaskRouter(), http.MethodPost, "/tasks", tc.body)

			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400, body = %s", rec.Code, rec.Body.String())
			}
			body := decodeError(t, rec)
			if body.Error.Code != infraHandler.CodeInvalidRequest {
				t.Fatalf("code = %q, want %q", body.Error.Code, infraHandler.CodeInvalidRequest)
			}
			if body.Error.Message == "" {
				t.Fatal("message is empty")
			}
		})
	}
}

func TestCreateTaskRejectsInvalidFields(t *testing.T) {
	tests := []struct {
		name      string
		body      string
		wantField string
	}{
		{name: "missing title", body: `{"description":"no title"}`, wantField: "title"},
		{name: "empty title", body: `{"title":""}`, wantField: "title"},
		{name: "whitespace title", body: `{"title":"   "}`, wantField: "title"},
		{name: "unknown status", body: `{"title":"a","status":"pending"}`, wantField: "status"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rec := doRequest(newTaskRouter(), http.MethodPost, "/tasks", tc.body)

			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400, body = %s", rec.Code, rec.Body.String())
			}
			body := decodeError(t, rec)
			if body.Error.Code != infraHandler.CodeValidationFailed {
				t.Fatalf("code = %q, want %q", body.Error.Code, infraHandler.CodeValidationFailed)
			}
			if _, ok := body.Error.Fields[tc.wantField]; !ok {
				t.Fatalf("fields = %v, want an entry for %q", body.Error.Fields, tc.wantField)
			}
		})
	}
}

func TestListTasksEmpty(t *testing.T) {
	rec := doRequest(newTaskRouter(), http.MethodGet, "/tasks", "")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body = %s", rec.Code, rec.Body.String())
	}
	if got := rec.Body.String(); got != "[]" {
		t.Fatalf("body = %q, want an empty JSON array", got)
	}
}

func TestListTasksPopulated(t *testing.T) {
	router := newTaskRouter()

	for _, title := range []string{"first", "second"} {
		rec := doRequest(router, http.MethodPost, "/tasks", `{"title":"`+title+`"}`)
		if rec.Code != http.StatusCreated {
			t.Fatalf("create %q status = %d, want 201", title, rec.Code)
		}
	}

	rec := doRequest(router, http.MethodGet, "/tasks", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}

	tasks := decodeTasks(t, rec)
	if len(tasks) != 2 {
		t.Fatalf("len(tasks) = %d, want 2", len(tasks))
	}

	titles := map[string]bool{}
	for _, task := range tasks {
		titles[task.Title] = true
	}
	if !titles["first"] || !titles["second"] {
		t.Fatalf("titles = %v, want both created tasks", titles)
	}
}

func TestGetTaskNotFound(t *testing.T) {
	rec := doRequest(newTaskRouter(), http.MethodGet, "/tasks/"+unknownID, "")

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404, body = %s", rec.Code, rec.Body.String())
	}
	body := decodeError(t, rec)
	if body.Error.Code != infraHandler.CodeNotFound {
		t.Fatalf("code = %q, want %q", body.Error.Code, infraHandler.CodeNotFound)
	}
}

func TestUpdateTask(t *testing.T) {
	router := newTaskRouter()

	created := decodeTask(t, doRequest(router, http.MethodPost, "/tasks", `{"title":"old title"}`))

	rec := doRequest(router, http.MethodPut, "/tasks/"+created.ID,
		`{"title":"new title","description":"new description","status":"done"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body = %s", rec.Code, rec.Body.String())
	}

	updated := decodeTask(t, rec)
	want := taskResponse{ID: created.ID, Title: "new title", Description: "new description", Status: domain.StatusDone}
	if updated != want {
		t.Fatalf("updated task = %+v, want %+v", updated, want)
	}

	fetched := decodeTask(t, doRequest(router, http.MethodGet, "/tasks/"+created.ID, ""))
	if fetched != want {
		t.Fatalf("fetched task = %+v, want %+v", fetched, want)
	}
}

func TestUpdateTaskReplacesWithoutStatusDefaulting(t *testing.T) {
	router := newTaskRouter()

	created := decodeTask(t, doRequest(router, http.MethodPost, "/tasks", `{"title":"task","status":"done"}`))

	// PUT is a full replacement: omitting status resets it to the default.
	rec := doRequest(router, http.MethodPut, "/tasks/"+created.ID, `{"title":"task"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if updated := decodeTask(t, rec); updated.Status != domain.StatusTodo {
		t.Fatalf("status = %q, want %q", updated.Status, domain.StatusTodo)
	}
}

func TestUpdateTaskNotFound(t *testing.T) {
	rec := doRequest(newTaskRouter(), http.MethodPut, "/tasks/"+unknownID, `{"title":"ghost"}`)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404, body = %s", rec.Code, rec.Body.String())
	}
	if body := decodeError(t, rec); body.Error.Code != infraHandler.CodeNotFound {
		t.Fatalf("code = %q, want %q", body.Error.Code, infraHandler.CodeNotFound)
	}
}

func TestUpdateTaskRejectsInvalidInput(t *testing.T) {
	router := newTaskRouter()
	created := decodeTask(t, doRequest(router, http.MethodPost, "/tasks", `{"title":"task"}`))

	tests := []struct {
		name string
		body string
	}{
		{name: "missing title", body: `{"description":"no title"}`},
		{name: "invalid status", body: `{"title":"task","status":"pending"}`},
		{name: "malformed json", body: `{"title":`},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rec := doRequest(router, http.MethodPut, "/tasks/"+created.ID, tc.body)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400, body = %s", rec.Code, rec.Body.String())
			}
		})
	}
}

func TestDeleteTask(t *testing.T) {
	router := newTaskRouter()
	created := decodeTask(t, doRequest(router, http.MethodPost, "/tasks", `{"title":"disposable"}`))

	rec := doRequest(router, http.MethodDelete, "/tasks/"+created.ID, "")
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204, body = %s", rec.Code, rec.Body.String())
	}
	if rec.Body.Len() != 0 {
		t.Fatalf("body = %q, want no content", rec.Body.String())
	}

	notFound := doRequest(router, http.MethodGet, "/tasks/"+created.ID, "")
	if notFound.Code != http.StatusNotFound {
		t.Fatalf("GET after delete status = %d, want 404", notFound.Code)
	}
	if tasks := decodeTasks(t, doRequest(router, http.MethodGet, "/tasks", "")); len(tasks) != 0 {
		t.Fatalf("len(tasks) = %d, want 0 after delete", len(tasks))
	}
}

func TestDeleteTaskNotFound(t *testing.T) {
	rec := doRequest(newTaskRouter(), http.MethodDelete, "/tasks/"+unknownID, "")

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404, body = %s", rec.Code, rec.Body.String())
	}
	if body := decodeError(t, rec); body.Error.Code != infraHandler.CodeNotFound {
		t.Fatalf("code = %q, want %q", body.Error.Code, infraHandler.CodeNotFound)
	}
}

func TestMalformedTaskID(t *testing.T) {
	tests := []struct {
		method string
		body   string
	}{
		{method: http.MethodGet},
		{method: http.MethodPut, body: `{"title":"task"}`},
		{method: http.MethodDelete},
	}

	for _, tc := range tests {
		t.Run(tc.method, func(t *testing.T) {
			rec := doRequest(newTaskRouter(), tc.method, "/tasks/not-a-uuid", tc.body)

			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400, body = %s", rec.Code, rec.Body.String())
			}
			body := decodeError(t, rec)
			if body.Error.Code != infraHandler.CodeValidationFailed {
				t.Fatalf("code = %q, want %q", body.Error.Code, infraHandler.CodeValidationFailed)
			}
			if _, ok := body.Error.Fields["id"]; !ok {
				t.Fatalf("fields = %v, want an id entry", body.Error.Fields)
			}
		})
	}
}

func TestTaskLifecycle(t *testing.T) {
	router := newTaskRouter()

	// Create.
	createRec := doRequest(router, http.MethodPost, "/tasks",
		`{"title":"lifecycle","description":"initial","status":"todo"}`)
	if createRec.Code != http.StatusCreated {
		t.Fatalf("create status = %d, want 201", createRec.Code)
	}
	created := decodeTask(t, createRec)

	// Get.
	if rec := doRequest(router, http.MethodGet, "/tasks/"+created.ID, ""); rec.Code != http.StatusOK {
		t.Fatalf("get status = %d, want 200", rec.Code)
	}

	// List.
	list := decodeTasks(t, doRequest(router, http.MethodGet, "/tasks", ""))
	if len(list) != 1 || list[0].ID != created.ID {
		t.Fatalf("list = %+v, want the created task", list)
	}

	// Update.
	updateRec := doRequest(router, http.MethodPut, "/tasks/"+created.ID,
		`{"title":"lifecycle updated","description":"updated","status":"done"}`)
	if updateRec.Code != http.StatusOK {
		t.Fatalf("update status = %d, want 200, body = %s", updateRec.Code, updateRec.Body.String())
	}

	// Get reflects the update.
	updated := decodeTask(t, doRequest(router, http.MethodGet, "/tasks/"+created.ID, ""))
	if updated.Title != "lifecycle updated" || updated.Status != domain.StatusDone {
		t.Fatalf("updated task = %+v, want updated title and status", updated)
	}

	// Delete.
	if rec := doRequest(router, http.MethodDelete, "/tasks/"+created.ID, ""); rec.Code != http.StatusNoContent {
		t.Fatalf("delete status = %d, want 204", rec.Code)
	}

	// Verify it is gone.
	if rec := doRequest(router, http.MethodGet, "/tasks/"+created.ID, ""); rec.Code != http.StatusNotFound {
		t.Fatalf("get after delete status = %d, want 404", rec.Code)
	}
}

// failingService fails every use case with an error that must not reach the
// client.
type failingService struct{}

func (failingService) Create(context.Context, service.Input) (domain.Task, error) {
	return domain.Task{}, errors.New("secret storage detail")
}

func (failingService) List(context.Context) ([]domain.Task, error) {
	return nil, errors.New("secret storage detail")
}

func (failingService) Get(context.Context, string) (domain.Task, error) {
	return domain.Task{}, errors.New("secret storage detail")
}

func (failingService) Update(context.Context, string, service.Input) (domain.Task, error) {
	return domain.Task{}, errors.New("secret storage detail")
}

func (failingService) Delete(context.Context, string) error {
	return errors.New("secret storage detail")
}

func TestInternalErrorsAreNotLeaked(t *testing.T) {
	router := newTestRouter(failingService{})

	tests := []struct {
		method string
		path   string
		body   string
	}{
		{method: http.MethodPost, path: "/tasks", body: `{"title":"task"}`},
		{method: http.MethodGet, path: "/tasks"},
		{method: http.MethodGet, path: "/tasks/" + unknownID},
		{method: http.MethodPut, path: "/tasks/" + unknownID, body: `{"title":"task"}`},
		{method: http.MethodDelete, path: "/tasks/" + unknownID},
	}

	for _, tc := range tests {
		t.Run(tc.method+" "+tc.path, func(t *testing.T) {
			rec := doRequest(router, tc.method, tc.path, tc.body)

			if rec.Code != http.StatusInternalServerError {
				t.Fatalf("status = %d, want 500, body = %s", rec.Code, rec.Body.String())
			}
			if strings.Contains(rec.Body.String(), "secret storage detail") {
				t.Fatalf("body leaked internal error: %s", rec.Body.String())
			}

			body := decodeError(t, rec)
			if body.Error.Code != infraHandler.CodeInternal {
				t.Fatalf("code = %q, want %q", body.Error.Code, infraHandler.CodeInternal)
			}
			if body.Error.Message != "internal server error" {
				t.Fatalf("message = %q, want generic message", body.Error.Message)
			}
		})
	}
}
