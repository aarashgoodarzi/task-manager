// Package app is the composition root: the only place that is allowed to
// know about every concrete adapter (config, database, task repository, HTTP
// server) and wire them together. No other package should import all of these
// at once.
package app

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/aarashgoodarzi/task-manager/internal/infra/config"
	"github.com/aarashgoodarzi/task-manager/internal/infra/database"
	infraMetrics "github.com/aarashgoodarzi/task-manager/internal/infra/metrics"
	"github.com/aarashgoodarzi/task-manager/internal/infra/server"
	taskRedis "github.com/aarashgoodarzi/task-manager/internal/task/cache/redis"
	"github.com/aarashgoodarzi/task-manager/internal/task/repository/postgres"
	"github.com/aarashgoodarzi/task-manager/internal/task/service"
)

// databaseStartupTimeout bounds the connectivity and schema checks performed
// during Initialize, so an unreachable database fails fast instead of hanging
// the boot path.
const databaseStartupTimeout = 5 * time.Second

// cacheStartupTimeout bounds the best-effort generation reset performed when a
// Redis cache is configured. Redis is optional, so this never fails startup.
const cacheStartupTimeout = 2 * time.Second

// profilerShutdownTimeout bounds how long a graceful stop waits for an
// in-flight pprof capture. A CPU profile streams for its requested duration,
// so shutdown does not wait indefinitely for one to finish.
const profilerShutdownTimeout = 5 * time.Second

// App holds every top-level dependency created during Initialize.
type App struct {
	cfg         config.Config
	log         *slog.Logger
	db          *sql.DB
	health      server.HealthChecker
	metrics     *infraMetrics.Metrics
	taskCount   *infraMetrics.TaskCountRefresher
	cache       service.ListCache
	cacheCloser io.Closer
	profiler    *http.Server
	profileLn   net.Listener
	server      *server.Server
}

// New returns an unwired App. Call Initialize before Start.
func New() *App {
	return &App{}
}

// Initialize loads configuration and constructs every adapter: logger, the
// PostgreSQL pool, the task repository and service, and the HTTP server.
//
// PostgreSQL is required. A missing DATABASE_URL, an unreachable server, or an
// uninitialized schema stops startup with an error; there is no in-memory
// fallback, so a request is never served from storage that would disappear on
// restart.
func (a *App) Initialize() error {
	a.cfg = config.Load()
	a.log = newLogger(a.cfg.Logger)

	if a.cfg.Database.DSN == "" {
		return fmt.Errorf("app: initialize database: DATABASE_URL is required")
	}

	db, err := database.New(database.Config{
		DSN:             a.cfg.Database.DSN,
		MaxOpenConns:    a.cfg.Database.MaxOpenConns,
		MaxIdleConns:    a.cfg.Database.MaxIdleConns,
		ConnMaxLifetime: a.cfg.Database.ConnMaxLifetime,
	})
	if err != nil {
		return fmt.Errorf("app: initialize database: %w", err)
	}

	// sql.Open is lazy, so reachability and schema state are checked explicitly
	// here. Both are bounded, and a failure closes the pool instead of leaving a
	// half-initialized process behind.
	ctx, cancel := context.WithTimeout(context.Background(), databaseStartupTimeout)
	defer cancel()

	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return fmt.Errorf("app: initialize database: ping: %w", err)
	}
	if err := database.VerifySchema(ctx, db); err != nil {
		_ = db.Close()
		return fmt.Errorf("app: initialize database: %w", err)
	}

	a.db = db
	a.health = db

	// The pool is wrapped once and shared by the task module and the metrics
	// refresher; both consume it through the narrow ports they declare.
	taskRepo := postgres.New(a.db)

	// Redis is an optional accelerator, never the source of truth. Constructing
	// the adapter does not dial, so a missing or unreachable server leaves the
	// cache disabled instead of stopping the API. The startup invalidation
	// closes the restart window: a process that died after a committed write
	// cannot leave an old generation readable, and a failure here is logged but
	// non-fatal (the adapter degrades to database reads).
	if a.cfg.Redis.URL != "" {
		cache, cacheErr := taskRedis.New(taskRedis.Config{
			URL: a.cfg.Redis.URL,
			TTL: a.cfg.Redis.ListTTL,
		})
		if cacheErr != nil {
			a.log.Warn("redis cache disabled", slog.Any("error", cacheErr))
		} else {
			a.cache = cache
			a.cacheCloser = cache

			cacheCtx, cancelCache := context.WithTimeout(context.Background(), cacheStartupTimeout)
			if err := cache.Invalidate(cacheCtx); err != nil {
				a.log.Warn("redis cache unavailable at startup; serving from PostgreSQL", slog.Any("error", err))
			} else {
				a.log.Info("redis cache enabled", slog.Duration("list_ttl", a.cfg.Redis.ListTTL))
			}
			cancelCache()
		}
	}

	// Instrumentation owns its own registry, so it can be constructed per App
	// (including in tests) without touching the global Prometheus registry.
	a.metrics = infraMetrics.New()
	a.taskCount, err = infraMetrics.NewTaskCountRefresher(
		taskRepo, a.metrics, a.cfg.Metrics.TasksRefreshInterval, a.log,
	)
	if err != nil {
		_ = db.Close()
		return fmt.Errorf("app: initialize metrics: %w", err)
	}
	// Profiling is opt-in and lives on its own loopback listener. It is bound
	// before the API server so a rejected address fails startup, and it is
	// never part of the Gin route table.
	if a.cfg.Profile.Addr != "" {
		profiler, listener, profileErr := newProfilerServer(a.cfg.Profile.Addr, a.log)
		if profileErr != nil {
			_ = db.Close()
			return fmt.Errorf("app: initialize profiler: %w", profileErr)
		}
		a.profiler = profiler
		a.profileLn = listener
	}

	// HTTP modules contribute routes through buildHandlers, so Initialize does
	// not grow an argument per module when a feature is added.
	a.server = server.New(server.Config{
		Addr:              a.cfg.Server.Addr,
		ReadHeaderTimeout: a.cfg.Server.ReadHeaderTimeout,
		ReadTimeout:       a.cfg.Server.ReadTimeout,
		WriteTimeout:      a.cfg.Server.WriteTimeout,
		IdleTimeout:       a.cfg.Server.IdleTimeout,
	}, a.log, a.health, a.metrics, a.buildHandlers(taskRepo)...)

	a.log.Info("task-manager initialized", slog.String("addr", a.cfg.Server.Addr))
	return nil
}

