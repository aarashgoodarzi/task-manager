// Package handler is the HTTP driving adapter for the task feature. It decodes
// requests, calls the task use cases through the narrow port declared here, and
// maps results and application errors to HTTP responses.
package handler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"

	infraHandler "github.com/aarashgoodarzi/task-manager/internal/infra/handler"
	"github.com/aarashgoodarzi/task-manager/internal/task/domain"
	"github.com/aarashgoodarzi/task-manager/internal/task/service"
)

// TaskService is the subset of the task use cases this adapter needs. It is
// declared at the point of use, so the handler can be tested with a stub.
type TaskService interface {
	Create(ctx context.Context, in service.Input) (domain.Task, error)
	List(ctx context.Context, query service.ListQuery) (service.ListResult, error)
	Get(ctx context.Context, id string) (domain.Task, error)
	Update(ctx context.Context, id string, in service.Input) (domain.Task, error)
	Delete(ctx context.Context, id string) error
}

// Handler serves the task REST endpoints.
type Handler struct {
	service TaskService
	log     *slog.Logger
}

// New returns a Handler that calls svc and logs unexpected failures to log.
func New(svc TaskService, log *slog.Logger) *Handler {
	return &Handler{service: svc, log: log}
}

// RegisterRoutes mounts the task endpoints on r.
func (h *Handler) RegisterRoutes(r gin.IRouter) {
	// Creation is a separate path from the collection resource: /tasks is
	// read-only, and the created task lives at /tasks/{id}.
	r.POST("/task", h.create)
	r.GET("/tasks", h.list)
	r.GET("/tasks/:id", h.get)
	r.PUT("/tasks/:id", h.update)
	r.DELETE("/tasks/:id", h.delete)
}

type taskRequest struct {
	Title       string `json:"title"`
	Description string `json:"description"`
	Status      string `json:"status"`
	Assignee    string `json:"assignee"`
}

type taskResponse struct {
	ID          string        `json:"id"`
	Title       string        `json:"title"`
	Description string        `json:"description"`
	Status      domain.Status `json:"status"`
	Assignee    string        `json:"assignee"`
}

// taskListResponse is the paginated collection envelope. The metadata comes
// from the same filtered set as the items, so page and total cannot disagree.
type taskListResponse struct {
	Items      []taskResponse `json:"items"`
	Page       int            `json:"page"`
	PageSize   int            `json:"page_size"`
	Total      int64          `json:"total"`
	TotalPages int            `json:"total_pages"`
}

func toResponse(task domain.Task) taskResponse {
	return taskResponse{
		ID:          task.ID,
		Title:       task.Title,
		Description: task.Description,
		Status:      task.Status,
		Assignee:    task.Assignee,
	}
}

func (h *Handler) create(c *gin.Context) {
	req, ok := decodeTaskRequest(c)
	if !ok {
		return
	}

	task, err := h.service.Create(c.Request.Context(), service.Input{
		Title:       req.Title,
		Description: req.Description,
		Status:      req.Status,
		Assignee:    req.Assignee,
	})
	if err != nil {
		h.writeError(c, err)
		return
	}

	c.Header("Location", "/tasks/"+task.ID)
	c.JSON(http.StatusCreated, toResponse(task))
}

func (h *Handler) list(c *gin.Context) {
	query, ok := decodeListQuery(c)
	if !ok {
		return
	}

	result, err := h.service.List(c.Request.Context(), query)
	if err != nil {
		h.writeError(c, err)
		return
	}

	items := make([]taskResponse, 0, len(result.Tasks))
	for _, task := range result.Tasks {
		items = append(items, toResponse(task))
	}
	c.JSON(http.StatusOK, taskListResponse{
		Items:      items,
		Page:       result.Page,
		PageSize:   result.PageSize,
		Total:      result.Total,
		TotalPages: result.TotalPages,
	})
}

func (h *Handler) get(c *gin.Context) {
	id, ok := taskID(c)
	if !ok {
		return
	}

	task, err := h.service.Get(c.Request.Context(), id)
	if err != nil {
		h.writeError(c, err)
		return
	}
	c.JSON(http.StatusOK, toResponse(task))
}

func (h *Handler) update(c *gin.Context) {
	id, ok := taskID(c)
	if !ok {
		return
	}
	req, ok := decodeTaskRequest(c)
	if !ok {
		return
	}

	task, err := h.service.Update(c.Request.Context(), id, service.Input{
		Title:       req.Title,
		Description: req.Description,
		Status:      req.Status,
		Assignee:    req.Assignee,
	})
	if err != nil {
		h.writeError(c, err)
		return
	}
	c.JSON(http.StatusOK, toResponse(task))
}

