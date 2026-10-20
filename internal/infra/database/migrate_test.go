package database

import (
	"context"
	"strings"
	"testing"
)

func TestLoadMigrationsOrdersVersions(t *testing.T) {
	migrations, err := loadMigrations()
	if err != nil {
		t.Fatalf("loadMigrations() unexpected error: %v", err)
	}
	if len(migrations) < 2 {
		t.Fatalf("len(migrations) = %d, want at least 2", len(migrations))
	}

	for i, m := range migrations {
		if want := migrationFilePattern.FindStringSubmatch(m.name)[1]; want != m.version {
			t.Fatalf("migration %d version = %q, want %q from %q", i, m.version, want, m.name)
		}
		if i > 0 && migrations[i-1].version >= m.version {
			t.Fatalf("migrations are not in ascending version order: %q then %q",
				migrations[i-1].version, m.version)
		}
	}

	if migrations[0].version != "0001" {
		t.Fatalf("first migration version = %q, want 0001", migrations[0].version)
	}
	if !strings.Contains(migrations[0].sql, "CREATE TABLE IF NOT EXISTS tasks") {
		t.Fatalf("migration %q does not create the tasks table", migrations[0].name)
	}
	if !strings.Contains(migrations[1].sql, "CREATE INDEX IF NOT EXISTS") {
		t.Fatalf("migration %q does not create an index", migrations[1].name)
	}
}

// TestMigrationsAreSingleStatements documents the constraint the runner relies
// on: one statement per file. The pgx stdlib driver is configured for the
// extended protocol, where a multi-statement string is rejected.
func TestMigrationsAreSingleStatements(t *testing.T) {
	migrations, err := loadMigrations()
	if err != nil {
		t.Fatalf("loadMigrations() unexpected error: %v", err)
	}

	for _, m := range migrations {
		sql := stripLineComments(m.sql)
		trimmed := strings.TrimSpace(sql)
		if !strings.HasSuffix(trimmed, ";") {
			t.Fatalf("migration %q does not end with a semicolon", m.name)
		}
		if count := strings.Count(trimmed, ";"); count != 1 {
			t.Fatalf("migration %q contains %d statements, want exactly 1", m.name, count)
		}
	}
}

func TestLoadMigrationsRejectsUnknownNames(t *testing.T) {
	for _, name := range []string{
		"create_tasks.sql",
		"1_create_tasks.sql",
		"0001-Create-Tasks.sql",
		"0001_create_tasks.SQL",
	} {
		if migrationFilePattern.MatchString(name) {
			t.Errorf("pattern accepts %q, want rejection", name)
		}
	}
}

func TestMigrateRequiresPool(t *testing.T) {
	if err := Migrate(context.Background(), nil); err == nil {
		t.Fatal("Migrate() with a nil pool: want error, got nil")
	}
}

// stripLineComments removes `--` comments so statement counting is not fooled
// by punctuation in a comment.
func stripLineComments(sql string) string {
	lines := strings.Split(sql, "\n")
	for i, line := range lines {
		if idx := strings.Index(line, "--"); idx >= 0 {
			lines[i] = line[:idx]
		}
	}
	return strings.Join(lines, "\n")
}
