# Task Manager

Task Manager is a small Go HTTP service backed by PostgreSQL. It currently
provides the operational core of the service: environment-based
configuration, a PostgreSQL connection pool, a Gin HTTP server with logging
and recovery middleware, a database-aware health endpoint, and graceful
shutdown.

Task CRUD, persistence queries, and database migrations are **not
implemented yet**. The sections below mark the boundary between what exists
and what is planned.

## Overview

### Current capabilities

| Area | Status |
| --- | --- |
| Environment configuration with defaults | Implemented |
| PostgreSQL pool construction and DSN validation | Implemented |
| HTTP server with recovery and request-logging middleware | Implemented |
| `GET /health` with a bounded database ping | Implemented |
| Graceful shutdown on `SIGINT` / `SIGTERM` | Implemented |
| Unit tests for config, pool construction, and the health handler | Implemented |
| Task domain model, service, and HTTP handlers | Not implemented |
| Task persistence and SQL queries | Not implemented |
| Schema migrations | Not implemented |
| Dockerfile / Docker Compose, CI pipeline, OpenAPI, metrics, tracing | Not implemented |

The service starts even when PostgreSQL is unreachable; only `/health`
reports the database condition. See
[Database availability](#database-availability) for details.

### Technology stack

| Concern | Choice |
| --- | --- |
| Language | Go 1.26.2 (declared in `go.mod`) |
| HTTP | [Gin](https://github.com/gin-gonic/gin) v1.12.0 |
| Database access | `database/sql` with the [pgx](https://github.com/jackc/pgx) v5 stdlib driver |
| Database | PostgreSQL |
| Logging | `log/slog` (text handler, JSON not used) |
| Configuration | Standard library `os.Getenv` with per-setting defaults |

No ORM, migration tool, or container setup is present. The Makefile is a thin
wrapper over the Go toolchain, not an additional build system.

## Architecture

Task Manager uses **Clean Architecture**, organized as per-feature vertical
slices. The rationale and the alternatives that were rejected are recorded in
[ADR 0001](decisions/0001-clean-architecture.md); the package
layout and the composition root are covered by
[ADR 0002](decisions/0002-package-layout-and-composition-root.md).

### Architectural style and rationale

- The application core owns the interfaces (*ports*) it needs from the
  outside world. Ports are declared where they are used, not in a central
  contracts package, so a port exists only when a real consumer needs it.
- Adapters implement those ports or drive the core from the outside. HTTP is
  a driving adapter; PostgreSQL is a driven adapter.
- The **composition root** (`internal/app`) is the only package that knows
  every concrete adapter and assembles them into one process.

This is Clean Architecture's dependency rule: business logic and application
rules must not depend on a framework, a driver, or a transport detail. The
codebase keeps Go's concrete package names (`handler → service (port) →
repository adapter`) instead of adding layer folders; the dependency
direction, not the folder names, is what makes the architecture.

### Package responsibilities

```
cmd/task-manager/          process entry point: construct App, Initialize, Start
internal/app/              composition root: config → logger → database → server
internal/infra/config/     environment loading and defaults
internal/infra/database/   PostgreSQL adapter: DSN validation, *sql.DB pool
internal/infra/server/     Gin router, middleware, /health, HTTP lifecycle
```

| Package | Responsibility | May depend on |
| --- | --- | --- |
| `cmd/task-manager` | Process entry point. Contains no logic beyond error handling for startup. | `internal/app` |
| `internal/app` | Composition root. Loads config, builds the logger, database pool, and server, and terminates the HTTP server on a signal. | `config`, `database`, `server` |
| `internal/infra/config` | Reads environment variables into plain structs and applies defaults. Performs no network or filesystem I/O and knows nothing about HTTP or SQL. | Standard library only |
| `internal/infra/database` | Validates the DSN with `pgx.ParseConfig`, opens a pooled `*sql.DB`, applies pool limits. Runs no queries and no migrations. | `database/sql`, `pgx` |
| `internal/infra/server` | Builds the Gin engine, registers middleware and routes, runs the HTTP server, and shuts it down gracefully. Defines the `HealthChecker` port it consumes. | Gin, standard library |

No domain or application package exists yet. `internal/task/` will be
introduced together with the task feature, not before (see
[ADR 0002](decisions/0002-package-layout-and-composition-root.md)).

### Dependency direction

Dependencies point inward. `config` and `database` never import the server;
the server never imports the database adapter. The server consumes only the
narrow port it declares itself:

```go
// internal/infra/server/server.go
type HealthChecker interface {
	PingContext(ctx context.Context) error
}
```

`*sql.DB` already satisfies that interface, so `internal/app` passes the pool
directly and `internal/infra/server` stays free of driver imports. The same
approach applies to future feature code: a service package declares the
repository port it needs, and a repository package implements it.

### Wiring and startup sequence

`cmd/task-manager/main.go` constructs `app.New()` and calls `Initialize`,
then `Start`. No wiring lives in `main`.

1. `config.Load()` reads the environment and applies defaults.
2. `newLogger(cfg.Logger)` builds a `slog.Logger` writing text records to
   stdout at the configured level.
3. `database.New(...)` validates `DATABASE_URL`, opens the pool, and applies
   the pool limits.
4. `server.New(...)` stores the pool as a `HealthChecker`, builds the Gin
   router with `gin.Recovery()` and a structured request logger, and wraps it
   in an `*http.Server` with the configured timeouts.
5. `Start()` installs a signal context for `SIGINT`/`SIGTERM`, blocks on
   `server.Run`, then shuts the HTTP server down with a 10-second budget and
   closes the database pool.

### Database access

Persistence uses `database/sql` with the pgx stdlib driver instead of an ORM.
`database.New` does not ping or dial the network: `sql.Open` is lazy, so pool
construction is fast and unit-testable without a live server. Connectivity is
verified by `/health`, and future repository methods will issue explicit SQL
against `*sql.DB`. The decision and its trade-offs are recorded in
[ADR 0003](decisions/0003-postgres-via-database-sql.md).

### Startup errors and health

`App.Initialize` and `App.Start` return errors; `main` logs and exits with
status 1. Missing or malformed configuration therefore fails at startup rather
than on the first request. `GET /health` is the only runtime check and reports
database reachability only. See
[ADR 0004](decisions/0004-startup-errors-and-health-endpoint.md).

## Prerequisites

- **Go 1.26.2 or newer.** The module declares `go 1.26.2`; `go.mod` is the
  source of truth.
- **PostgreSQL** reachable through `DATABASE_URL`. The DSN is required for
  startup; a live server is needed only for `/health` to return `200`. No
  schema, database, or table is created or required by the service.
- **`make`** (optional) for the wrapper targets in the
  [Makefile reference](#makefile-reference). Every target has a direct `go`
  equivalent.
- **`curl`** (optional) for the examples below.

## Configuration

All settings come from environment variables and are read once at startup.
There is no configuration file parser, no hot reload, and no flag parsing.

| Variable | Required | Default | Purpose |
| --- | --- | --- | --- |
| `DATABASE_URL` | **Yes** | *(empty)* | PostgreSQL connection string in the format accepted by pgx/libpq, e.g. `postgres://user:pass@localhost:5432/task_manager?sslmode=disable`. |
| `PORT` | No | `8080` | TCP port; the server listens on `:<PORT>` on all interfaces. |
| `DATABASE_MAX_OPEN_CONNS` | No | `10` | Maximum open connections in the pool. |
| `DATABASE_MAX_IDLE_CONNS` | No | `5` | Maximum idle connections retained. |
| `DATABASE_CONN_MAX_LIFETIME` | No | `30m` | Maximum lifetime of a pooled connection (Go duration). |
| `LOG_LEVEL` | No | `info` | One of `debug`, `info`, `warn`, `error`. Any other value falls back to `info`. |
| `SERVER_READ_HEADER_TIMEOUT` | No | `5s` | Deadline for reading request headers. |
| `SERVER_READ_TIMEOUT` | No | `15s` | Deadline for reading the whole request. |
| `SERVER_WRITE_TIMEOUT` | No | `15s` | Deadline for writing the response. |
| `SERVER_IDLE_TIMEOUT` | No | `60s` | How long an idle keep-alive connection is kept open. |
| `GIN_MODE` | No | `debug` | Read by Gin itself, not by `internal/infra/config`. Set to `release` to silence Gin's debug output and route registration logs. |

Behavior worth knowing:

- **Empty values are treated as unset.** Setting `PORT=` behaves like leaving
  it out.
- **Invalid numeric or duration values do not fail startup.** An unparseable
  `DATABASE_MAX_OPEN_CONNS` or `SERVER_READ_TIMEOUT` silently falls back to
  the default. Only `DATABASE_URL` is validated.
- **`DATABASE_URL` is required.** An empty DSN aborts startup with
  `database: DSN is required`.

### Using `.env.example`

[`.env.example`](.env.example) documents the supported variables. The service
does **not** read `.env` files itself; the file is a template for your shell
environment. Copy it, fill in real values, and export it before starting:

```bash
cp .env.example .env
# edit .env: set DATABASE_URL for your PostgreSQL instance

set -a && . ./.env && set +a
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
module, run `go mod download` and retry. `.env.example` is not required for
the build.

### Run the service

```bash
make run
# equivalent:
go run ./cmd/task-manager
```

A successful start logs the following, then keeps running (Gin also prints
its route registration and debug-mode warning unless `GIN_MODE=release` is
set):

```
level=INFO msg="task-manager initialized" addr=:8080
level=INFO msg="server listening" addr=:8080
```

Verify it from another terminal:

```bash
curl -i http://localhost:8080/health
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

Press `Ctrl+C` (or send `SIGTERM`). The server stops accepting requests,
drains in-flight requests for up to 10 seconds, logs `shutting down server`,
closes the database pool, and exits.

There is no `make stop` target; the service runs in the foreground.

### Database availability

The service does not create a database, table, or migration. Any reachable
PostgreSQL database satisfies `/health`; the ping does not select a schema.
Before starting, confirm the instance is reachable, for example:

```bash
psql "$DATABASE_URL" -c 'select 1'
```

Current behavior when the database is unavailable:

- **`DATABASE_URL` missing or unparseable:** startup fails before the HTTP
  server binds, `main` logs `initialize: ...` and exits with status 1.
- **`DATABASE_URL` valid but the server is down:** startup **succeeds**,
  because the pool is created lazily and never dialed during
  `Initialize`. The process listens normally and `GET /health` returns
  `503` until the database becomes reachable.

### Docker

There is no `Dockerfile`, `docker-compose.yml`, or container-based workflow in
this repository. Run the service with the Go toolchain as shown above.

## Testing and Quality Checks

All test commands below were verified against this tree. The configuration
tests read the process environment directly, so run them with no overriding
`PORT`, `DATABASE_URL`, `DATABASE_MAX_OPEN_CONNS`, `SERVER_*`, or `LOG_LEVEL`
values in your shell — a preset `PORT` makes `TestLoadDefaults` fail. Use
`env -u PORT` for a clean run if needed.

| Task | Command |
| --- | --- |
| All tests | `make test` or `go test ./...` |
| One package | `go test ./internal/infra/config` |
| Verbose output | `go test -v ./...` |
| Coverage summary | `go test -cover ./...` |
| Coverage profile | `go test -coverprofile=coverage.out ./...` |
| Browse coverage report | `go tool cover -html=coverage.out` |
| Vet | `make vet` or `go vet ./...` |
| Formatting check | `make fmt` or `gofmt -l .` |
| Apply formatting | `gofmt -w .` |
| Build the binary | `make build` or `go build -o task-manager ./cmd/task-manager` |
| Compile every package | `go build ./...` |

Notes:

- `make fmt` and `gofmt -l .` only **list** files whose formatting differs.
  They neither rewrite files nor fail the command. Use `gofmt -w .` to fix
  them, and check for non-empty output in scripts or CI.
- `coverage.out` matches the `*.out` pattern in `.gitignore`.
- The unit tests do not require PostgreSQL. `database.New` is tested for DSN
  validation and pool limits without dialing; the health handler is tested
  through an HTTP recorder with a stub `HealthChecker`.
- `cmd/task-manager` and `internal/app` have no test files.

## Makefile Reference

The Makefile is a thin wrapper over the Go toolchain. It declares
`BINARY ?= task-manager` and `CMD := ./cmd/task-manager`.

| Target | Purpose | Example | Prerequisites |
| --- | --- | --- | --- |
| `build` | Compile the service into `$(BINARY)` (default: `./task-manager`). | `make build` | Go toolchain |
| `run` | Run the service in the foreground via `go run`. | `make run` | `DATABASE_URL` in the environment |
| `test` | Run `go test ./...`. | `make test` | Go toolchain |
| `vet` | Run `go vet ./...`. | `make vet` | Go toolchain |
| `fmt` | List files that `gofmt` would change; does not modify them. | `make fmt` | Go toolchain |

Every target is listed in `.PHONY`. There are no `clean`, `cover`, `lint`,
`migrate`, or `docker` targets; use the direct Go commands from
[Testing and Quality Checks](#testing-and-quality-checks) for those
workflows.

## API

Only one endpoint exists. There are no task endpoints yet.

### `GET /health`

Reports whether the process is up and whether PostgreSQL responds to a ping.
The database ping uses a 2-second timeout derived from the request context.

**Response — healthy (HTTP 200)**

```json
{"status":"ok"}
```

**Response — database unreachable or ping timed out (HTTP 503)**

```json
{"error":"database unreachable","status":"error"}
```

Notes:

- The response contains no version, uptime, or per-dependency detail.
- The check is shallow: it verifies connectivity only, not schema or data.
- If the server were constructed without a database handle (used only in
  tests), `/health` returns `200 {"status":"ok"}`.
- Other methods on `/health` and any other path return Gin's `404` response.

## Development Conventions

### Adding a feature

The next feature is the task aggregate. It should follow the same shape the
service already uses for infrastructure:

```
internal/task/
├── domain/       entities and value types, no framework imports
├── service/      use cases; declares the repository port it needs
├── handler/      driving adapter: HTTP/Gin, request/response mapping
└── repository/   driven adapter: explicit SQL against *sql.DB
```

Rules that keep the architecture intact:

1. **Declare ports where they are consumed.** A repository interface belongs
   beside the service that calls it, not in a shared interface package and not
   in the repository package. `HealthChecker` in `internal/infra/server` is the
   existing example.
2. **Keep the core free of transport and driver imports.** `domain` and
   `service` must not import Gin, `net/http`, `database/sql`, or `pgx`.
3. **Wire everything in `internal/app`.** Feature constructors are called
   there and passed inward as ports. No other package imports config,
   database, and server together.
4. **Keep `main` trivial.** It only constructs the app, calls `Initialize` and
   `Start`, and handles returned errors.
5. **Test at the port boundary.** Service tests use stub ports; handler tests
   use `httptest` and a stub use case; repository tests are the only ones that
   need a real database.
6. **Return errors, do not panic.** Panics belong only to programmer bugs that
   Gin's recovery middleware should turn into a 500; configuration and startup
   problems are returned as errors.

When a new architectural choice is made — a migration strategy, an ORM
replacement, a readiness/liveness split — record it in `/decisions` rather
than only in code comments.

## Architecture Decisions

Architecture Decision Records live in [`decisions/`](decisions/README.md).
Each record captures the context, the decision, the alternatives that were
considered, and the resulting trade-offs.

| ADR | Decision |
| --- | --- |
| [0001](decisions/0001-clean-architecture.md) | Adopt Clean Architecture and its dependency rule as the service architecture. |
| [0002](decisions/0002-package-layout-and-composition-root.md) | Feature-oriented package layout and a single composition root that owns wiring. |
| [0003](decisions/0003-postgres-via-database-sql.md) | Use `database/sql` with pgx instead of an ORM or a shared `Database` port. |
| [0004](decisions/0004-startup-errors-and-health-endpoint.md) | Return startup errors to the process edge and expose a shallow, database-backed health endpoint. |
