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

// ListPage mirrors the SQL adapter's contract: filter, order by ID, apply the
// exclusive after-id bound, then limit.
func (r *stubRepository) ListPage(_ context.Context, filter service.Filter) (service.TaskPage, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	matched := make([]domain.Task, 0, len(r.tasks))
	for _, task := range r.tasks {
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
	return service.TaskPage{Tasks: matched}, nil
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
	return newTestRouter(service.New(newStubRepository(), nil, discardLogger()))
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

func decodeList(t *testing.T, rec *httptest.ResponseRecorder) taskListResponse {
	t.Helper()
	var page taskListResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &page); err != nil {
		t.Fatalf("decode task list: %v (body = %s)", err, rec.Body.String())
	}
	return page
}

func decodeTasks(t *testing.T, rec *httptest.ResponseRecorder) []taskResponse {
	t.Helper()
	return decodeList(t, rec).Items
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

	rec := doRequest(router, http.MethodPost, "/task",
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
	rec := doRequest(newTaskRouter(), http.MethodPost, "/task",
		`{"title":"ship it","status":"in_progress"}`)

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201, body = %s", rec.Code, rec.Body.String())
	}
	if task := decodeTask(t, rec); task.Status != domain.StatusInProgress {
		t.Fatalf("status = %q, want %q", task.Status, domain.StatusInProgress)
	}
}

// TestPostToCollectionDoesNotCreateTask pins the route contract: creation lives
// on /task, and the collection path accepts reads only. Gin answers 405 because
// /tasks is a registered path with a different method, exactly as the
// production router does (HandleMethodNotAllowed is enabled there).
func TestPostToCollectionDoesNotCreateTask(t *testing.T) {
	router := gin.New()
	router.HandleMethodNotAllowed = true
	New(service.New(newStubRepository(), nil, discardLogger()), discardLogger()).RegisterRoutes(router)

	rec := doRequest(router, http.MethodPost, "/tasks", `{"title":"must not be created"}`)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405, body = %s", rec.Code, rec.Body.String())
	}

	listed := doRequest(router, http.MethodGet, "/tasks", "")
	if listed.Code != http.StatusOK {
		t.Fatalf("GET /tasks status = %d, want 200", listed.Code)
	}
	if tasks := decodeTasks(t, listed); len(tasks) != 0 {
		t.Fatalf("POST /tasks created %d task(s), want none", len(tasks))
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
			rec := doRequest(newTaskRouter(), http.MethodPost, "/task", tc.body)

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
			rec := doRequest(newTaskRouter(), http.MethodPost, "/task", tc.body)

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

	page := decodeList(t, rec)
	if page.Items == nil || len(page.Items) != 0 {
		t.Fatalf("items = %#v, want an empty array", page.Items)
	}
	if page.PageSize != service.DefaultPageSize || page.HasMore || page.NextCursor != nil {
		t.Fatalf("page = %+v, want defaults with no continuation", page)
	}
	if strings.Contains(rec.Body.String(), `"items":null`) {
		t.Fatalf("body = %s, want items encoded as []", rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"next_cursor":null`) {
		t.Fatalf("body = %s, want next_cursor encoded as null", rec.Body.String())
	}
}

func TestListTasksPopulated(t *testing.T) {
	router := newTaskRouter()

	for _, title := range []string{"first", "second"} {
		rec := doRequest(router, http.MethodPost, "/task", `{"title":"`+title+`"}`)
		if rec.Code != http.StatusCreated {
			t.Fatalf("create %q status = %d, want 201", title, rec.Code)
		}
	}

	rec := doRequest(router, http.MethodGet, "/tasks", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}

	page := decodeList(t, rec)
	if page.HasMore || page.NextCursor != nil {
		t.Fatalf("page = %+v, want a complete single page", page)
	}
	tasks := page.Items
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

	created := decodeTask(t, doRequest(router, http.MethodPost, "/task", `{"title":"old title"}`))

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

	created := decodeTask(t, doRequest(router, http.MethodPost, "/task", `{"title":"task","status":"done"}`))

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
	created := decodeTask(t, doRequest(router, http.MethodPost, "/task", `{"title":"task"}`))

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
	created := decodeTask(t, doRequest(router, http.MethodPost, "/task", `{"title":"disposable"}`))

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

// createTask posts one task through the HTTP layer and returns the decoded
// response, so list tests exercise the same write path clients use.
func createTask(t *testing.T, router *gin.Engine, body string) taskResponse {
	t.Helper()
	rec := doRequest(router, http.MethodPost, "/task", body)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create status = %d, want 201, body = %s", rec.Code, rec.Body.String())
	}
	return decodeTask(t, rec)
}

func TestListTasksFiltersByStatus(t *testing.T) {
	router := newTaskRouter()
	createTask(t, router, `{"title":"todo one"}`)
	createTask(t, router, `{"title":"in progress","status":"in_progress"}`)
	createTask(t, router, `{"title":"done one","status":"done"}`)

	page := decodeList(t, doRequest(router, http.MethodGet, "/tasks?status=in_progress", ""))
	if page.HasMore || len(page.Items) != 1 || page.Items[0].Title != "in progress" {
		t.Fatalf("items = %+v, want only the in_progress task", page.Items)
	}
}

func TestListTasksFiltersByAssignee(t *testing.T) {
	router := newTaskRouter()
	createTask(t, router, `{"title":"mine","assignee":"alice"}`)
	createTask(t, router, `{"title":"theirs","assignee":"bob"}`)
	createTask(t, router, `{"title":"nobody"}`)

	page := decodeList(t, doRequest(router, http.MethodGet, "/tasks?assignee=alice", ""))
	if page.HasMore || len(page.Items) != 1 || page.Items[0].Title != "mine" || page.Items[0].Assignee != "alice" {
		t.Fatalf("items = %+v, want only alice's task", page.Items)
	}
}

func TestListTasksCombinesFilters(t *testing.T) {
	router := newTaskRouter()
	createTask(t, router, `{"title":"alice todo","assignee":"alice"}`)
	createTask(t, router, `{"title":"alice done","assignee":"alice","status":"done"}`)
	createTask(t, router, `{"title":"bob done","assignee":"bob","status":"done"}`)

	page := decodeList(t, doRequest(router, http.MethodGet, "/tasks?status=done&assignee=alice", ""))
	if page.HasMore || len(page.Items) != 1 || page.Items[0].Title != "alice done" {
		t.Fatalf("page = %+v, want only alice's done task", page)
	}
}

func TestListTasksPagination(t *testing.T) {
	router := newTaskRouter()
	for _, title := range []string{"one", "two", "three", "four", "five"} {
		createTask(t, router, `{"title":"`+title+`"}`)
	}

	wantSizes := []int{2, 2, 1}
	seen := make(map[string]struct{}, 5)
	cursor := ""
	for index, wantSize := range wantSizes {
		path := "/tasks?page_size=2"
		if cursor != "" {
			path += "&cursor=" + cursor
		}
		page := decodeList(t, doRequest(router, http.MethodGet, path, ""))

		if page.PageSize != 2 {
			t.Fatalf("page_size = %d, want 2", page.PageSize)
		}
		wantMore := index < len(wantSizes)-1
		if page.HasMore != wantMore {
			t.Fatalf("page %d has_more = %v, want %v", index+1, page.HasMore, wantMore)
		}
		if wantMore {
			if page.NextCursor == nil || *page.NextCursor == "" {
				t.Fatalf("page %d missing next_cursor", index+1)
			}
			cursor = *page.NextCursor
		} else if page.NextCursor != nil {
			t.Fatalf("final page next_cursor = %v, want null", page.NextCursor)
		}
		if len(page.Items) != wantSize {
			t.Fatalf("page %d has %d items, want %d", index+1, len(page.Items), wantSize)
		}
		for _, item := range page.Items {
			if _, duplicate := seen[item.ID]; duplicate {
				t.Fatalf("task %s appeared on more than one page", item.ID)
			}
			seen[item.ID] = struct{}{}
		}
	}

	if len(seen) != 5 {
		t.Fatalf("collected %d distinct tasks across three pages, want 5", len(seen))
	}
}

func TestListTasksRejectsMalformedAndIncompatibleCursor(t *testing.T) {
	router := newTaskRouter()
	createTask(t, router, `{"title":"one"}`)
	createTask(t, router, `{"title":"two"}`)

	first := decodeList(t, doRequest(router, http.MethodGet, "/tasks?page_size=1", ""))
	if first.NextCursor == nil {
		t.Fatal("expected a continuation cursor")
	}

	malformed := doRequest(router, http.MethodGet, "/tasks?cursor=not-a-cursor", "")
	if malformed.Code != http.StatusBadRequest {
		t.Fatalf("malformed status = %d, want 400", malformed.Code)
	}
	if body := decodeError(t, malformed); body.Error.Fields["cursor"] == "" {
		t.Fatalf("fields = %v, want a cursor entry", body.Error.Fields)
	}

	incompatible := doRequest(router, http.MethodGet,
		"/tasks?cursor="+*first.NextCursor+"&page_size=2", "")
	if incompatible.Code != http.StatusBadRequest {
		t.Fatalf("incompatible status = %d, want 400", incompatible.Code)
	}
	if body := decodeError(t, incompatible); body.Error.Fields["cursor"] == "" {
		t.Fatalf("fields = %v, want a cursor entry", body.Error.Fields)
	}
}

func TestListTasksEmptyFilterResult(t *testing.T) {
	router := newTaskRouter()
	createTask(t, router, `{"title":"todo","assignee":"alice"}`)

	page := decodeList(t, doRequest(router, http.MethodGet, "/tasks?status=done", ""))
	if page.HasMore || page.NextCursor != nil || len(page.Items) != 0 {
		t.Fatalf("page = %+v, want an empty result with no continuation", page)
	}
}

func TestListTasksAssigneeRoundTrip(t *testing.T) {
	router := newTaskRouter()
	created := createTask(t, router, `{"title":"owned","assignee":"  alice  "}`)

	if created.Assignee != "alice" {
		t.Fatalf("assignee = %q, want the trimmed label", created.Assignee)
	}
	fetched := decodeTask(t, doRequest(router, http.MethodGet, "/tasks/"+created.ID, ""))
	if fetched.Assignee != "alice" {
		t.Fatalf("fetched assignee = %q, want alice", fetched.Assignee)
	}

	rec := doRequest(router, http.MethodPut, "/tasks/"+created.ID,
		`{"title":"reassigned","assignee":"bob"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("update status = %d, want 200", rec.Code)
	}
	if updated := decodeTask(t, rec); updated.Assignee != "bob" {
		t.Fatalf("updated assignee = %q, want bob", updated.Assignee)
	}

	// PUT is a full replacement: omitting assignee clears it.
	rec = doRequest(router, http.MethodPut, "/tasks/"+created.ID, `{"title":"unassigned"}`)
	if updated := decodeTask(t, rec); updated.Assignee != "" {
		t.Fatalf("assignee = %q, want it cleared by replacement", updated.Assignee)
	}
}

func TestListTasksRejectsInvalidQuery(t *testing.T) {
	tests := []struct {
		name   string
		query  string
		field  string
		status int
	}{
		{name: "unknown status", query: "status=pending", field: "status"},
		{name: "empty status", query: "status=", field: "status"},
		{name: "empty assignee", query: "assignee=", field: "assignee"},
		{name: "whitespace assignee", query: "assignee=%20%20", field: "assignee"},
		{name: "page size zero", query: "page_size=0", field: "page_size"},
		{name: "negative page size", query: "page_size=-5", field: "page_size"},
		{name: "page size above maximum", query: "page_size=101", field: "page_size"},
		{name: "malformed page size", query: "page_size=ten", field: "page_size"},
		{name: "malformed cursor", query: "cursor=not-a-cursor", field: "cursor"},
		{name: "obsolete page parameter", query: "page=1", field: "page"},
		{name: "unknown parameter", query: "staus=todo", field: "staus"},
	}

	router := newTaskRouter()
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rec := doRequest(router, http.MethodGet, "/tasks?"+tc.query, "")
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400, body = %s", rec.Code, rec.Body.String())
			}
			body := decodeError(t, rec)
			if body.Error.Code != infraHandler.CodeValidationFailed {
				t.Fatalf("code = %q, want %q", body.Error.Code, infraHandler.CodeValidationFailed)
			}
			if _, ok := body.Error.Fields[tc.field]; !ok {
				t.Fatalf("fields = %v, want an entry for %q", body.Error.Fields, tc.field)
			}
		})
	}
}

func TestListTasksAcceptsMaximumPageSize(t *testing.T) {
	page := decodeList(t, doRequest(newTaskRouter(), http.MethodGet,
		fmt.Sprintf("/tasks?page_size=%d", service.MaxPageSize), ""))
	if page.PageSize != service.MaxPageSize {
		t.Fatalf("page_size = %d, want %d", page.PageSize, service.MaxPageSize)
	}
}

func TestTaskLifecycle(t *testing.T) {
	router := newTaskRouter()

	// Create.
	createRec := doRequest(router, http.MethodPost, "/task",
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

func (failingService) List(context.Context, service.ListQuery) (service.ListResult, error) {
	return service.ListResult{}, errors.New("secret storage detail")
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
		{method: http.MethodPost, path: "/task", body: `{"title":"task"}`},
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
