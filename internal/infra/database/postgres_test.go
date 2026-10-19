package database

import "testing"

func TestNewRequiresDSN(t *testing.T) {
	_, err := New(Config{})
	if err == nil {
		t.Fatal("New() with empty DSN: want error, got nil")
	}
}

func TestNewRejectsMalformedDSN(t *testing.T) {
	_, err := New(Config{DSN: "not a valid dsn ://"})
	if err == nil {
		t.Fatal("New() with malformed DSN: want error, got nil")
	}
}

func TestNewAcceptsWellFormedDSNWithoutDialing(t *testing.T) {
	// sql.Open only validates and registers the driver; it never dials the
	// network, so this stays a fast, offline unit test even though the
	// connection string points at a host that may not exist.
	db, err := New(Config{
		DSN:          "postgres://user:pass@localhost:5432/task_manager?sslmode=disable",
		MaxOpenConns: 5,
		MaxIdleConns: 2,
	})
	if err != nil {
		t.Fatalf("New() with valid DSN: unexpected error: %v", err)
	}
	defer db.Close()

	if got := db.Stats().MaxOpenConnections; got != 5 {
		t.Fatalf("MaxOpenConnections = %d, want 5", got)
	}
}
