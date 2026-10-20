package app

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/aarashgoodarzi/task-manager/internal/infra/database"
)

// unreachableDSN points at a port nothing listens on, so the startup ping fails
// fast without needing a database.
const unreachableDSN = "postgres://user:pass@127.0.0.1:1/task_manager?sslmode=disable&connect_timeout=1"

func TestInitializeRequiresDatabaseURL(t *testing.T) {
	t.Setenv("DATABASE_URL", "")
	t.Setenv("LOG_LEVEL", "error")

	a := New()
	err := a.Initialize()
	if err == nil {
		t.Fatal("Initialize() without DATABASE_URL: want error, got nil")
	}
	if !strings.Contains(err.Error(), "DATABASE_URL is required") {
		t.Fatalf("error = %v, want it to name the missing setting", err)
	}
	if a.db != nil || a.health != nil || a.server != nil {
		t.Fatal("app was partially initialized without a database")
	}
}

func TestInitializeFailsOnMalformedDSN(t *testing.T) {
	t.Setenv("DATABASE_URL", "not a valid dsn ://")
	t.Setenv("LOG_LEVEL", "error")

	a := New()
	err := a.Initialize()
	if err == nil {
		t.Fatal("Initialize() with a malformed DSN: want error, got nil")
	}
	if !strings.Contains(err.Error(), "app: initialize database") {
		t.Fatalf("error = %v, want it to name the failing stage", err)
	}
	if a.db != nil || a.server != nil {
		t.Fatal("app was partially initialized after a failed initialization")
	}
}

func TestInitializeFailsWhenDatabaseIsUnreachable(t *testing.T) {
	t.Setenv("DATABASE_URL", unreachableDSN)
	t.Setenv("LOG_LEVEL", "error")

	a := New()
	err := a.Initialize()
	if err == nil {
		t.Fatal("Initialize() with an unreachable database: want error, got nil")
	}
	if !strings.Contains(err.Error(), "app: initialize database: ping") {
		t.Fatalf("error = %v, want it to name the failed ping", err)
	}
	if a.db != nil || a.health != nil || a.server != nil {
		t.Fatal("app was partially initialized after an unreachable database")
	}
}

// TestInitializeFailsWhenSchemaIsMissing proves startup fails closed instead of
// serving requests that could only answer 500. It points the pool at a schema
// that does not exist, so the connection is healthy but the table is not.
func TestInitializeFailsWhenSchemaIsMissing(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL is not set; skipping PostgreSQL integration test")
	}

	t.Setenv("DATABASE_URL", dsn+"&search_path=definitely_missing_schema")
	t.Setenv("LOG_LEVEL", "error")

	a := New()
	err := a.Initialize()
	if err == nil {
		t.Fatal("Initialize() without a schema: want error, got nil")
	}
	if !strings.Contains(err.Error(), "schema is not initialized") {
		t.Fatalf("error = %v, want it to explain that the schema is missing", err)
	}
	if !strings.Contains(err.Error(), "make migrate") {
		t.Fatalf("error = %v, want an actionable migration hint", err)
	}
	if a.db != nil || a.server != nil {
		t.Fatal("app was partially initialized without a schema")
	}
}

func TestInitializeWiresPostgreSQL(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL is not set; skipping PostgreSQL integration test")
	}

	// Bring the schema up to date the same way a deploy does.
	migrateDB, err := database.New(database.Config{DSN: dsn, MaxOpenConns: 2, MaxIdleConns: 1})
	if err != nil {
		t.Fatalf("open test database: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	if err := database.Migrate(ctx, migrateDB); err != nil {
		cancel()
		_ = migrateDB.Close()
		t.Fatalf("migrate test database: %v", err)
	}
	cancel()
	if err := migrateDB.Close(); err != nil {
		t.Fatalf("close migration pool: %v", err)
	}

	t.Setenv("DATABASE_URL", dsn)
	t.Setenv("LOG_LEVEL", "error")

	a := New()
	if err := a.Initialize(); err != nil {
		t.Fatalf("Initialize() unexpected error: %v", err)
	}
	t.Cleanup(func() {
		if a.db != nil {
			_ = a.db.Close()
		}
	})

	if a.db == nil {
		t.Fatal("db is nil, want a PostgreSQL pool")
	}
	if a.health == nil {
		t.Fatal("health checker is nil, want the pool")
	}
	if a.server == nil {
		t.Fatal("server is nil, want an initialized server")
	}
	if a.log == nil {
		t.Fatal("logger is nil")
	}
	if a.metrics == nil {
		t.Fatal("metrics is nil, want an initialized registry")
	}
	if a.taskCount == nil {
		t.Fatal("task count refresher is nil, want it wired to the task repository")
	}
}
