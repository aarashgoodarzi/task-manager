# 0006. Task persistence in PostgreSQL: table, versioned migrations, and an explicit-SQL repository

* Status: Accepted
* Date: 2026-10-20
* Scope: `internal/infra/database/` (schema and migrations),
  `internal/task/repository/postgres/`, `cmd/migrate/`, `Makefile`
* Amends: [0003](0003-postgres-via-database-sql.md) — the migration strategy
  it deferred is decided here.

## Context

This change adds the task aggregate and the repository port that persistence
must satisfy. The HTTP contract is not part of it: the use cases and handler are
added on top of the same port in the next change, so the schema is derived from
the aggregate and not from a transport. The port has a single shape:

```go
type Repository interface {
	Create(ctx context.Context, task domain.Task) (domain.Task, error)
	List(ctx context.Context) ([]domain.Task, error)
	Get(ctx context.Context, id string) (domain.Task, error)
	Update(ctx context.Context, task domain.Task) (domain.Task, error)
	Delete(ctx context.Context, id string) error
}
```

Three questions follow: what the table looks like, how the schema evolves, and
how the adapter is verified. [ADR 0003](0003-postgres-via-database-sql.md)
already fixed the driver (`database/sql` + pgx stdlib), rejected an ORM, and
rejected a shared generic `Database` port. It also deferred migrations until a
first table existed, and rejected running them from the application process.

The identifier, status, and validation rules are defined by the aggregate in
`internal/task/domain`: version 4 UUID identifiers assigned by the adapter,
`title` required after trimming, `description` optional, and a closed status set
(`todo`, `in_progress`, `done`).

## Decision

### Schema

`internal/infra/database/migrations/` holds the schema as ordered SQL files:

| Migration | Statement |
| --- | --- |
| `0001_create_tasks.sql` | `CREATE TABLE IF NOT EXISTS tasks (id UUID PRIMARY KEY, title TEXT NOT NULL CHECK (btrim(title) <> ''), description TEXT NOT NULL DEFAULT '', status TEXT NOT NULL CHECK (status IN ('todo', 'in_progress', 'done')))` |
| `0002_index_tasks_status.sql` | `CREATE INDEX IF NOT EXISTS tasks_status_idx ON tasks (status)` |

The table stores exactly the aggregate's four fields. No timestamps, ownership,
soft-delete column, or JSON metadata is added: the API exposes no such field, and
a column without a consumer would be a contract guessed in advance. The
primary-key index serves point lookups and the deterministic list order; the
status index supports status-based access without scanning.

The constraints duplicate rules the domain already enforces
(`btrim(title) <> ''`, the closed status set). This is deliberate defence in
depth: a bug in a future write path, a manual statement, or a second service
cannot store a value the API would reject. The database is the last line, not the
only line.

`status` is `TEXT` with a `CHECK` rather than a PostgreSQL `ENUM`. Adding a value
to an enum requires `ALTER TYPE`, which is a different kind of migration than a
`CHECK` change, and the closed set is small and owned by the domain. A lookup
table is also rejected: three values with no attributes do not need joins.

### Migrations

- **Versioned files, embedded in the binary.** Each file is named
  `NNNN_description.sql`; lexical order is version order, and the loader rejects
  any name that does not match `^\d{4}_[a-z0-9_]+\.sql$`, so a misordered change
  fails before touching the database. `embed.FS` means the binary carries its
  schema and never depends on the working directory or an external tool.
- **One statement per file.** The pgx stdlib driver runs prepared/extended
  queries, which reject multi-statement strings; keeping one statement per file
  makes that limitation irrelevant instead of surprising.
- **Additive and non-destructive.** Migrations only create tables, indexes, and
  constraints. No existing table is dropped or rewritten, and re-applying the
  runner is a no-op.
- **Applied once, recorded.** `schema_migrations (version TEXT PRIMARY KEY,
  applied_at TIMESTAMPTZ)` records each applied version. Each migration runs in
  its own transaction together with its `INSERT`, so a failure leaves both the
  schema and the ledger unchanged.
