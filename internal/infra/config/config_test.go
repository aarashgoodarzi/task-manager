package config

import (
	"testing"
	"time"
)

func TestLoadDefaults(t *testing.T) {
	cfg := Load()

	if cfg.Server.Addr != ":8080" {
		t.Fatalf("Server.Addr = %q, want %q", cfg.Server.Addr, ":8080")
	}
	if cfg.Database.DSN != "" {
		t.Fatalf("Database.DSN = %q, want empty default", cfg.Database.DSN)
	}
	if cfg.Database.MaxOpenConns != 10 {
		t.Fatalf("Database.MaxOpenConns = %d, want 10", cfg.Database.MaxOpenConns)
	}
	if cfg.Logger.Level != "info" {
		t.Fatalf("Logger.Level = %q, want %q", cfg.Logger.Level, "info")
	}
}

func TestLoadReadsEnvOverrides(t *testing.T) {
	t.Setenv("PORT", "9090")
	t.Setenv("DATABASE_URL", "postgres://user:pass@localhost:5432/task_manager?sslmode=disable")
	t.Setenv("DATABASE_MAX_OPEN_CONNS", "25")
	t.Setenv("SERVER_READ_TIMEOUT", "2s")
	t.Setenv("LOG_LEVEL", "debug")

	cfg := Load()

	if cfg.Server.Addr != ":9090" {
		t.Fatalf("Server.Addr = %q, want %q", cfg.Server.Addr, ":9090")
	}
	if cfg.Database.DSN != "postgres://user:pass@localhost:5432/task_manager?sslmode=disable" {
		t.Fatalf("Database.DSN = %q", cfg.Database.DSN)
	}
	if cfg.Database.MaxOpenConns != 25 {
		t.Fatalf("Database.MaxOpenConns = %d, want 25", cfg.Database.MaxOpenConns)
	}
	if cfg.Server.ReadTimeout != 2*time.Second {
		t.Fatalf("Server.ReadTimeout = %v, want 2s", cfg.Server.ReadTimeout)
	}
	if cfg.Logger.Level != "debug" {
		t.Fatalf("Logger.Level = %q, want %q", cfg.Logger.Level, "debug")
	}
}

func TestLoadIgnoresInvalidIntOverride(t *testing.T) {
	t.Setenv("DATABASE_MAX_OPEN_CONNS", "not-a-number")

	cfg := Load()

	if cfg.Database.MaxOpenConns != 10 {
		t.Fatalf("Database.MaxOpenConns = %d, want fallback 10", cfg.Database.MaxOpenConns)
	}
}
