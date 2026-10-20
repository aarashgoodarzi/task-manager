package database

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
	"io/fs"
	"regexp"
	"sort"
	"strings"
)

// migrationFS holds the versioned schema migrations. They are embedded so the
// binary carries its own schema and never depends on a working directory or an
// external migration tool.
//
//go:embed migrations/*.sql
var migrationFS embed.FS

// migrationFilePattern enforces the naming rule that defines migration order:
// a zero-padded version, an underscore, a lowercase description, and a .sql
// suffix. Lexical order is therefore version order.
var migrationFilePattern = regexp.MustCompile(`^(\d{4})_[a-z0-9_]+\.sql$`)

// advisoryLockKey serialises concurrent migration runs (for example two
// instances starting at once). The transaction-scoped lock is released when the
// transaction commits or rolls back.
const advisoryLockKey = 4242001

const createMigrationsTable = `
CREATE TABLE IF NOT EXISTS schema_migrations (
    version    TEXT PRIMARY KEY,
    applied_at TIMESTAMPTZ NOT NULL DEFAULT now()
)`

// migration is one versioned, additive schema change.
type migration struct {
	version string
	name    string
	sql     string
}

// Migrate applies every embedded migration that has not been applied yet, in
// version order, and records each one in schema_migrations.
//
// Migrations are additive: they create tables, indexes, and constraints, and
// never drop or rewrite existing data. Each migration runs in its own
// transaction, so a failing migration leaves the schema as it was and is not
// recorded. Running Migrate again applies only what is missing.
func Migrate(ctx context.Context, db *sql.DB) error {
	if db == nil {
		return fmt.Errorf("database: migrate: nil pool")
	}

	migrations, err := loadMigrations()
	if err != nil {
		return err
	}

	if _, err := db.ExecContext(ctx, createMigrationsTable); err != nil {
		return fmt.Errorf("database: migrate: create schema_migrations: %w", err)
	}

	for _, m := range migrations {
		if err := applyMigration(ctx, db, m); err != nil {
			return err
		}
	}
	return nil
}

// loadMigrations reads, validates, and orders the embedded migration files.
// It fails loudly on a name that does not follow the version convention, so a
// misordered schema change cannot be applied silently.
func loadMigrations() ([]migration, error) {
	entries, err := fs.ReadDir(migrationFS, "migrations")
	if err != nil {
		return nil, fmt.Errorf("database: migrate: read migrations: %w", err)
	}

	migrations := make([]migration, 0, len(entries))
	seen := make(map[string]string, len(entries))
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}

		name := entry.Name()
		match := migrationFilePattern.FindStringSubmatch(name)
		if match == nil {
			return nil, fmt.Errorf("database: migrate: %q does not match NNNN_description.sql", name)
		}

		version := match[1]
		if other, ok := seen[version]; ok {
			return nil, fmt.Errorf("database: migrate: version %s is used by both %q and %q", version, other, name)
		}
		seen[version] = name

		content, err := migrationFS.ReadFile("migrations/" + name)
		if err != nil {
			return nil, fmt.Errorf("database: migrate: read %q: %w", name, err)
		}
		if strings.TrimSpace(string(content)) == "" {
			return nil, fmt.Errorf("database: migrate: %q is empty", name)
		}

		migrations = append(migrations, migration{
			version: version,
			name:    name,
			sql:     string(content),
		})
	}

	sort.Slice(migrations, func(i, j int) bool {
		return migrations[i].version < migrations[j].version
	})
	return migrations, nil
}

// applyMigration runs one migration inside a transaction guarded by an
// advisory lock, skipping versions that are already recorded.
func applyMigration(ctx context.Context, db *sql.DB, m migration) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("database: migrate: begin %s: %w", m.name, err)
	}
	defer func() {
		// Rollback after a successful commit is a no-op error; nothing to do.
		_ = tx.Rollback()
	}()

	if _, err := tx.ExecContext(ctx, "SELECT pg_advisory_xact_lock($1)", advisoryLockKey); err != nil {
		return fmt.Errorf("database: migrate: lock for %s: %w", m.name, err)
	}

	var applied bool
	err = tx.QueryRowContext(ctx,
		"SELECT EXISTS (SELECT 1 FROM schema_migrations WHERE version = $1)",
		m.version,
	).Scan(&applied)
	if err != nil {
		return fmt.Errorf("database: migrate: check %s: %w", m.name, err)
	}
	if applied {
		return tx.Commit()
	}

	if _, err := tx.ExecContext(ctx, m.sql); err != nil {
		return fmt.Errorf("database: migrate: apply %s: %w", m.name, err)
	}
	if _, err := tx.ExecContext(ctx,
		"INSERT INTO schema_migrations (version) VALUES ($1)", m.version,
	); err != nil {
		return fmt.Errorf("database: migrate: record %s: %w", m.name, err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("database: migrate: commit %s: %w", m.name, err)
	}
	return nil
}
