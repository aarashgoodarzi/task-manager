// Package config loads runtime settings for the task-manager service from
// environment variables. It has no knowledge of HTTP, SQL, or any other
// infrastructure concern — it only produces plain values that other
// packages use to construct themselves.
package config

import (
	"os"
	"strconv"
	"time"
)

// Config holds every runtime setting the service needs to start.
type Config struct {
	Server   ServerConfig
	Database DatabaseConfig
	Redis    RedisConfig
	Logger   LoggerConfig
	Metrics  MetricsConfig
}

// ServerConfig drives the HTTP server listener and timeouts.
type ServerConfig struct {
	Addr              string
	ReadHeaderTimeout time.Duration
	ReadTimeout       time.Duration
	WriteTimeout      time.Duration
	IdleTimeout       time.Duration
}

// DatabaseConfig drives the PostgreSQL connection pool.
type DatabaseConfig struct {
	DSN             string
	MaxOpenConns    int
	MaxIdleConns    int
	ConnMaxLifetime time.Duration
}

// LoggerConfig drives structured logging (stdlib slog).
type LoggerConfig struct {
	Level string
}

// RedisConfig drives the optional task-list cache. An empty URL disables the
// cache; the service keeps serving from PostgreSQL.
type RedisConfig struct {
	// URL is a go-redis connection URL. Empty disables caching.
	URL string
	// ListTTL bounds how long a cached task list may be served. It is finite
	// so a missed invalidation cannot persist indefinitely.
	ListTTL time.Duration
}

// defaultRedisListTTL is deliberately short relative to the task mutation
// rate: it bounds staleness if a process dies between a committed write and
// invalidation. The generation token handles the normal case immediately.
const defaultRedisListTTL = 60 * time.Second

// MetricsConfig drives the Prometheus instrumentation refresh loop.
type MetricsConfig struct {
	// TasksRefreshInterval is how often tasks_count is re-read from
	// PostgreSQL. It bounds how stale the gauge can be; it is never read per
	// request.
	TasksRefreshInterval time.Duration
}

// defaultTasksRefreshInterval paces the tasks_count refresh. It is short
// enough that a dashboard is useful and long enough that a full-table count is
// negligible for this service.
const defaultTasksRefreshInterval = 15 * time.Second

// Load reads environment variables and applies defaults for anything unset.
// It performs no validation that requires other packages (e.g. it does not
// dial the database); callers decide how to react to empty required values.
func Load() Config {
	tasksRefreshInterval := envDuration("METRICS_TASKS_REFRESH_INTERVAL", defaultTasksRefreshInterval)
	if tasksRefreshInterval <= 0 {
		// A non-positive interval would make the refresher's ticker panic.
		tasksRefreshInterval = defaultTasksRefreshInterval
	}

	listTTL := envDuration("REDIS_CACHE_TTL", defaultRedisListTTL)
	if listTTL <= 0 {
		// A non-positive TTL would either disable expiry or be rejected by
		// the adapter; keep the finite default.
		listTTL = defaultRedisListTTL
	}

	return Config{
		Server: ServerConfig{
			Addr:              ":" + env("PORT", "8080"),
			ReadHeaderTimeout: envDuration("SERVER_READ_HEADER_TIMEOUT", 5*time.Second),
			ReadTimeout:       envDuration("SERVER_READ_TIMEOUT", 15*time.Second),
			WriteTimeout:      envDuration("SERVER_WRITE_TIMEOUT", 15*time.Second),
			IdleTimeout:       envDuration("SERVER_IDLE_TIMEOUT", 60*time.Second),
		},
		Database: DatabaseConfig{
			DSN:             env("DATABASE_URL", ""),
			MaxOpenConns:    envInt("DATABASE_MAX_OPEN_CONNS", 10),
			MaxIdleConns:    envInt("DATABASE_MAX_IDLE_CONNS", 5),
			ConnMaxLifetime: envDuration("DATABASE_CONN_MAX_LIFETIME", 30*time.Minute),
		},
		Redis: RedisConfig{
			URL:     env("REDIS_URL", ""),
			ListTTL: listTTL,
		},
		Logger: LoggerConfig{
			Level: env("LOG_LEVEL", "info"),
		},
		Metrics: MetricsConfig{
			TasksRefreshInterval: tasksRefreshInterval,
		},
	}
}

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func envInt(key string, fallback int) int {
	v := os.Getenv(key)
	if v == "" {
		return fallback
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return fallback
	}
	return n
}

func envDuration(key string, fallback time.Duration) time.Duration {
	v := os.Getenv(key)
	if v == "" {
		return fallback
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return fallback
	}
	return d
}
