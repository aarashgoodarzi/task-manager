# 0003. PostgreSQL through `database/sql` and pgx, without an ORM or a shared Database port

* Status: Accepted
* Date: 2026-10-19
* Scope: `internal/infra/database/`, future `internal/task/repository/`

## Context

PostgreSQL is the persistence choice for this service. A prior, larger Go
service in the same ecosystem persisted through a broad shared port — a
`Database` interface exposing `Create/Get/List/Update/Delete/Upsert/Exec/
Transaction` — implemented with GORM. Every feature repository depended on
that interface rather than on SQL.

That design solved a real problem there: many entities, several teams, and a
consistent query surface. Task Manager is not that shape. It has one planned
aggregate (Task), no queries yet, and no second datastore. Two risks follow
from copying the larger design:

- **Speculative abstraction.** A generic CRUD interface is defined before any
  query exists, so its shape is guessed rather than derived from use.
- **Hidden behavior.** ORM-generated SQL obscures transaction boundaries,
  indexes used, and the exact statements that production runs.

There is also a smaller, immediate question: how much should pool construction
do? Dialing or migrating during startup would make initialization slow and
dependent on a live server, and would make the adapter harder to unit-test.

## Decision

1. **Use `database/sql` with the pgx stdlib driver**
   (`github.com/jackc/pgx/v5/stdlib`, registered as `pgx` via blank import).
2. **`internal/infra/database.New` returns a plain `*sql.DB`.** It validates
   the DSN, opens the pool, and applies the pool settings. It performs no
   queries, runs no migrations, and does not ping.
3. **Do not introduce an ORM.** No GORM, no query builder, no entity
   auto-mapping.
4. **Do not introduce a shared `Database` interface** with generic CRUD
   methods. A repository feature will define its own narrow port in the
   application/service package that consumes it.
5. **Write explicit SQL in the driven adapter.** When task persistence is
   implemented, `internal/task/repository` will receive `*sql.DB` from the
   composition root and issue statements directly.

Two implementation details are part of the decision:

- The DSN is validated eagerly with `pgx.ParseConfig` so a malformed
  `DATABASE_URL` fails at startup with a clear error instead of surfacing on
  the first request.
- `sql.Open` is lazy — it validates arguments and returns a pool handle
  without dialing — so `New` is fast, offline-testable, and safe to call
  before the database is reachable. Reachability is checked by `/health`
  (ADR 0004).

## Alternatives considered

1. **GORM with a shared `Database` port.** Rejected: it introduces a second
   abstraction layer over an abstraction for a single entity, hides the SQL
   that defines the service's behavior, and makes transaction control implicit
   for callers that need it explicit.
2. **`pgxpool` (native pgx API) without `database/sql`.** Viable, and faster
   for pgx-specific features. Rejected for now because `database/sql` is the
   standard seam for Go persistence: repositories depend only on the standard
   library, stubs and test doubles are trivial, and the adapter can be swapped
   without touching repository signatures. Revisit if pgx-only capabilities
   (for example `CopyFrom` or batched queries) become necessary.
3. **`sqlx` or another thin helper.** Rejected: it saves struct-scanning
   boilerplate but adds a dependency and a second dialect of SQL handling.
   Scanning helpers can be written locally if repetition becomes painful.
4. **Embed SQL and DSN handling in handlers or `internal/app`.** Rejected:
   it breaks the adapter boundaries from ADR 0001 and ADR 0002 and makes the
   HTTP layer depend on the driver.
5. **Auto-migrate at startup.** Rejected for the current state: there is no
   schema yet, and running migrations from the application process couples
   deployment to schema changes. A migration strategy will be its own decision
   when the first table exists.

## Trade-offs

- **Gain:** every statement is visible and reviewable; no ORM dependency;
  pool construction is offline-testable; repositories can be unit-tested
  against a stubbed port.
- **Cost:** SQL and row scanning are written by hand, including repetitive
  `Scan` calls; there are no relation helpers, hooks, or automatic timestamps.
- **Cost:** no migrations means schema changes are currently out of scope
  entirely — the service requires no tables at all.
- **Cost:** the DSN is parsed twice, once by `pgx.ParseConfig` for validation
  and once inside `sql.Open`. The cost is negligible at startup and buys a
  precise error message.
- **Mitigation:** add migrations and any scanning helpers only when task
  persistence is implemented, and keep them in the persistence slice rather
  than in shared infrastructure.

## Consequences

- `github.com/jackc/pgx/v5` is a direct dependency, imported for
  `pgx.ParseConfig` and for the stdlib driver registration; no ORM module
  exists in `go.mod`.
- Future repositories may import `database/sql` and receive `*sql.DB`, but
  they must not import Gin or the HTTP package.
- Changing databases later means writing a new driven adapter that satisfies
  the same service-owned port; handlers and use cases stay unchanged.
- Connection pool limits remain configuration, not code: `DATABASE_MAX_OPEN_CONNS`,
  `DATABASE_MAX_IDLE_CONNS`, and `DATABASE_CONN_MAX_LIFETIME` are applied in
  `database.New`.
- `/health` depends on `PingContext` only; it does not assume any schema
  exists.

## Amendment

[ADR 0006](0006-postgres-task-persistence.md) decides the migration strategy
this record deferred. The driver, ORM, and port decisions above are unchanged:

- The schema is versioned SQL embedded in the binary and applied by
  `cmd/migrate` (`make migrate`), one statement per file, recorded in
  `schema_migrations`.
- The application process still does not migrate at startup, which preserves
  this record's rejection of auto-migration; starting the service never alters
  the schema.
- Task SQL lives in `internal/task/repository/postgres`, over `database/sql`
  with explicit parameters, exactly as required here.
