# Task Manager

Task Manager is a small Go HTTP service implementing task CRUD, with tasks
persisted in PostgreSQL. The HTTP contract is served by Gin, use cases live
behind a service-owned repository port, and the only repository adapter is an
explicit-SQL PostgreSQL implementation. The schema is versioned, embedded in the
binary, and applied by a separate `migrate` command.

The service fails closed: without `DATABASE_URL`, without a reachable
PostgreSQL server, or without the expected schema it refuses to start. There is
no in-memory fallback and no hidden storage.

## Overview

### Current capabilities

| Area | Status |
| --- | --- |
| Environment configuration with defaults | Implemented |
| PostgreSQL pool construction and DSN validation | Implemented |
| Startup connectivity and schema verification (fail closed) | Implemented |
| Versioned, embedded SQL migrations applied by `cmd/migrate` | Implemented |
| HTTP server with recovery and request-logging middleware | Implemented |
| `GET /health` (readiness) and `GET /health/live` (liveness) | Implemented |
| Graceful shutdown on `SIGINT` / `SIGTERM` | Implemented |
| Task domain model, statuses, validation, and identifier format | Implemented |
| Task service use cases behind a repository port | Implemented |
| PostgreSQL task repository with explicit SQL | Implemented |
| CRUD endpoints: `POST/GET/PUT/DELETE /tasks` | Implemented |
| Consistent JSON error envelope, `404` / `405` handling | Implemented |
| Unit, service, HTTP, and PostgreSQL integration tests | Implemented |
| Filtering, pagination, sorting controls, authentication | Not implemented |
| Dockerfile / Docker Compose, CI pipeline, OpenAPI, metrics, tracing | Not implemented |

Every request reads and writes the `tasks` table; task data survives restarts.

### Technology stack