// Start runs the HTTP server and the task-count refresher until an
// interrupt/termination signal arrives, then shuts everything down in reverse
// order of construction.
func (a *App) Start() error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	refresherCtx, stopRefresher := context.WithCancel(ctx)
	refresherDone := make(chan struct{})
	if a.taskCount != nil {
		go func() {
			defer close(refresherDone)
			a.taskCount.Run(refresherCtx)
		}()
	}

	// The profiling server has no background work of its own; it is started
	// alongside the API server and stopped as soon as the API server returns.
	profileDone := make(chan struct{})
	if a.profiler != nil {
		go func() {
			defer close(profileDone)
			if serveErr := a.profiler.Serve(a.profileLn); serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
				a.log.Error("pprof listener failed", slog.Any("error", serveErr))
			}
		}()
	}

	runErr := a.server.Run(ctx)

	// Stop profiling before tearing down the dependencies a capture may still
	// be reading, and wait for the listener goroutine so shutdown leaves no
	// stray goroutine behind.
	if a.profiler != nil {
		shutdownCtx, cancelProfile := context.WithTimeout(context.Background(), profilerShutdownTimeout)
		if shutdownErr := a.profiler.Shutdown(shutdownCtx); shutdownErr != nil {
			a.log.Warn("pprof listener did not stop cleanly", slog.Any("error", shutdownErr))
		}
		cancelProfile()
		<-profileDone
	}

	// Cancel the refresher before closing the pool and wait for it, so an
	// in-flight count query returns a cancellation error instead of racing a
	// closed database handle.
	stopRefresher()
	if a.taskCount != nil {
		<-refresherDone
	}

	// The cache has no background work; closing it before the pool keeps the
	// shutdown order easy to reason about without changing the database rule.
	if a.cacheCloser != nil {
		if closeErr := a.cacheCloser.Close(); closeErr != nil {
			a.log.Error("error closing redis cache", slog.Any("error", closeErr))
		}
	}

	if a.db != nil {
		if closeErr := a.db.Close(); closeErr != nil {
			a.log.Error("error closing database", slog.Any("error", closeErr))
		}
	}

	return runErr
}