- **Serialised.** Each transaction takes `pg_advisory_xact_lock` before checking
  the ledger, so two concurrent publishers cannot apply the same migration
  twice.
- **Run by `cmd/migrate`, not by the service.** `make migrate` (or
  `go run ./cmd/migrate`) applies pending migrations and exits. The application
  process never changes the schema, which keeps ADR 0003's rejection of
  auto-migration at startup: a deploy that ships code and schema together stays
  an explicit two-step operation, and a read-only replica or a debugging session
  cannot mutate the schema by starting the service.

### Repository

`internal/task/repository/postgres` implements `service.Repository` over
`*sql.DB`. A compile-time assertion (`var _ service.Repository =
(*Repository)(nil)`) keeps the adapter honest when the port changes.

- **Explicit SQL, one place per statement.** `Create` inserts with
  `RETURNING`; `List` selects all rows; `Get` selects by identifier; `Update`
  updates with `RETURNING`; `Delete` deletes and inspects `RowsAffected`.
- **Identifiers come from the domain.** `Create` calls `domain.NewID`, the same
  generator, so identifier format is defined once. The client never supplies an
  identifier.
- **Collisions are retried, not surfaced.** `INSERT ... ON CONFLICT (id) DO
  NOTHING RETURNING ...` yields no row on collision; the adapter retries with a
  new identifier up to five times and then returns an error.
- **Not-found is a sentinel, not a driver detail.** `sql.ErrNoRows` from `Get`
  and `Update`, and `RowsAffected() == 0` from `Delete`, become
  `service.ErrNotFound`. The HTTP layer maps that with `errors.Is` and every
  other failure to a logged `500`, so SQL text never reaches a client.
- **`Update` never inserts.** It is a single `UPDATE ... RETURNING`; a missing
  row is `ErrNotFound`, matching the API contract (no upsert).
- **Returned values are copies.** Every method returns freshly scanned values,
  so callers cannot mutate adapter state.
- **Deterministic list order.** `List` orders by `id`, which makes responses
  stable and uses the primary-key index.
- **Identifiers are cast explicitly.** Parameters use `$1::uuid`, so a string
  identifier is always interpreted as a UUID by PostgreSQL rather than depending
  on driver-side type inference.

### Testing

Unit tests for the domain stay database-free. Persistence is verified by
integration tests that talk to a real PostgreSQL:

- `internal/task/repository/postgres/postgres_test.go` covers CRUD, missing
  rows, malformed identifiers, identifier uniqueness and collision retry, state
  shared across adapter instances, concurrent creates, and direct `INSERT`s that
  must violate the table constraints.
- `internal/infra/database/migrate_integration_test.go` applies the migrations
  twice and asserts the ledger has one row per file and the table and index
  exist.
- Both suites read `TEST_DATABASE_URL` and `t.Skip` with an explicit message
  when it is unset, so `go test ./...` stays runnable on a machine with no
  PostgreSQL. They also call `current_database()` and refuse to run destructive
  statements against a database whose name does not contain `test`; the tests
  truncate `tasks` between cases.
- Migration loading, ordering, naming, statement count, and the nil-pool guard
  are covered without a database.

## Alternatives considered

1. **An ORM or query builder (GORM, `sqlx`).** Rejected for the reasons in ADR
   0003: the SQL is small, reviewable, and the statement itself documents the
   behavior; scanning helpers are not worth a dependency here.
2. **`pgxpool` instead of `database/sql`.** Rejected for now for the same reason
   as ADR 0003: the standard interface keeps the adapter swappable and the port
   unchanged. Explicit `$1::uuid` casts keep the SQL driver-agnostic.
3. **A timestamped `created_at`/`updated_at` pair and ordering by creation
   time.** Deferred: the API exposes no timestamps and has no use case for them,
   so list order is defined by identifier instead of introducing columns the
   domain cannot represent.
4. **A PostgreSQL `ENUM` type for status.** Rejected: changing the set would
   require `ALTER TYPE` semantics instead of an ordinary migration, and the
   domain already owns the closed set; a `CHECK` keeps the database honest with a
   simpler evolution path.
