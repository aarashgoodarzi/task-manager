package config

import (
	"testing"
	"time"
)

// clearConfigEnv removes ambient configuration so tests that assert defaults
// are not affected by variables exported by the developer's shell or runner.
// Load treats an empty value as unset, which is the production behavior.
func clearConfigEnv(t *testing.T) {
	t.Helper()

	for _, key := range []string{
		"PORT",
		"SERVER_READ_HEADER_TIMEOUT",
		"SERVER_READ_TIMEOUT",
		"SERVER_WRITE_TIMEOUT",
		"SERVER_IDLE_TIMEOUT",
		"DATABASE_URL",
		"DATABASE_MAX_OPEN_CONNS",
		"DATABASE_MAX_IDLE_CONNS",
		"DATABASE_CONN_MAX_LIFETIME",
		"REDIS_URL",
		"REDIS_CACHE_TTL",
		"LOG_LEVEL",
		"METRICS_TASKS_REFRESH_INTERVAL",
	} {
		t.Setenv(key, "")
	}
}

func TestLoadDefaults(t *testing.T) {
	clearConfigEnv(t)

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
	if cfg.Redis.URL != "" {
		t.Fatalf("Redis.URL = %q, want empty default (cache disabled)", cfg.Redis.URL)
	}
	if cfg.Redis.ListTTL != 60*time.Second {
		t.Fatalf("Redis.ListTTL = %v, want 60s", cfg.Redis.ListTTL)
	}
	if cfg.Logger.Level != "info" {
		t.Fatalf("Logger.Level = %q, want %q", cfg.Logger.Level, "info")
	}
	if cfg.Metrics.TasksRefreshInterval != 15*time.Second {
		t.Fatalf("Metrics.TasksRefreshInterval = %v, want 15s", cfg.Metrics.TasksRefreshInterval)
	}
}

func TestLoadReadsEnvOverrides(t *testing.T) {
	clearConfigEnv(t)

	t.Setenv("PORT", "9090")
	t.Setenv("DATABASE_URL", "postgres://user:pass@localhost:5432/task_manager?sslmode=disable")
	t.Setenv("DATABASE_MAX_OPEN_CONNS", "25")
	t.Setenv("SERVER_READ_TIMEOUT", "2s")
	t.Setenv("LOG_LEVEL", "debug")
	t.Setenv("METRICS_TASKS_REFRESH_INTERVAL", "30s")
	t.Setenv("REDIS_URL", "redis://localhost:6379/3")
	t.Setenv("REDIS_CACHE_TTL", "2m")

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
	if cfg.Redis.URL != "redis://localhost:6379/3" {
		t.Fatalf("Redis.URL = %q", cfg.Redis.URL)
	}
	if cfg.Redis.ListTTL != 2*time.Minute {
		t.Fatalf("Redis.ListTTL = %v, want 2m", cfg.Redis.ListTTL)
	}
	if cfg.Logger.Level != "debug" {
		t.Fatalf("Logger.Level = %q, want %q", cfg.Logger.Level, "debug")
	}
	if cfg.Metrics.TasksRefreshInterval != 30*time.Second {
		t.Fatalf("Metrics.TasksRefreshInterval = %v, want 30s", cfg.Metrics.TasksRefreshInterval)
	}
}

func TestLoadIgnoresInvalidIntOverride(t *testing.T) {
	clearConfigEnv(t)

	t.Setenv("DATABASE_MAX_OPEN_CONNS", "not-a-number")

	cfg := Load()

	if cfg.Database.MaxOpenConns != 10 {
		t.Fatalf("Database.MaxOpenConns = %d, want fallback 10", cfg.Database.MaxOpenConns)
	}
}

// TestLoadRejectsNonPositiveCacheTTL keeps a finite expiry in place; the
// adapter rejects a non-positive TTL and caching would silently disable.
func TestLoadRejectsNonPositiveCacheTTL(t *testing.T) {
	for _, value := range []string{"0s", "-5s", "not-a-duration"} {
		t.Run(value, func(t *testing.T) {
			clearConfigEnv(t)
			t.Setenv("REDIS_CACHE_TTL", value)

			cfg := Load()

			if cfg.Redis.ListTTL != 60*time.Second {
				t.Fatalf("Redis.ListTTL = %v, want the 60s fallback", cfg.Redis.ListTTL)
			}
		})
	}
}

// TestLoadRejectsNonPositiveRefreshInterval proves a bad value cannot reach the
// refresher, where a non-positive ticker interval would panic.
func TestLoadRejectsNonPositiveRefreshInterval(t *testing.T) {
	for _, value := range []string{"0s", "-5s", "not-a-duration"} {
		t.Run(value, func(t *testing.T) {
			clearConfigEnv(t)
			t.Setenv("METRICS_TASKS_REFRESH_INTERVAL", value)

			cfg := Load()

			if cfg.Metrics.TasksRefreshInterval != 15*time.Second {
				t.Fatalf("Metrics.TasksRefreshInterval = %v, want the 15s fallback", cfg.Metrics.TasksRefreshInterval)
			}
		})
	}
}
