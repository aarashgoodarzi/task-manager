package server

import (
	"context"
	"errors"
	"log/slog"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
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

func newTestServer(t *testing.T, db HealthChecker) *Server {
	t.Helper()
	return New(Config{Addr: ":0"}, slog.Default(), db)
}

func TestHealthReturnsOKWhenDatabaseHealthy(t *testing.T) {
	s := newTestServer(t, stubHealthChecker{})
	router := s.buildRouter()

	req := httptest.NewRequest("GET", "/health", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200, body = %s", rec.Code, rec.Body.String())
	}
}

func TestHealthReturnsServiceUnavailableWhenDatabaseDown(t *testing.T) {
	s := newTestServer(t, stubHealthChecker{err: errors.New("connection refused")})
	router := s.buildRouter()

	req := httptest.NewRequest("GET", "/health", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != 503 {
		t.Fatalf("status = %d, want 503, body = %s", rec.Code, rec.Body.String())
	}
}

func TestHealthReturnsOKWhenNoDatabaseConfigured(t *testing.T) {
	s := newTestServer(t, nil)
	router := s.buildRouter()

	req := httptest.NewRequest("GET", "/health", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200, body = %s", rec.Code, rec.Body.String())
	}
}
