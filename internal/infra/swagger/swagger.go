// Package swagger serves the embedded OpenAPI contract and an offline Swagger
// UI. It is an HTTP adapter: it knows about Gin and the documentation assets,
// but no feature handler or business package depends on it.
package swagger

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/swaggest/swgui/v5emb"

	"github.com/aarashgoodarzi/task-manager/api"
)

const (
	// SpecPath is the public path of the embedded OpenAPI document.
	SpecPath = "/swagger/openapi.yaml"
	// UIPath is the public base path of the Swagger UI.
	UIPath = "/swagger/"
)

const specContentType = "application/yaml; charset=utf-8"

// Handler registers the documentation routes on the shared router.
type Handler struct {
	ui http.Handler
}

// New returns a handler backed by the assets compiled into the binary. The UI
// reads the specification from SpecPath, so no external origin or runtime file
// is involved.
func New() *Handler {
	return &Handler{
		ui: v5emb.New("Task Manager API", SpecPath, UIPath),
	}
}

// RegisterRoutes mounts the read-only documentation endpoints. Only GET is
// registered; an unsupported method on the same path falls through to the
// router's 405 handler.
func (h *Handler) RegisterRoutes(r gin.IRouter) {
	// A single wildcard route lets the spec and the UI share the /swagger/
	// namespace without conflicting with each other.
	r.GET("/swagger/*any", h.serve)
}

func (h *Handler) serve(c *gin.Context) {
	switch c.Param("any") {
	case "/openapi.yaml":
		c.Data(http.StatusOK, specContentType, api.OpenAPI)
		return
	case "/index.html":
		// Swagger UI's index is served at the base path. Rewriting the request
		// lets /swagger/index.html work without redirecting the browser.
		c.Request.URL.Path = UIPath
	}

	h.ui.ServeHTTP(c.Writer, c.Request)
}
