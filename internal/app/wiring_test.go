package app

import (
	"io"
	"log/slog"
	"net/http"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/aarashgoodarzi/task-manager/internal/task/repository/postgres"
)

// TestBuildHandlersComposesTaskAndDocsRoutes verifies the composition point
// hands the server every module route the service promises. Registering the
// returned registrars on a bare router exercises the same RegisterRoutes calls
// server.New makes, without a database or a listening socket.
func TestBuildHandlersComposesTaskAndDocsRoutes(t *testing.T) {
	a := &App{log: slog.New(slog.NewTextHandler(io.Discard, nil))}

	// Route registration never touches the pool, so a repository with no
	// database is enough to observe which routes each module contributes.
	modules := a.buildHandlers(postgres.New(nil))
	if len(modules) == 0 {
		t.Fatal("buildHandlers returned no modules")
	}

	r := gin.New()
	for i, module := range modules {
		if module == nil {
			t.Fatalf("module %d is nil; the server would panic mounting it", i)
		}
		module.RegisterRoutes(r)
	}

	registered := make(map[string]bool, len(r.Routes()))
	for _, route := range r.Routes() {
		registered[route.Method+" "+route.Path] = true
	}

	want := []string{
		http.MethodPost + " /task",
		http.MethodGet + " /tasks",
		http.MethodGet + " /tasks/:id",
		http.MethodPut + " /tasks/:id",
		http.MethodDelete + " /tasks/:id",
		http.MethodGet + " /swagger/*any",
	}
	for _, route := range want {
		if !registered[route] {
			t.Errorf("route %q is not registered by buildHandlers", route)
		}
	}
}
