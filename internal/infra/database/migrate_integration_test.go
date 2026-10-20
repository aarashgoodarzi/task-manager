package database

import (
	"context"
	"database/sql"
	"os"
	"strings"
	"testing"
	"time"
)

// TestMigrateIsIdempotent runs against a real PostgreSQL instance named by
// TEST_DATABASE_URL. Without it the test skips instead of pretending to pass.
func TestMigrateIsIdempotent(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL is not set; skipping PostgreSQL integration tests")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	db, err := New(Config{DSN: dsn, MaxOpenConns: 5, MaxIdleConns: 2, ConnMaxLifetime: time.Minute})
	if err != nil {
		t.Fatalf("open test database: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	if err := db.PingContext(ctx); err != nil {
		t.Fatalf("ping test database: %v", err)
	}

	var name string
	if err := db.QueryRowContext(ctx, "SELECT current_database()").Scan(&name); err != nil {
		t.Fatalf("read current_database(): %v", err)
	}
	if !strings.Contains(name, "test") {
		t.Fatalf("refusing to migrate database %q: the name must contain \"test\"", name)
	}

	migrations, err := loadMigrations()
	if err != nil {
		t.Fatalf("loadMigrations() unexpected error: %v", err)
	}

	for run := 1; run <= 2; run++ {
		if err := Migrate(ctx, db); err != nil {
			t.Fatalf("Migrate() run %d: %v", run, err)
		}
	}

	var applied int
	if err := db.QueryRowContext(ctx, "SELECT count(*) FROM schema_migrations").Scan(&applied); err != nil {
		t.Fatalf("count applied migrations: %v", err)
	}
	if applied != len(migrations) {
		t.Fatalf("applied migrations = %d, want %d after two runs", applied, len(migrations))
	}

	assertSchemaExists(t, db, "table", "tasks")
	assertSchemaExists(t, db, "index", "tasks_status_idx")
}

func assertSchemaExists(t *testing.T, db *sql.DB, kind, name string) {
	t.Helper()

	var exists bool
	err := db.QueryRowContext(context.Background(),
		`SELECT EXISTS (
		     SELECT 1 FROM pg_catalog.pg_class c
		     JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
		     WHERE c.relname = $1 AND c.relkind = $2 AND n.nspname = current_schema()
		 )`, name, map[string]string{"table": "r", "index": "i"}[kind],
	).Scan(&exists)
	if err != nil {
		t.Fatalf("check %s %q: %v", kind, name, err)
	}
	if !exists {
		t.Fatalf("%s %q does not exist in the current schema", kind, name)
	}
}