func (h *Handler) delete(c *gin.Context) {
	id, ok := taskID(c)
	if !ok {
		return
	}

	if err := h.service.Delete(c.Request.Context(), id); err != nil {
		h.writeError(c, err)
		return
	}
	c.Status(http.StatusNoContent)
}

// listQueryParams is the closed set of parameters GET /tasks accepts. Unknown
// names are rejected rather than ignored, so a typo cannot silently return an
// unfiltered collection.
var listQueryParams = map[string]struct{}{
	"status":    {},
	"assignee":  {},
	"page":      {},
	"page_size": {},
}

// decodeListQuery parses and validates the collection query string. It writes
// the error envelope itself and reports whether the request may continue.
// Range rules (page >= 1, page_size <= MaxPageSize, assignee length, overflow)
// live in the use case and are reported with the same envelope.
func decodeListQuery(c *gin.Context) (service.ListQuery, bool) {
	values := c.Request.URL.Query()
	fields := make(map[string]string)

	for name := range values {
		if _, ok := listQueryParams[name]; !ok {
			fields[name] = "unsupported query parameter"
		}
	}

	var query service.ListQuery

	if values.Has("status") {
		status, err := domain.ParseStatusFilter(values.Get("status"))
		switch {
		case err != nil:
			fields["status"] = `must be one of "todo", "in_progress", "done"`
		case status == "":
			fields["status"] = "must not be empty"
		default:
			query.Status = status
		}
	}

	if values.Has("assignee") {
		assignee := strings.TrimSpace(values.Get("assignee"))
		if assignee == "" {
			fields["assignee"] = "must not be empty"
		} else {
			query.Assignee = assignee
		}
	}

	if values.Has("page") {
		page, err := strconv.Atoi(values.Get("page"))
		switch {
		case err != nil:
			fields["page"] = "must be a whole number"
		case page < 1:
			// An explicit zero is a client error; only an omitted page uses
			// the default, so the two cases cannot be confused.
			fields["page"] = "must be at least 1"
		default:
			query.Page = page
		}
	}

	if values.Has("page_size") {
		pageSize, err := strconv.Atoi(values.Get("page_size"))
		switch {
		case err != nil:
			fields["page_size"] = "must be a whole number"
		case pageSize < 1:
			fields["page_size"] = "must be at least 1"
		case pageSize > service.MaxPageSize:
			fields["page_size"] = fmt.Sprintf("must be at most %d", service.MaxPageSize)
		default:
			query.PageSize = pageSize
		}
	}

	if len(fields) == 0 {
		normalized, err := query.Normalize()
		if err != nil {
			var validation *domain.ValidationError
			if errors.As(err, &validation) {
				fields[validation.Field] = validation.Message
			} else {
				fields["query"] = "invalid query parameters"
			}
		} else {
			return normalized, true
		}
	}

	infraHandler.Write(c, http.StatusBadRequest, infraHandler.CodeValidationFailed,
		"invalid query parameters", fields)
	return service.ListQuery{}, false
}

// decodeTaskRequest strictly decodes a single JSON object. Unknown fields and
// trailing content are rejected so malformed requests fail loudly instead of
// being silently ignored.
func decodeTaskRequest(c *gin.Context) (taskRequest, bool) {
	var req taskRequest

	dec := json.NewDecoder(c.Request.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		infraHandler.Write(c, http.StatusBadRequest, infraHandler.CodeInvalidRequest,
			"request body must be a valid JSON object", nil)
		return taskRequest{}, false
	}
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		infraHandler.Write(c, http.StatusBadRequest, infraHandler.CodeInvalidRequest,
			"request body must contain a single JSON object", nil)
		return taskRequest{}, false
	}

	return req, true
}

// taskID extracts and validates the :id path parameter.
func taskID(c *gin.Context) (string, bool) {
	id := c.Param("id")
	if !domain.IsValidID(id) {
		infraHandler.Write(c, http.StatusBadRequest, infraHandler.CodeValidationFailed,
			"invalid task id", map[string]string{"id": "must be a UUID"})
		return "", false
	}
	return id, true
}

// writeError maps application and domain errors to HTTP responses. Anything
// unrecognized becomes a generic 500 so internal details never reach clients.
func (h *Handler) writeError(c *gin.Context, err error) {
	var validation *domain.ValidationError
	switch {
	case errors.As(err, &validation):
		infraHandler.Write(c, http.StatusBadRequest, infraHandler.CodeValidationFailed,
			"invalid task data", map[string]string{validation.Field: validation.Message})
	case errors.Is(err, service.ErrNotFound):
		infraHandler.Write(c, http.StatusNotFound, infraHandler.CodeNotFound, "task not found", nil)
	default:
		if h.log != nil {
			h.log.Error("task request failed",
				slog.String("method", c.Request.Method),
				slog.String("path", c.Request.URL.Path),
				slog.Any("error", err),
			)
		}
		infraHandler.Internal(c)
	}
}
