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
	Logger   LoggerConfig
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

// Load reads environment variables and applies defaults for anything unset.
// It performs no validation that requires other packages (e.g. it does not
// dial the database); callers decide how to react to empty required values.
func Load() Config {
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
		Logger: LoggerConfig{
			Level: env("LOG_LEVEL", "info"),
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