5. **A `statuses` lookup table with a foreign key.** Rejected: three values with
   no attributes; the join and seeding cost buys nothing the `CHECK` does not.
6. **No database constraints, validation only in the domain.** Rejected: the
   database is the durable boundary; a second writer or a manual statement must
   not be able to store a state the API would reject.
7. **Auto-migrating at startup.** Rejected for the reason ADR 0003 gives:
   starting a process must not mutate the schema. `cmd/migrate` is explicit,
   scriptable, and can run as its own deploy step.
8. **Running migrations from `internal/infra/database.New`.** Rejected: pool
   construction would gain a network round trip and a write, making it
   untestable offline and surprising for a constructor.
9. **Integration tests behind a build tag instead of an environment variable.**
   Rejected: a tag makes the tests invisible to `go test ./...` and easy to
   forget; `t.Skip` with a message keeps them discoverable while remaining
   optional.
10. **A generic `Repository[T]` or shared CRUD interface.** Rejected by ADR 0003:
    one aggregate, one port, declared by its consumer.
11. **Truncating whatever database `TEST_DATABASE_URL` points at.** Rejected:
    a mistyped DSN must not be able to erase a development database, so the
    suites require `test` in the database name.

## Trade-offs

- **Gain:** tasks survive restarts; the constraints are enforced by the
  database; the port stays unchanged when the API is added; schema evolution is
  explicit, ordered, and auditable; persistence is tested against the real
  engine rather than a mock.
- **Cost:** two processes to think about in deployment — migrate, then start.
  Startup verifies the schema (ADR 0005), so forgetting the migration is caught
  before the HTTP server binds rather than on the first request.
- **Cost:** integration tests need a PostgreSQL instance and a database whose
  name contains `test`; they are skipped on machines without one, so a broken SQL
  change can pass `go test ./...` locally.
- **Cost:** `Check` constraints mean an invalid status is rejected twice (domain
  and database); the duplicate rule must be kept in sync, and changing the
  status set requires a migration as well as a domain change.
- **Cost:** ordering by a random identifier is stable but not meaningful to a
  reader; a creation-order view would need a timestamp column.
- **Cost:** `CREATE TABLE IF NOT EXISTS` cannot repair a table that already
  exists with a different shape; schema drift must be corrected by a new
  migration.
- **Mitigation:** keep migrations small and additive, run the integration suite
  in CI against a service container, and treat `TEST_DATABASE_URL` as required in
  any environment that claims to verify persistence.

## Consequences

- `internal/task/repository/postgres` is the only place that contains task SQL;
  handlers and the service never see `*sql.DB`.
- `internal/task/domain.NewID` is the single identifier generator, used by the
  PostgreSQL adapter and covered by a domain test.
- Changing the task schema means adding a migration file, never editing an
  applied one; the ledger is the record of what has run.
- `make migrate` (added to the `Makefile`) is the supported way to create or
  update the schema; `cmd/migrate` requires `DATABASE_URL` and fails with a
  non-zero exit on any error.
- `internal/app` constructs the adapter and wires it into the task service
  (ADR 0005); no other package owns task SQL, and there is no fallback store.
- New aggregates follow the same shape: a migration per statement, a repository
  in the feature slice, explicit SQL, sentinel not-found mapping, and an
  environment-gated integration suite.

## Amendment: runtime wiring

ADR 0005 adds the task API and the composition this record left out:

- `internal/app` constructs `postgres.New(pool)` and passes it to
  `service.New`; the adapter is the only task repository. There is no in-memory
  fallback.
- Startup pings the pool with a 5-second budget and calls
  `database.VerifySchema` before the HTTP server is built. The trade-off noted
  above — an unmigrated database reaching query time — is closed: startup fails
  closed and the error names `make migrate`.
- `cmd/migrate` is still the only process that changes the schema; the service
  never auto-migrates.

## Amendment: continuous integration

The environment-gated integration suite described above now runs in CI against
a real PostgreSQL service, with an explicit assertion that the database-backed
tests executed rather than skipped. The workflow, its isolation model, and its
trade-offs are recorded in
[ADR 0007](0007-ci-with-postgresql-service.md).
