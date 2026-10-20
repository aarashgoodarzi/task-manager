package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/aarashgoodarzi/task-manager/api"
	"github.com/aarashgoodarzi/task-manager/internal/infra/handler"
	"github.com/aarashgoodarzi/task-manager/internal/infra/swagger"
)

func init() {
	gin.SetMode(gin.TestMode)
}

type stubHealthChecker struct {
	err error
}

func (s stubHealthChecker) PingContext(context.Context) error {
	return s.err
}

type stubRegistrar struct {
	called bool
}

func (s *stubRegistrar) RegisterRoutes(r gin.IRouter) {
	s.called = true
	r.GET("/stub", func(c *gin.Context) {
		c.Status(http.StatusNoContent)
	})
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func newTestServer(t *testing.T, db HealthChecker, modules ...RouteRegistrar) *Server {
	t.Helper()
	return New(Config{Addr: ":0"}, discardLogger(), db, modules...)
}

func serve(s *Server, method, path string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(method, path, nil)
	s.buildRouter().ServeHTTP(rec, req)
	return rec
}

func decodeJSON(t *testing.T, rec *httptest.ResponseRecorder, into any) {
	t.Helper()
	if err := json.Unmarshal(rec.Body.Bytes(), into); err != nil {
		t.Fatalf("decode response: %v (body = %s)", err, rec.Body.String())
	}
}

func TestHealthReportsDatabaseOK(t *testing.T) {
	rec := serve(newTestServer(t, stubHealthChecker{}), http.MethodGet, "/health")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body = %s", rec.Code, rec.Body.String())
	}

	var body map[string]string
	decodeJSON(t, rec, &body)
	if body["status"] != "ok" || body["database"] != "ok" {
		t.Fatalf("body = %v, want status=ok database=ok", body)
	}
}

func TestHealthReportsDatabaseUnreachable(t *testing.T) {
	rec := serve(newTestServer(t, stubHealthChecker{err: errors.New("connection refused")}), http.MethodGet, "/health")

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503, body = %s", rec.Code, rec.Body.String())
	}

	var body map[string]string
	decodeJSON(t, rec, &body)
	if body["status"] != "error" || body["error"] != "database unreachable" {
		t.Fatalf("body = %v, want status=error with an unreachable message", body)
	}
	if body["database"] != "" {
		t.Fatalf("body = %v, must not claim a database status when the ping failed", body)
	}
}

func TestHealthReportsMissingCheckerAsUnready(t *testing.T) {
	rec := serve(newTestServer(t, nil), http.MethodGet, "/health")

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503, body = %s", rec.Code, rec.Body.String())
	}

	var body map[string]string
	decodeJSON(t, rec, &body)
	if body["status"] != "error" || body["error"] != "database not configured" {
		t.Fatalf("body = %v, want an explicit not-configured error", body)
	}
	if body["database"] != "" {
		t.Fatalf("body = %v, must not claim a database status without a check", body)
	}
}

func TestLiveReportsProcessHealthWithoutDatabase(t *testing.T) {
	rec := serve(newTestServer(t, nil), http.MethodGet, "/health/live")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body = %s", rec.Code, rec.Body.String())
	}

	var body map[string]string
	decodeJSON(t, rec, &body)
	if body["status"] != "ok" {
		t.Fatalf("body = %v, want status=ok", body)
	}
	if len(body) != 1 {
		t.Fatalf("body = %v, want liveness to report only the process", body)
	}
}

func TestUnknownRouteReturnsErrorEnvelope(t *testing.T) {
	rec := serve(newTestServer(t, nil), http.MethodGet, "/unknown")

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404, body = %s", rec.Code, rec.Body.String())
	}

	var body handler.Response
	decodeJSON(t, rec, &body)
	if body.Error.Code != handler.CodeNotFound {
		t.Fatalf("code = %q, want %q", body.Error.Code, handler.CodeNotFound)
	}
}

func TestMethodNotAllowedReturnsErrorEnvelope(t *testing.T) {
	rec := serve(newTestServer(t, nil), http.MethodPut, "/health")

	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405, body = %s", rec.Code, rec.Body.String())
	}

	var body handler.Response
	decodeJSON(t, rec, &body)
	if body.Error.Code != handler.CodeMethodNotAllowed {
		t.Fatalf("code = %q, want %q", body.Error.Code, handler.CodeMethodNotAllowed)
	}
}

func TestModulesRegisterRoutes(t *testing.T) {
	registrar := &stubRegistrar{}
	s := newTestServer(t, nil, registrar)

	if !registrar.called {
		t.Fatal("RegisterRoutes was not called")
	}

	if rec := serve(s, http.MethodGet, "/stub"); rec.Code != http.StatusNoContent {
		t.Fatalf("stub route status = %d, want 204", rec.Code)
	}
}

func TestDocumentationRoutesAreServedFromModules(t *testing.T) {
	s := newTestServer(t, nil, swagger.New())

	spec := serve(s, http.MethodGet, swagger.SpecPath)
	if spec.Code != http.StatusOK {
		t.Fatalf("spec status = %d, want 200, body = %s", spec.Code, spec.Body.String())
	}
	if !bytes.Equal(spec.Body.Bytes(), api.OpenAPI) {
		t.Fatal("served spec differs from the embedded contract")
	}

	ui := serve(s, http.MethodGet, swagger.UIPath)
	if ui.Code != http.StatusOK {
		t.Fatalf("UI status = %d, want 200, body = %s", ui.Code, ui.Body.String())
	}
	if !strings.Contains(ui.Body.String(), "Task Manager API") {
		t.Fatal("UI did not render the API title")
	}
}