| Concern | Choice |
| --- | --- |
| Language | Go 1.26.2 (declared in `go.mod`) |
| HTTP | [Gin](https://github.com/gin-gonic/gin) v1.12.0 |
| Database access | `database/sql` with the [pgx](https://github.com/jackc/pgx) v5 stdlib driver |
| Database | PostgreSQL (required) |
| Migrations | Embedded `NNNN_description.sql` files applied by `cmd/migrate` |
| Logging | `log/slog` (text handler, JSON not used) |
| Configuration | Standard library `os.Getenv` with per-setting defaults |

No ORM, external migration tool, or container setup is present. The Makefile is
a thin wrapper over the Go toolchain.

## Architecture

Task Manager uses **Clean Architecture**, organized as per-feature vertical
slices. The rationale and the alternatives that were rejected are recorded in
[ADR 0001](decisions/0001-clean-architecture.md); the package
layout and the composition root are covered by
[ADR 0002](decisions/0002-package-layout-and-composition-root.md); the task
feature and its HTTP contract by
[ADR 0005](decisions/0005-task-crud-api.md); PostgreSQL persistence and
migrations by
[ADR 0006](decisions/0006-postgres-task-persistence.md).

### Architectural style and rationale

- The application core owns the interfaces (*ports*) it needs from the
  outside world. Ports are declared where they are used, not in a central
  contracts package, so a port exists only when a real consumer needs it.
- Adapters implement those ports or drive the core from the outside. HTTP is
  a driving adapter; the PostgreSQL repository is a driven adapter.
- The **composition root** (`internal/app`) is the only package that knows
  every concrete adapter and assembles them into one process.

This is Clean Architecture's dependency rule: business logic and application
rules must not depend on a framework, a driver, or a transport detail. The
codebase keeps Go's concrete package names (`handler → service (port) →
repository adapter`) instead of adding layer folders; the dependency
direction, not the folder names, is what makes the architecture.

### Package responsibilities

```
cmd/task-manager/                  process entry point: construct App, Initialize, Start
cmd/migrate/                       apply embedded migrations and exit
internal/app/                      composition root: config → logger → database (ping,
                                   schema check) → task repository/service/handler → server
internal/infra/config/             environment loading and defaults
internal/infra/database/           PostgreSQL adapter: DSN validation, *sql.DB pool,
                                   embedded migrations, schema verification
internal/infra/server/             Gin router, middleware, /health, 404/405, HTTP lifecycle
internal/infra/handler/            shared HTTP delivery helpers and JSON error envelope
internal/task/domain/              Task aggregate, statuses, validation, UUID identifiers
internal/task/service/             use cases; declares the repository port it consumes
internal/task/handler/             Gin handlers: request/response mapping, error mapping
internal/task/repository/postgres/ PostgreSQL driven adapter: explicit SQL over *sql.DB
```

| Package | Responsibility | May depend on |
| --- | --- | --- |
| `cmd/task-manager` | Process entry point. Contains no logic beyond error handling for startup. | `internal/app` |
| `cmd/migrate` | Applies pending migrations using the configuration and exits non-zero on any error. | `config`, `database` |
| `internal/app` | Composition root. Loads config, builds the logger, the required pool, verifies the schema, wires the task stack, and the server; shuts them down in reverse order. | `config`, `database`, `server`, task packages |
| `internal/infra/config` | Reads environment variables into plain structs and applies defaults. Performs no network or filesystem I/O and knows nothing about HTTP or SQL. | Standard library only |
| `internal/infra/database` | Validates the DSN with `pgx.ParseConfig`, opens a pooled `*sql.DB`, applies pool limits, embeds and applies migrations, verifies required tables. | `database/sql`, `pgx`, `embed` |
| `internal/infra/server` | Builds the Gin engine, registers middleware, `/health`, `/health/live`, feature modules, and `404`/`405` handlers; runs the HTTP server and shuts it down gracefully. Declares the `HealthChecker` and `RouteRegistrar` ports it consumes. | Gin, `internal/infra/handler`, standard library |
| `internal/infra/handler` | Shared HTTP delivery code. Defines the single JSON error envelope and writes it with an HTTP status. | Gin, `net/http` |
| `internal/task/domain` | `Task`, the closed `Status` set, defaults, validation, and the UUID identifier format and generator. No framework or driver imports. | Standard library |
| `internal/task/service` | Create, list, get, update, delete use cases. Declares the `Repository` port and `ErrNotFound`. | `context`, `domain` |
| `internal/task/handler` | Decodes requests strictly, validates path IDs, calls the use cases, and maps results and errors to HTTP. Declares the `TaskService` port it consumes. | Gin, `internal/infra/handler`, `domain`, `service` |
| `internal/task/repository/postgres` | Explicit SQL for CRUD, identifier generation, `sql.ErrNoRows` → `service.ErrNotFound` mapping, deterministic list order. | `database/sql`, `domain`, `service` |

### Dependency direction

Dependencies point inward. `config` and `database` never import the server; the
server never imports the database adapter or the task packages. The server
consumes only the narrow ports it declares itself:

```go
// internal/infra/server/server.go
type HealthChecker interface {
	PingContext(ctx context.Context) error
}

type RouteRegistrar interface {
	RegisterRoutes(r gin.IRouter)
}
```

`*sql.DB` satisfies `HealthChecker`, so `internal/app` passes the pool directly.
The task handler satisfies `RouteRegistrar`, so the server registers feature
routes without importing the task package. Inside the feature,
`service.Repository` is declared by the service and implemented by
`repository/postgres`; `handler.TaskService` is declared by the handler and
satisfied by `*service.Service`.

### Wiring and startup sequence

`cmd/task-manager/main.go` constructs `app.New()` and calls `Initialize`, then
`Start`. No wiring lives in `main`.

1. `config.Load()` reads the environment and applies defaults.
2. `newLogger(cfg.Logger)` builds a `slog.Logger` writing text records to
   stdout at the configured level.
3. `DATABASE_URL` is required. An empty value aborts startup before any other
   adapter is built.
4. `database.New(...)` validates the DSN, opens the pooled `*sql.DB`, and
   applies the pool limits.
5. `Initialize` pings the pool with a 5-second budget and then runs
   `database.VerifySchema`, which requires the `tasks` table. A failed ping or a
   missing table closes the pool and returns an error, so the process never
   starts half-wired.
6. `postgres.New(pool)` → `service.New(...)` → `handler.New(...)` builds the task
   stack on the PostgreSQL repository.
7. `server.New(...)` receives the pool as the health checker and the task handler
   as a `RouteRegistrar`, builds the Gin router with `gin.Recovery()`, a
   structured request logger, `/health`, `/health/live`, and the task routes, and
   wraps it in an `*http.Server` with the configured timeouts.
8. `Start()` installs a signal context for `SIGINT`/`SIGTERM`, blocks on
   `server.Run`, then shuts the HTTP server down with a 10-second budget and
   closes the connection pool.

### Database access

Persistence uses `database/sql` with the pgx stdlib driver instead of an ORM,
as decided in [ADR 0003](decisions/0003-postgres-via-database-sql.md).
`database.New` does not ping or dial: `sql.Open` is lazy, so pool construction
stays fast and unit-testable. Reachability is checked explicitly in
`App.Initialize` (fail closed) and again by `/health` at runtime.

The schema is one table:

```sql
CREATE TABLE IF NOT EXISTS tasks (
    id          UUID PRIMARY KEY,
    title       TEXT NOT NULL CHECK (btrim(title) <> ''),
    description TEXT NOT NULL DEFAULT '',
    status      TEXT NOT NULL CHECK (status IN ('todo', 'in_progress', 'done'))
);

CREATE INDEX IF NOT EXISTS tasks_status_idx ON tasks (status);
```

Rules that matter:

- The table stores exactly the fields the API exposes; there are no timestamps
  or hidden columns.
- The `CHECK` constraints repeat the domain rules, so an invalid row cannot be
  written even by a statement that bypasses the service.
- `GET /tasks` is ordered by `id`, ascending. The order is stable across calls
  but is not creation order.
- Identifiers are version 4 UUIDs generated by the adapter from
  `crypto/rand`; a collision is retried.

### Migrations

Migration files live in `internal/infra/database/migrations/`, are embedded in
the binary, and are named `NNNN_description.sql`. Lexical order is version
order, each file contains exactly one statement, and applied versions are
recorded in `schema_migrations`. Each migration runs in its own transaction
under an advisory lock, so concurrent runs cannot apply a version twice.

Migrations are **not** run by the service. Apply them explicitly:

```bash
make migrate
# equivalent:
go run ./cmd/migrate
```

`make migrate` requires `DATABASE_URL` and exits non-zero on failure. The
service verifies the table exists at startup and tells you to run `make migrate`
when it does not.

### Startup errors and health

`App.Initialize` and `App.Start` return errors; `main` logs and exits with
status 1. Startup is fail-closed:

| Condition | Result |
| --- | --- |
| `DATABASE_URL` empty | error `app: initialize database: DATABASE_URL is required` |
| Malformed DSN | error `app: initialize database: database: invalid DSN: ...` |
| Server unreachable / ping timeout | error `app: initialize database: ping: ...` |
| `tasks` table missing | error `... schema is not initialized: table "tasks" is missing; run \`make migrate\`` |

`GET /health` is the readiness probe: it pings the database with a 2-second
budget and reports `503` when the ping fails. `GET /health/live` is the liveness
probe: it reports `200` without touching the database, so an orchestrator can
tell "process running" from "process ready". See
[ADR 0004](decisions/0004-startup-errors-and-health-endpoint.md), amended by
[ADR 0005](decisions/0005-task-crud-api.md).

## Prerequisites

- **Go 1.26.2 or newer.** The module declares `go 1.26.2`; `go.mod` is the
  source of truth.
- **PostgreSQL — required.** The service, the migration command, and the
  integration tests all need it. Verified against PostgreSQL 14.13.
- **`make`** (optional) for the wrapper targets in the
  [Makefile reference](#makefile-reference). Every target has a direct `go`
  equivalent.
- **`curl`** (optional) for the examples below.

Create the databases used below:

```bash
createdb task_manager      # runtime database
createdb task_manager_test # integration tests (the name must contain "test")
```

## Configuration

All settings come from environment variables and are read once at startup.
There is no configuration file parser, no hot reload, and no flag parsing.

| Variable | Required | Default | Purpose |
| --- | --- | --- | --- |
| `DATABASE_URL` | **Yes** | *(none)* | PostgreSQL connection string in the format accepted by pgx/libpq, e.g. `postgres://user:pass@localhost:5432/task_manager?sslmode=disable`. |
| `PORT` | No | `8080` | TCP port; the server listens on `:<PORT>` on all interfaces. |
| `DATABASE_MAX_OPEN_CONNS` | No | `10` | Maximum open connections in the pool. |
| `DATABASE_MAX_IDLE_CONNS` | No | `5` | Maximum idle connections retained. |
| `DATABASE_CONN_MAX_LIFETIME` | No | `30m` | Maximum lifetime of a pooled connection (Go duration). |
| `LOG_LEVEL` | No | `info` | One of `debug`, `info`, `warn`, `error`. Any other value falls back to `info`. |
| `SERVER_READ_HEADER_TIMEOUT` | No | `5s` | Deadline for reading request headers. |
| `SERVER_READ_TIMEOUT` | No | `15s` | Deadline for reading the whole request. |
| `SERVER_WRITE_TIMEOUT` | No | `15s` | Deadline for writing the response. |
| `SERVER_IDLE_TIMEOUT` | No | `60s` | How long an idle keep-alive connection is kept open. |
| `TEST_DATABASE_URL` | No | *(empty)* | Used only by integration tests and `make test-integration`; the service ignores it. |
| `GIN_MODE` | No | `debug` | Read by Gin itself, not by `internal/infra/config`. Set to `release` to silence Gin's debug output and route registration logs. |

Behavior worth knowing:

- **Empty values are treated as unset** for optional settings. Setting `PORT=`
  behaves like leaving it out.
- **Invalid numeric or duration values do not fail startup.** An unparseable
  `DATABASE_MAX_OPEN_CONNS` or `SERVER_READ_TIMEOUT` silently falls back to the
  default. `DATABASE_URL` is validated.
- **`DATABASE_URL` is mandatory.** A missing or malformed value aborts startup
  before the HTTP server binds.

### Using `.env.example`

[`.env.example`](.env.example) documents the supported variables. The service
does **not** read `.env` files itself; the file is a template for your shell
environment. Copy it, adjust values, and export it before starting:

```bash
cp .env.example .env
# set DATABASE_URL and TEST_DATABASE_URL for your PostgreSQL instance

set -a && . ./.env && set +a
make migrate
make run
```

`.env` is listed in `.gitignore` and must not be committed. Keep real
credentials out of the repository, and treat the values in `.env.example` as
placeholders only.

Exporting is enough for a single terminal. For a new shell, export the file
again. To override a single setting for one run, prefix the command:

```bash
PORT=9090 make run
```

## Running Locally

### Download dependencies

```bash
go mod download
```

This populates the module cache and verifies `go.sum`. It is optional when the
cache already has the dependencies; if a build reports a missing checksum or
module, run `go mod download` and retry.

### Prepare the schema

```bash
export DATABASE_URL='postgres:///task_manager?host=localhost&sslmode=disable'
make migrate
```

Expected output: `migrate: schema is up to date`. Re-running the command is
safe; applied versions are recorded in `schema_migrations`.

### Run the service

```bash
make run
# equivalent:
go run ./cmd/task-manager
```

A successful start logs the following, then keeps running (Gin also prints its
route registration and debug-mode warning unless `GIN_MODE=release` is set):

```
level=INFO msg="task-manager initialized" addr=:8080
level=INFO msg="server listening" addr=:8080
```

Verify it from another terminal:

```bash
curl -i http://localhost:8080/health
curl -i http://localhost:8080/health/live
curl -i http://localhost:8080/tasks
```

### Build the binary

```bash
make build
# equivalent:
go build -o task-manager ./cmd/task-manager
./task-manager
```

`make build` writes the binary named `task-manager` in the repository root,
which is git-ignored. To put it elsewhere:

```bash
go build -o bin/task-manager ./cmd/task-manager
```

The output name can be overridden. The package path is fixed in the Makefile:

```bash
make build BINARY=bin/task-manager
```

### Stop the service

Press `Ctrl+C` (or send `SIGTERM`). The server stops accepting requests, drains
in-flight requests for up to 10 seconds, logs `shutting down server`, closes the
database pool, and exits. Tasks remain in PostgreSQL.

There is no `make stop` target; the service runs in the foreground.

### Data and storage semantics

- **Durable.** Tasks survive restarts, crashes, and redeploys.
- **Shared.** Every process pointed at the same database sees the same tasks.
- **List order is by `id`, ascending.** Stable across calls, but not creation
  order; the API exposes no timestamps.
- **No filtering, pagination, or search.** `GET /tasks` returns every row.
- **No soft delete.** `DELETE` removes the row.

### Database availability

- **Startup** fails with a non-zero exit when the server is unreachable, the
  ping times out, or the schema is missing. There is no retry loop and no
  fallback storage.
- **Runtime** failures after startup (for example PostgreSQL restarting) surface
  as `500 internal_error` from the task endpoints; the details are logged, not
  returned. `/health` reports `503` until the database is reachable again, and
  `/health/live` keeps reporting `200` because the process itself is alive.

### Docker

There is no `Dockerfile`, `docker-compose.yml`, or container-based workflow in
this repository. Run the service with the Go toolchain as shown above.

## Testing and Quality Checks

The suite has two tiers:

- **Database-free tests** cover the domain, service use cases, HTTP adapter, and
  router. They run anywhere.
- **PostgreSQL integration tests** cover the repository and the migrations. They
  run only when `TEST_DATABASE_URL` is set and otherwise `t.Skip` with an
  explicit message. They truncate the `tasks` table, so they refuse to run
  against a database whose name does not contain `test`.

The configuration tests read the process environment directly, so run them with
no overriding `PORT`, `DATABASE_URL`, `DATABASE_MAX_OPEN_CONNS`, `SERVER_*`, or
`LOG_LEVEL` values in your shell — a preset `PORT` makes `TestLoadDefaults`
fail. Use `env -u PORT` for a clean run if needed.

| Task | Command |
| --- | --- |
| Database-free tests | `make test` or `go test ./...` |
| Whole suite including PostgreSQL | `make test-integration TEST_DATABASE_URL='postgres:///task_manager_test?host=localhost&sslmode=disable'` |
| One package | `go test ./internal/task/handler` |
| Verbose output | `go test -v ./...` |
| Race detector | `TEST_DATABASE_URL=... go test -race ./...` |
| Coverage summary | `TEST_DATABASE_URL=... go test -cover ./...` |
| Coverage profile | `TEST_DATABASE_URL=... go test -coverprofile=coverage.out ./...` |
| Browse coverage report | `go tool cover -html=coverage.out` |
| Vet | `make vet` or `go vet ./...` |
| Formatting check | `make fmt` or `gofmt -l .` |
| Apply formatting | `gofmt -w .` |
| Build the binary | `make build` or `go build -o task-manager ./cmd/task-manager` |
| Compile every package | `go build ./...` |
| Apply migrations | `make migrate` or `go run ./cmd/migrate` |

Notes:

- `make fmt` and `gofmt -l .` only **list** files whose formatting differs.
  They neither rewrite files nor fail the command. Use `gofmt -w .` to fix
  them, and check for non-empty output in scripts or CI.
- `coverage.out` matches the `*.out` pattern in `.gitignore`.
- Measured statement coverage on this tree: **77.4%** with the PostgreSQL
  integration tests and **58.4%** without them. The difference is the
  PostgreSQL adapter, whose statements only execute against a real database.
- `internal/app` tests cover a missing `DATABASE_URL`, a malformed DSN, an
  unreachable server, a missing schema, and a successful PostgreSQL wiring when
  `TEST_DATABASE_URL` is available.
- `cmd/task-manager`, `cmd/migrate`, and `internal/infra/handler` have no test
  files of their own (the envelope is exercised through the task handler and
  server tests; `cmd/migrate` is exercised by the migration integration test
  and by running the command).

## Makefile Reference

The Makefile is a thin wrapper over the Go toolchain. It declares
`BINARY ?= task-manager` and `CMD := ./cmd/task-manager`.

| Target | Purpose | Example | Prerequisites |
| --- | --- | --- | --- |
| `build` | Compile the service into `$(BINARY)` (default: `./task-manager`). | `make build` | Go toolchain |
| `run` | Run the service in the foreground via `go run`. | `make run` | `DATABASE_URL`, migrated schema |
| `migrate` | Apply pending embedded migrations. | `make migrate` | `DATABASE_URL` |
| `test` | Run `go test ./...`; PostgreSQL tests skip without `TEST_DATABASE_URL`. | `make test` | Go toolchain |
| `test-integration` | Run the whole suite with integration tests enabled; fails when `TEST_DATABASE_URL` is unset. | `make test-integration TEST_DATABASE_URL=...` | PostgreSQL |
| `vet` | Run `go vet ./...`. | `make vet` | Go toolchain |
| `fmt` | List files that `gofmt` would change; does not modify them. | `make fmt` | Go toolchain |

Every target is listed in `.PHONY`. There are no `clean`, `cover`, `lint`, or
`docker` targets; use the direct Go commands from
[Testing and Quality Checks](#testing-and-quality-checks) for those workflows.

## API

The service exposes health endpoints and a REST resource for tasks. All request
and response bodies are JSON (`Content-Type: application/json`).

### Task endpoints

| Method | Path | Behavior |
| --- | --- | --- |
| `POST` | `/tasks` | Create a task. Returns `201` with the task and a `Location` header. |
| `GET` | `/tasks` | List all tasks. Returns `200` with a JSON array (`[]` when empty). |
| `GET` | `/tasks/:id` | Retrieve one task. Returns `200`. |
| `PUT` | `/tasks/:id` | Replace a task. Returns `200` with the updated task. |
| `DELETE` | `/tasks/:id` | Delete a task. Returns `204` with no body. |

Task representation:

```json
{
  "id": "6ba7b810-9dad-41d1-80b4-00c04fd430c8",
  "title": "Write the report",
  "description": "Cover the API contract",
  "status": "todo"
}
```

Rules:

- `id` is a server-generated version 4 UUID. Clients never send it.
- `title` is required. Leading and trailing whitespace is trimmed; a title that
  is empty or only whitespace is rejected by the service and by a database
  constraint.
- `description` is optional and may be empty.
- `status` is one of `todo`, `in_progress`, `done`. Omitting it selects the
  default `todo`. No transition rules are enforced: any allowed value may be
  set on create or update.
- `PUT` is a **full replacement** of the mutable fields: `title` is required,
  `description` is replaced (empty when omitted), and omitting `status` resets
  it to `todo`. Unknown IDs return `404`; `PUT` never creates a task.
- Request bodies are decoded strictly: unknown fields, trailing content, wrong
  types, or an empty body return `400`.
- A malformed `:id` (not UUID-shaped) returns `400`; a well-formed but unknown
  ID returns `404`.
- `GET /tasks` returns every task ordered by `id`, ascending.

#### Create a task

```bash
curl -i -X POST http://localhost:8080/tasks \
  -H 'Content-Type: application/json' \
  -d '{"title":"Write the report","description":"Cover the API contract","status":"in_progress"}'
```

**Response — HTTP 201 Created**

```http
Location: /tasks/6ba7b810-9dad-41d1-80b4-00c04fd430c8
```

```json
{
  "id": "6ba7b810-9dad-41d1-80b4-00c04fd430c8",
  "title": "Write the report",
  "description": "Cover the API contract",
  "status": "in_progress"
}
```

#### List tasks

```bash
curl -i http://localhost:8080/tasks
```

**Response — HTTP 200 OK**

```json
[]
```

#### Get, replace, delete

```bash
curl -i http://localhost:8080/tasks/6ba7b810-9dad-41d1-80b4-00c04fd430c8

curl -i -X PUT http://localhost:8080/tasks/6ba7b810-9dad-41d1-80b4-00c04fd430c8 \
  -H 'Content-Type: application/json' \
  -d '{"title":"Report published","description":"Done","status":"done"}'

curl -i -X DELETE http://localhost:8080/tasks/6ba7b810-9dad-41d1-80b4-00c04fd430c8
```

`DELETE` returns `204 No Content` with an empty body. A subsequent `GET`
returns `404`; the row is removed from the table.

### Error responses

Every API error uses one envelope:

```json
{
  "error": {
    "code": "validation_error",
    "message": "invalid task data",
    "fields": {
      "title": "title is required"
    }
  }
}
```

`fields` is present only for validation failures. `message` is safe to display
and never contains internal errors, SQL text, constraint names, or stack traces.

| Status | `code` | When |
| --- | --- | --- |
| `400` | `invalid_request` | Body is not a single valid JSON object, has unknown fields, or has the wrong field types. |
| `400` | `validation_error` | `title` is missing/blank, `status` is not an allowed value, or `:id` is not a UUID. |
| `404` | `not_found` | Task ID is well-formed but does not exist, or the route is unknown. |
| `405` | `method_not_allowed` | The path exists but does not support the method. |
| `500` | `internal_error` | Unexpected failure, including a database error. Details are logged, not returned. |

#### Validation example

```bash
curl -i -X POST http://localhost:8080/tasks \
  -H 'Content-Type: application/json' \
  -d '{"title":"x","status":"pending"}'
```

**Response — HTTP 400 Bad Request**

```json
{
  "error": {
    "code": "validation_error",
    "message": "invalid task data",
    "fields": {
      "status": "status must be one of \"todo\", \"in_progress\", \"done\""
    }
  }
}
```

#### Unknown task

```json
{
  "error": {
    "code": "not_found",
    "message": "task not found"
  }
}
```

### Health endpoints

`GET /health` is the readiness probe. It pings the configured database with a
2-second budget and never claims a check it did not run.

**Response — database ping succeeds (HTTP 200)**

```json
{"database":"ok","status":"ok"}
```

**Response — database unreachable or ping timed out (HTTP 503)**

```json
{"error":"database unreachable","status":"error"}
```

**Response — no checker wired (HTTP 503)** — only reachable in tests that build
the server directly; `internal/app` always supplies the pool.

```json
{"error":"database not configured","status":"error"}
```

`GET /health/live` is the liveness probe. It never touches the database.

**Response — process is running (HTTP 200)**

```json
{"status":"ok"}
```

Notes:

- The health check is shallow: it verifies connectivity only, not schema or
  data. Schema presence is verified once, at startup.
- `/health` and `/health/live` keep their operational response shapes; the error
  envelope above applies to the task API.
- Other methods on either health path return `405`; unknown paths return `404`,
  both with the error envelope.

## Development Conventions

### Adding a feature

The task slice demonstrates the shape new features should follow:

```
internal/task/
├── domain/              entities, value types, invariants; no framework imports
├── service/             use cases; declares the repository port it consumes
├── handler/             driving adapter: HTTP/Gin, request/response mapping
└── repository/postgres/ driven adapter: explicit SQL for the aggregate
```

Rules that keep the architecture intact:

1. **Declare ports where they are consumed.** A repository interface belongs
   beside the service that calls it, not in a shared interface package and not
   in the repository package. `HealthChecker` and `RouteRegistrar` in
   `internal/infra/server` and `TaskService` in `internal/task/handler` are the
   existing examples.
2. **Keep the core free of transport and driver imports.** `domain` and
   `service` must not import Gin, `net/http`, `database/sql`, or `pgx`.
3. **Wire everything in `internal/app`.** Feature constructors are called there
   and passed inward as ports. No other package imports config, database, and
   server together.
4. **Keep `main` trivial.** It only constructs the app, calls `Initialize` and
   `Start`, and handles returned errors.
5. **Test at the port boundary.** Service and HTTP tests use hand-written fakes
   and never need PostgreSQL; the SQL adapter has its own integration suite
   gated by `TEST_DATABASE_URL`.
6. **Return errors, do not panic.** Panics belong only to programmer bugs that
   Gin's recovery middleware should turn into a 500; configuration and startup
   problems are returned as errors.
7. **Keep error responses uniform.** New endpoints must use
   `internal/infra/handler` and the documented codes instead of inventing a
   response shape.
8. **Change the schema with a migration.** Never edit an applied migration file;
   add the next `NNNN_` file with one statement.

When a new architectural choice is made — a migration strategy, a readiness or
liveness split, a caching layer — record it in `/decisions` rather than only in
code comments.

## Architecture Decisions

Architecture Decision Records live in [`decisions/`](decisions/README.md). Each
record captures the context, the decision, the alternatives that were
considered, and the resulting trade-offs.

| ADR | Decision |
| --- | --- |
| [0001](decisions/0001-clean-architecture.md) | Adopt Clean Architecture and its dependency rule as the service architecture. |
| [0002](decisions/0002-package-layout-and-composition-root.md) | Feature-oriented package layout and a single composition root that owns wiring. |
| [0003](decisions/0003-postgres-via-database-sql.md) | Use `database/sql` with pgx instead of an ORM or a shared `Database` port; migration strategy amended by 0006. |
| [0004](decisions/0004-startup-errors-and-health-endpoint.md) | Return startup errors to the process edge and expose a shallow, database-backed health endpoint. |
| [0005](decisions/0005-task-crud-api.md) | Task model, service-owned repository port, HTTP and error contract. |
| [0006](decisions/0006-postgres-task-persistence.md) | PostgreSQL task persistence: table, versioned embedded migrations, explicit-SQL repository, integration testing. |
