// Package handler is the HTTP driving adapter for the task feature. It decodes
// requests, calls the task use cases through the narrow port declared here, and
// maps results and application errors to HTTP responses.
package handler

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"

	"github.com/gin-gonic/gin"

	infraHandler "github.com/aarashgoodarzi/task-manager/internal/infra/handler"
	"github.com/aarashgoodarzi/task-manager/internal/task/domain"
	"github.com/aarashgoodarzi/task-manager/internal/task/service"
)

// TaskService is the subset of the task use cases this adapter needs. It is
// declared at the point of use, so the handler can be tested with a stub.
type TaskService interface {
	Create(ctx context.Context, in service.Input) (domain.Task, error)
	List(ctx context.Context) ([]domain.Task, error)
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
}

type taskResponse struct {
	ID          string        `json:"id"`
	Title       string        `json:"title"`
	Description string        `json:"description"`
	Status      domain.Status `json:"status"`
}

func toResponse(task domain.Task) taskResponse {
	return taskResponse{
		ID:          task.ID,
		Title:       task.Title,
		Description: task.Description,
		Status:      task.Status,
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
	})
	if err != nil {
		h.writeError(c, err)
		return
	}

	c.Header("Location", "/tasks/"+task.ID)
	c.JSON(http.StatusCreated, toResponse(task))
}

func (h *Handler) list(c *gin.Context) {
	tasks, err := h.service.List(c.Request.Context())
	if err != nil {
		h.writeError(c, err)
		return
	}

	responses := make([]taskResponse, 0, len(tasks))
	for _, task := range tasks {
		responses = append(responses, toResponse(task))
	}
	c.JSON(http.StatusOK, responses)
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
