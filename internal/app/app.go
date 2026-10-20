// Package app is the composition root: the only place that is allowed to
// know about every concrete adapter (config, database, task repository, HTTP
// server) and wire them together. No other package should import all of these
// at once.
package app

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/aarashgoodarzi/task-manager/internal/infra/config"
	"github.com/aarashgoodarzi/task-manager/internal/infra/database"
	"github.com/aarashgoodarzi/task-manager/internal/infra/server"
	"github.com/aarashgoodarzi/task-manager/internal/infra/swagger"
	"github.com/aarashgoodarzi/task-manager/internal/task/handler"
	"github.com/aarashgoodarzi/task-manager/internal/task/repository/postgres"
	"github.com/aarashgoodarzi/task-manager/internal/task/service"
)

// databaseStartupTimeout bounds the connectivity and schema checks performed
// during Initialize, so an unreachable database fails fast instead of hanging
// the boot path.
const databaseStartupTimeout = 5 * time.Second

// App holds every top-level dependency created during Initialize.
type App struct {
	cfg    config.Config
	log    *slog.Logger
	db     *sql.DB
	health server.HealthChecker
	server *server.Server
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

	taskRepo := postgres.New(a.db)
	taskService := service.New(taskRepo)
	taskHandler := handler.New(taskService, a.log)
	swaggerHandler := swagger.New()

	a.server = server.New(server.Config{
		Addr:              a.cfg.Server.Addr,
		ReadHeaderTimeout: a.cfg.Server.ReadHeaderTimeout,
		ReadTimeout:       a.cfg.Server.ReadTimeout,
		WriteTimeout:      a.cfg.Server.WriteTimeout,
		IdleTimeout:       a.cfg.Server.IdleTimeout,
	}, a.log, a.health, taskHandler, swaggerHandler)

	a.log.Info("task-manager initialized", slog.String("addr", a.cfg.Server.Addr))
	return nil
}

// Start runs the HTTP server until an interrupt/termination signal arrives,
// then shuts everything down in reverse order of construction.
func (a *App) Start() error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	runErr := a.server.Run(ctx)

	if a.db != nil {
		if closeErr := a.db.Close(); closeErr != nil {
			a.log.Error("error closing database", slog.Any("error", closeErr))
		}
	}

	return runErr
}
