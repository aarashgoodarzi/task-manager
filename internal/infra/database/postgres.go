// Package database is the PostgreSQL adapter. It is the only package in
// this service allowed to know about SQL drivers or connection strings.
// It exposes a plain *sql.DB so that feature repositories (added later,
// e.g. internal/task/repository) can depend on the standard library
// database/sql API instead of on this package directly.
package database

import (
	"database/sql"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	_ "github.com/jackc/pgx/v5/stdlib" // registers the "pgx" database/sql driver
)

// Config holds the PostgreSQL connection pool settings.
type Config struct {
	DSN             string
	MaxOpenConns    int
	MaxIdleConns    int
	ConnMaxLifetime time.Duration
}

// New validates the DSN and opens a pooled PostgreSQL connection.
//
// It deliberately does not run migrations, ping the server, or execute any
// domain query — that keeps this function fast, side-effect-free beyond the
// pool handle, and unit-testable without a live database.
func New(cfg Config) (*sql.DB, error) {
	if cfg.DSN == "" {
		return nil, fmt.Errorf("database: DSN is required")
	}
	if _, err := pgx.ParseConfig(cfg.DSN); err != nil {
		return nil, fmt.Errorf("database: invalid DSN: %w", err)
	}

	db, err := sql.Open("pgx", cfg.DSN)
	if err != nil {
		return nil, fmt.Errorf("database: open: %w", err)
	}

	db.SetMaxOpenConns(cfg.MaxOpenConns)
	db.SetMaxIdleConns(cfg.MaxIdleConns)
	db.SetConnMaxLifetime(cfg.ConnMaxLifetime)

	return db, nil
}
