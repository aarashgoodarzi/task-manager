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

	"github.com/aarashgoodarzi/task-manager/internal/infra/handler"
	"github.com/aarashgoodarzi/task-manager/internal/infra/metrics"
)

// HealthChecker is the only capability the server needs from the database
// adapter. It is defined here, at the point of use, rather than in the
// database package, so that this package stays decoupled from any specific
// driver — *sql.DB already satisfies it.
type HealthChecker interface {
	PingContext(ctx context.Context) error
}

// RouteRegistrar is a feature's HTTP adapter: it registers its own routes on
// the shared router. The server depends on this narrow port instead of on
// concrete feature handlers, so routes are composed in internal/app.
type RouteRegistrar interface {
	RegisterRoutes(r gin.IRouter)
}

// Instrumentation is the observability adapter the server records completed
// requests on and mounts at metrics.Path. It is declared here, at the point of
// use; *metrics.Metrics satisfies it. A nil value disables instrumentation and
// the /metrics route (tests use that; the composition root always supplies
// one).
type Instrumentation interface {
	ObserveRequest(method, route string, status int, duration time.Duration)
	Handler() http.Handler
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
	log     *slog.Logger
	db      HealthChecker
	instr   Instrumentation
	modules []RouteRegistrar
	srv     *http.Server
}

// New builds the Gin router and wraps it in an *http.Server. db is the
// database checker /health pings; a nil value makes /health report 503, since a
// process without persistence is not ready. Tests construct the server that way
// but the composition root always supplies a pool.
func New(cfg Config, log *slog.Logger, db HealthChecker, instr Instrumentation, modules ...RouteRegistrar) *Server {
	s := &Server{log: log, db: db, instr: instr, modules: modules}
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
	r.HandleMethodNotAllowed = true
	r.Use(gin.Recovery())
	r.Use(requestIDMiddleware())
	r.Use(s.observeRequests())

	if s.instr != nil {
		// gin.WrapH hands the ResponseWriter and request to the standard
		// Prometheus handler. observeRequests skips this route, so scrapes do
		// not appear in requests_total or request_latency_histogram.
		r.GET(metrics.Path, gin.WrapH(s.instr.Handler()))
	}

	r.GET("/health", s.handleHealth)
	r.GET("/health/live", s.handleLive)

	for _, module := range s.modules {
		module.RegisterRoutes(r)
	}

	r.NoRoute(func(c *gin.Context) {
		handler.Write(c, http.StatusNotFound, handler.CodeNotFound, "route not found", nil)
	})
	r.NoMethod(func(c *gin.Context) {
		handler.Write(c, http.StatusMethodNotAllowed, handler.CodeMethodNotAllowed, "method not allowed", nil)
	})

	return r
}

// handleLive answers a process-liveness probe. It never touches the database,
// so an orchestrator can tell a running process from a ready one.
func (s *Server) handleLive(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{"status": "ok"})
}

// handleHealth answers a readiness probe: the process plus database
// connectivity. It never reports a database check that did not run.
func (s *Server) handleHealth(c *gin.Context) {
	if s.db == nil {
		// No checker means no persistence is wired. That is not a healthy state,
		// so say so instead of claiming an unchecked database is fine.
		c.JSON(http.StatusServiceUnavailable, gin.H{"status": "error", "error": "database not configured"})
		return
	}

	ctx, cancel := context.WithTimeout(c.Request.Context(), 2*time.Second)
	defer cancel()

	if err := s.db.PingContext(ctx); err != nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"status": "error", "error": "database unreachable"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"status": "ok", "database": "ok"})
}

// observeRequests logs every completed request and records it in the metrics
// adapter. It uses a deferred completion block so a panic unwinding through the
// handler still produces a log line and a metric, then re-panics for
// gin.Recovery to write the 500 response.
func (s *Server) observeRequests() gin.HandlerFunc {
	return func(c *gin.Context) {
		start := time.Now()

		var recovered any
		panicked := false
		defer func() {
			if rec := recover(); rec != nil {
				recovered = rec
				panicked = true
			}

			status := c.Writer.Status()
			if panicked {
				// gin.Recovery has not written the response yet; record the
				// status it is about to produce instead of a misleading 200.
				status = http.StatusInternalServerError
			}

			// FullPath is the registered pattern (/tasks/:id), not the concrete
			// URL, so identifiers never become metric labels. Requests that
			// matched no route share one bounded label instead of one per path.
			route := c.FullPath()
			if route == "" {
				route = metrics.UnmatchedRoute
			}
			duration := time.Since(start)

			// The scrape endpoint is logged but never instrumented, so observing
			// metrics cannot recursively observe itself.
			if s.instr != nil && route != metrics.Path {
				s.instr.ObserveRequest(c.Request.Method, route, status, duration)
			}

			if s.log != nil {
				attrs := []slog.Attr{
					slog.String("request_id", RequestID(c)),
					slog.String("method", c.Request.Method),
					slog.String("path", c.Request.URL.Path),
					slog.String("route", route),
					slog.Int("status", status),
					slog.Duration("duration", duration),
					slog.Int("bytes", c.Writer.Size()),
				}
				if panicked {
					// The panic value and stack are programmer diagnostics; the
					// value can contain data, so only its occurrence is logged here.
					attrs = append(attrs, slog.Bool("panic", true))
				}
				s.log.LogAttrs(c.Request.Context(), slog.LevelInfo, "request", attrs...)
			}

			if panicked {
				panic(recovered)
			}
		}()

		c.Next()
	}
}
