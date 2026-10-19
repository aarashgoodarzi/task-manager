// Package app is the composition root: the only place that is allowed to
// know about every concrete adapter (config, database, HTTP server) and
// wire them together. No other package should import all of these at once.
package app

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/aarashgoodarzi/task-manager/internal/infra/config"
	"github.com/aarashgoodarzi/task-manager/internal/infra/database"
	"github.com/aarashgoodarzi/task-manager/internal/infra/server"
)

// App holds every top-level dependency created during Initialize.
type App struct {
	cfg    config.Config
	log    *slog.Logger
	db     *sql.DB
	server *server.Server
}

// New returns an unwired App. Call Initialize before Start.
func New() *App {
	return &App{}
}

// Initialize loads configuration and constructs every adapter: logger,
// database connection pool, and HTTP server. It fails fast if a required
// setting is missing so misconfiguration is caught at startup, not on the
// first request.
func (a *App) Initialize() error {
	a.cfg = config.Load()
	a.log = newLogger(a.cfg.Logger)

	db, err := database.New(database.Config{
		DSN:             a.cfg.Database.DSN,
		MaxOpenConns:    a.cfg.Database.MaxOpenConns,
		MaxIdleConns:    a.cfg.Database.MaxIdleConns,
		ConnMaxLifetime: a.cfg.Database.ConnMaxLifetime,
	})
	if err != nil {
		return fmt.Errorf("app: initialize database: %w", err)
	}
	a.db = db

	a.server = server.New(server.Config{
		Addr:              a.cfg.Server.Addr,
		ReadHeaderTimeout: a.cfg.Server.ReadHeaderTimeout,
		ReadTimeout:       a.cfg.Server.ReadTimeout,
		WriteTimeout:      a.cfg.Server.WriteTimeout,
		IdleTimeout:       a.cfg.Server.IdleTimeout,
	}, a.log, a.db)

	a.log.Info("task-manager initialized", slog.String("addr", a.cfg.Server.Addr))
	return nil
}

// Start runs the HTTP server until an interrupt/termination signal arrives,
// then shuts everything down in reverse order of construction.
func (a *App) Start() error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	runErr := a.server.Run(ctx)

	if closeErr := a.db.Close(); closeErr != nil {
		a.log.Error("error closing database", slog.Any("error", closeErr))
	}

	return runErr
}
