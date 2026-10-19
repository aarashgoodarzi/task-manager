// Package server is the HTTP delivery layer: Gin router setup, middleware,
// and process lifecycle. It depends on Gin and on the HealthChecker port
// defined below; it must never contain business logic or SQL.
package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
)

// HealthChecker is the only capability the server needs from the database
// adapter. It is defined here, at the point of use, rather than in the
// database package, so that this package stays decoupled from any specific
// driver — *sql.DB already satisfies it.
type HealthChecker interface {
	PingContext(ctx context.Context) error
}

// Config drives the HTTP listener and timeouts.
type Config struct {
	Addr              string
	ReadHeaderTimeout time.Duration
	ReadTimeout       time.Duration
	WriteTimeout      time.Duration
	IdleTimeout       time.Duration
}

// Server owns routing, middleware, and the HTTP server lifecycle.
type Server struct {
	log *slog.Logger
	db  HealthChecker
	srv *http.Server
}

// New builds the Gin router and wraps it in an *http.Server. db may be nil
// if no readiness check is desired (e.g. in tests).
func New(cfg Config, log *slog.Logger, db HealthChecker) *Server {
	s := &Server{log: log, db: db}
	s.srv = &http.Server{
		Addr:              cfg.Addr,
		Handler:           s.buildRouter(),
		ReadHeaderTimeout: cfg.ReadHeaderTimeout,
		ReadTimeout:       cfg.ReadTimeout,
		WriteTimeout:      cfg.WriteTimeout,
		IdleTimeout:       cfg.IdleTimeout,
	}
	return s
}

// Run starts the server and blocks until ctx is cancelled, then shuts down
// gracefully.
func (s *Server) Run(ctx context.Context) error {
	errCh := make(chan error, 1)
	go func() {
		s.log.Info("server listening", slog.String("addr", s.srv.Addr))
		if err := s.srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
			return
		}
		close(errCh)
	}()

	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		s.log.Info("shutting down server")
		return s.srv.Shutdown(shutdownCtx)
	case err := <-errCh:
		if err != nil {
			return fmt.Errorf("server: %w", err)
		}
		return nil
	}
}

func (s *Server) buildRouter() *gin.Engine {
	r := gin.New()
	r.Use(gin.Recovery())
	r.Use(s.requestLogger())
	r.GET("/health", s.handleHealth)
	return r
}

func (s *Server) handleHealth(c *gin.Context) {
	if s.db == nil {
		c.JSON(http.StatusOK, gin.H{"status": "ok"})
		return
	}

	ctx, cancel := context.WithTimeout(c.Request.Context(), 2*time.Second)
	defer cancel()

	if err := s.db.PingContext(ctx); err != nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"status": "error", "error": "database unreachable"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"status": "ok"})
}

func (s *Server) requestLogger() gin.HandlerFunc {
	return func(c *gin.Context) {
		start := time.Now()
		c.Next()
		if s.log == nil {
			return
		}
		s.log.Info("request",
			slog.String("method", c.Request.Method),
			slog.String("path", c.Request.URL.Path),
			slog.Int("status", c.Writer.Status()),
			slog.Duration("duration", time.Since(start)),
		)
	}
}
