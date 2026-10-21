package database

import (
	"context"
	"database/sql"
	"fmt"
)

// requiredTables is the schema the service cannot run without. It is owned by
// this package because this package owns the migrations that create it.
var requiredTables = []string{"tasks"}

// requiredColumns lists columns added by later migrations that the current
// binary reads. Checking them keeps a half-migrated database from starting and
// then failing every request with a 500.
var requiredColumns = []struct {
	table  string
	column string
}{
	{table: "tasks", column: "assignee"},
}

// SchemaQuerier is the subset of *sql.DB and *sql.Conn that VerifySchema needs,
// so a caller can check a specific pinned connection in tests.
type SchemaQuerier interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// VerifySchema returns an error naming the first missing table. Startup calls
// it after a successful ping so the process fails closed with an actionable
// message instead of accepting requests that can only answer 500.
func VerifySchema(ctx context.Context, q SchemaQuerier) error {
	if q == nil {
		return fmt.Errorf("database: verify schema: no connection")
	}

	for _, table := range requiredTables {
		var present bool
		if err := q.QueryRowContext(ctx, "SELECT to_regclass($1) IS NOT NULL", table).Scan(&present); err != nil {
			return fmt.Errorf("database: verify schema: %w", err)
		}
		if !present {
			return fmt.Errorf("database: schema is not initialized: table %q is missing; run `make migrate`", table)
		}
	}

	for _, required := range requiredColumns {
		var present bool
		if err := q.QueryRowContext(ctx,
			`SELECT count(*) > 0 FROM information_schema.columns
			 WHERE table_schema = current_schema() AND table_name = $1 AND column_name = $2`,
			required.table, required.column,
		).Scan(&present); err != nil {
			return fmt.Errorf("database: verify schema: %w", err)
		}
		if !present {
			return fmt.Errorf(
				"database: schema is out of date: column %q on table %q is missing; run `make migrate`",
				required.column, required.table)
		}
	}
	return nil
}
