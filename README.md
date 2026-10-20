# Task Manager

Task Manager is a small Go HTTP service for creating, reading, updating, and
deleting tasks. Tasks are persisted in PostgreSQL; there is no in-memory
fallback. The HTTP API is backed by Gin, the use cases live behind a
service-owned repository interface, and the only repository adapter is an
explicit-SQL PostgreSQL implementation.

The service is deliberately fail-closed: without `DATABASE_URL`, without a
reachable PostgreSQL server, or without the expected schema it refuses to start.
The schema is versioned and embedded in the binary, but migrations are applied
by a separate command, never by application startup.

An OpenAPI 3.0.3 contract and an offline Swagger UI are served by the service.
Prometheus metrics are exposed at `GET /metrics`, and every request is
correlated by an `X-Request-ID` that appears in the response and in the
structured request log. A multi-stage Docker image and a
[Compose environment](#9-docker-compose) run the complete stack locally.

## 1. Overview

### Implemented functionality

| Area | Status |
| --- | --- |
| Environment configuration with defaults | Implemented |
| PostgreSQL pool construction and DSN validation | Implemented |
| Startup connectivity and schema verification (fail closed) | Implemented |
| Versioned, embedded SQL migrations applied by `cmd/migrate` | Implemented |
| HTTP server with recovery, request-ID correlation, and request logging | Implemented |
| Prometheus metrics at `GET /metrics` | Implemented |
| `GET /health` readiness and `GET /health/live` liveness | Implemented |
| Graceful shutdown on `SIGINT` / `SIGTERM` | Implemented |
| Task domain model, statuses, validation, and UUID identifiers | Implemented |
| Task service use cases behind a repository interface | Implemented |
| PostgreSQL task repository using explicit SQL | Implemented |
| CRUD endpoints: `POST /task` and `GET/PUT/DELETE /tasks` | Implemented |
| Uniform JSON error envelope and `404` / `405` handling | Implemented |
| OpenAPI 3.0.3 contract and embedded Swagger UI | Implemented |
| Unit, service, HTTP, PostgreSQL integration, and docs tests | Implemented |
| GitHub Actions CI with PostgreSQL, coverage gate, and image build | Implemented |
| Multi-stage Docker image and Docker Compose environment | Implemented |
| Authentication, authorization, and multi-tenancy | Not implemented |
| Filtering, pagination, sorting controls, and search | Not implemented |
| Task timestamps, priorities, tags, or soft delete | Not implemented |
| Metrics storage, dashboards, and alerting | Not implemented (the service exposes `/metrics`; no Prometheus server or Grafana is bundled) |
| Distributed tracing (OpenTelemetry, spans, exporters) | Not implemented (requests are correlated within this process, not traced across services) |

### Technology stack

| Concern | Choice |
| --- | --- |
| Language | Go 1.26.2, declared in [`go.mod`](go.mod) |
| HTTP | [Gin](https://github.com/gin-gonic/gin) v1.12.0 |
| Database access | `database/sql` with the [pgx](https://github.com/jackc/pgx) v5 stdlib driver |
| Database | PostgreSQL; Compose pins `postgres:16.4-alpine` |
| Migrations | Embedded `NNNN_description.sql` files applied by `cmd/migrate` |
| API documentation | OpenAPI 3.0.3 at [`api/openapi.yaml`](api/openapi.yaml); embedded [Swagger UI](https://swagger.io/tools/swagger-ui/) via `swaggest/swgui` |
| Containers | Multi-stage [`Dockerfile`](Dockerfile) (Alpine runtime, non-root) and [`compose.yaml`](compose.yaml) |
| Logging | `log/slog` with a text handler; one structured line per request, including `request_id` |
| Metrics | [Prometheus Go client](https://github.com/prometheus/client_golang) with a dedicated registry, served at `/metrics` |
| Request correlation | `X-Request-ID` propagation or generation; identifier stored in the Gin context |
| Configuration | Standard-library `os.Getenv`, read once at startup |

No ORM, external migration tool, or hosted documentation service is required.

### Scope and limitations

This is a single-aggregate REST service. The task resource has four fields,
`id`, `title`, `description`, and `status`, and there are no timestamps or
ownership fields. The API is unauthenticated and has no pagination, filtering,
sorting, search, or transition rules for status values.

The service is intended to run against PostgreSQL. It has no volatile storage
mode, no automatic database provisioning, and no migration-at-startup behavior.
Production concerns such as authentication, authorization, rate limiting,
metrics storage and dashboards, distributed tracing, and horizontal
coordination are outside the current scope. The metrics endpoint is
pull-based and in-process: the service exposes numbers, it does not store,
aggregate, or alert on them.

## 2. Prerequisites

Choose one of the two setup paths:

- **Docker path:** Docker Engine with Docker Compose v2. Compose supplies
  PostgreSQL, runs migrations, and starts the API.
- **Local path:** Go and a PostgreSQL server, plus `make` if you want to use the
  Makefile targets.

Tool requirements:

| Tool | Requirement | Needed for |
| --- | --- | --- |
| Go | 1.26.2 or a compatible newer toolchain; `go.mod` declares `go 1.26.2` | Building and running locally |
| PostgreSQL | A reachable server; the repository is verified locally against 14.13 and Compose pins 16.4 | Runtime, migrations, integration tests |
| `make` | Optional | Makefile wrappers; every target has a direct `go` equivalent |
| Docker + Compose v2 | Optional | Image and local Compose environment |
| `curl` | Optional | The verification commands below |
| Node.js and npm | Optional | Only the Redocly OpenAPI lint command |

Create the local databases used by the local setup:

```bash
createdb task_manager
createdb task_manager_test
```

`task_manager` is the runtime database. `task_manager_test` is for integration
tests; its name must contain `test` because the repository tests truncate the
`tasks` table, and the migration test likewise refuses a non-test database.

## 3. Quick Start

### Option A: Docker Compose

This path needs no local Go toolchain, database, or manual migration. Compose
starts PostgreSQL, waits for its health check, runs the one-shot `migrate`
service, and only then starts `app`:

```bash
docker compose up -d --build
docker compose ps          # wait until the app service is healthy
```

Migrations are applied automatically by the `migrate` service; the API never
migrates at startup. The local Go path in Option B requires the explicit
`make migrate` step instead.

Wait until the `app` service is healthy, then verify it:

```bash
curl -s http://localhost:8080/health
# {"database":"ok","status":"ok"}

curl -s http://localhost:8080/tasks
# [] on a fresh database

curl -s http://localhost:8080/metrics | head
# Prometheus text exposition: requests_total, request_latency_histogram, ...

curl -si http://localhost:8080/health/live | grep -i '^x-request-id'
# X-Request-ID: 6f1c... (generated when the request did not supply one)
```

Open Swagger UI and the raw contract on the running service:

| URL | Content |
| --- | --- |
| <http://localhost:8080/swagger/index.html> | Swagger UI |
| <http://localhost:8080/swagger/> | Swagger entry point; the trailing slash matters |
| <http://localhost:8080/swagger/openapi.yaml> | Raw OpenAPI 3.0.3 document, served as `application/yaml` |

The UI and the specification are embedded in the binary, so no extra service,
CDN, or asset directory is required.

The application always listens on port `8080` inside the container. Compose
publishes it on the host as `${APP_PORT:-8080}`; `APP_PORT` changes only the
host side. If `8080` is already in use:

```bash
APP_PORT=9090 docker compose up -d --build
curl -s http://localhost:9090/health
# Swagger then moves with the host port:
#   http://localhost:9090/swagger/index.html
#   http://localhost:9090/swagger/openapi.yaml
```

Follow logs and stop the stack:

```bash
docker compose logs -f app
docker compose down       # stop containers; preserve the database volume
```

`docker compose down -v` deletes the named database volume and all task data.
Read [Database and Migrations](#7-database-and-migrations) before using it.

### Option B: local Go process

With a local PostgreSQL server running:

```bash
go mod download

export DATABASE_URL='postgres:///task_manager?host=localhost&sslmode=disable'
export TEST_DATABASE_URL='postgres:///task_manager_test?host=localhost&sslmode=disable'

make migrate
make run
```

The DSNs above use the current operating-system user with a local socket and
TCP on `localhost`. For a password-protected server, use a full DSN such as:

```bash
export DATABASE_URL='postgres://user:pass@localhost:5432/task_manager?sslmode=disable'
```

A successful migration prints:

```text
migrate: schema is up to date
```

A successful start logs the initialization and listening messages and keeps
running in the foreground:

```text
level=INFO msg="task-manager initialized" addr=:8080
level=INFO msg="server listening" addr=:8080
```

Gin may also print its debug-mode warning and route registration when
`GIN_MODE` is unset. Set `GIN_MODE=release` to reduce that output.

Verify the service from another terminal:

```bash
curl -s http://localhost:8080/health
curl -s http://localhost:8080/health/live
curl -s http://localhost:8080/tasks
curl -s http://localhost:8080/metrics | head
```

To build a binary instead of running through `go run`:

```bash
make build
./task-manager
```

`make build` writes the ignored `task-manager` binary in the repository root.
Press `Ctrl+C` or send `SIGTERM` to stop either form; the server drains requests
for up to 10 seconds and closes the database pool.

## 4. Configuration

All settings are read once from environment variables. There is no
configuration-file parser, no flag parsing, and no hot reload. The service does
not read `.env` files itself; export variables in the shell, or let Docker
Compose read its own `.env` file for the container environment. Configuration is
loaded in the composition root described by
[ADR 0002](decisions/0002-package-layout-and-composition-root.md).

| Variable | Required | Default | Purpose |
| --- | --- | --- | --- |
| `DATABASE_URL` | **Yes** | *(none)* | PostgreSQL connection string accepted by pgx/libpq, for example `postgres://user:pass@localhost:5432/task_manager?sslmode=disable`. |
| `PORT` | No | `8080` | TCP port; the HTTP server listens on `:<PORT>` on all interfaces. |
| `SERVER_READ_HEADER_TIMEOUT` | No | `5s` | Deadline for reading request headers. |
| `SERVER_READ_TIMEOUT` | No | `15s` | Deadline for reading the complete request. |
| `SERVER_WRITE_TIMEOUT` | No | `15s` | Deadline for writing the response. |
| `SERVER_IDLE_TIMEOUT` | No | `60s` | How long an idle keep-alive connection is retained. |
| `DATABASE_MAX_OPEN_CONNS` | No | `10` | Maximum open connections in the PostgreSQL pool. |
| `DATABASE_MAX_IDLE_CONNS` | No | `5` | Maximum idle connections retained. |
| `DATABASE_CONN_MAX_LIFETIME` | No | `30m` | Maximum lifetime of a pooled connection. |
| `LOG_LEVEL` | No | `info` | `debug`, `info`, `warn`, or `error`; any other value falls back to `info`. |
| `METRICS_TASKS_REFRESH_INTERVAL` | No | `15s` | How often `tasks_count` is re-read from PostgreSQL. Zero, negative, and unparsable values fall back to `15s`. |
| `TEST_DATABASE_URL` | No | *(empty)* | Test-only DSN for integration tests; the service ignores it. |
| `GIN_MODE` | No | `debug` | Read by Gin, not by the application configuration. `release` silences Gin's debug output. |

Behavior worth knowing:

- Empty optional values are treated as unset. For example, `PORT=` behaves like
  leaving `PORT` out.
- Invalid numeric or duration values fall back to their defaults rather than
  failing startup. A non-positive `METRICS_TASKS_REFRESH_INTERVAL` is replaced
  by `15s` because a non-positive ticker interval would panic. `DATABASE_URL`
  is different: an empty or malformed value aborts startup before the HTTP
  listener binds.
- `DATABASE_URL` is validated with `pgx.ParseConfig`; the process pings the
  server and verifies the `tasks` table before serving requests.

### Docker Compose variables

`compose.yaml` reads these optional variables:

| Variable | Default | Purpose |
| --- | --- | --- |
| `POSTGRES_USER` | `task_manager` | Role created in the Compose PostgreSQL container and used in its DSN. |
| `POSTGRES_PASSWORD` | `task_manager` | Local-development password for that role. Do not reuse it elsewhere. |
| `POSTGRES_DB` | `task_manager` | Database created and migrated in the container. |
| `APP_PORT` | `8080` | Host port published for the API. The container always listens on `8080`. |
| `LOG_LEVEL` | `info` | Passed through to the API container. |

Inside Compose, `DATABASE_URL` is assembled from the PostgreSQL variables and
points at the service name `postgres` on port `5432`. PostgreSQL is not
published on the host; inspect it through the container:

```bash
docker compose exec postgres psql -U task_manager -d task_manager
```

Substitute your `POSTGRES_USER` and `POSTGRES_DB` values if you overrode them.

### Using `.env.example`

[`.env.example`](.env.example) is a starting template for the core runtime and
test variables. It is not read by the service. For a local process, copy and
edit it, then export it:

```bash
cp .env.example .env
# Edit DATABASE_URL and TEST_DATABASE_URL for your PostgreSQL instance.
set -a && . ./.env && set +a
make migrate
make run
```

`.env` is ignored by Git and must not be committed. The values in
`.env.example` are placeholders; real credentials belong in your shell,
Compose environment, or a secret manager. For a one-off override:

```bash
PORT=9090 make run
```

## 5. API Reference

The task and health endpoints use JSON request and response bodies. The
documentation endpoints serve YAML and HTML.

### Documentation

The service exposes:

| Path | Content |
| --- | --- |
| `/swagger/` | Interactive Swagger UI |
| `/swagger/index.html` | The same UI, without a redirect |
| `/swagger/openapi.yaml` | The canonical OpenAPI 3.0.3 document, served as `application/yaml` |
| `/swagger/<asset>` | Embedded CSS, JavaScript, and favicon assets |

The UI and assets are compiled into the binary. There is no CDN, external
origin, or runtime asset directory. Only `GET` is registered for these paths;
other methods return `405` with the normal error envelope. `/swagger` redirects
to `/swagger/`, and unknown documentation assets return `404` from the embedded
file server.

[`api/openapi.yaml`](api/openapi.yaml) is the canonical contract for every task
and health route, including status codes, schemas, defaults, examples, and the
error envelope. Update it in the same change as any route, payload, status, or
header change. It is hand-written and has no generation step. The task contract
is recorded in [ADR 0005](decisions/0005-task-crud-api.md); the documentation
artifact is recorded in [ADR 0009](decisions/0009-openapi-contract.md).

Validate it with Redocly (Node.js/npm, development only):

```bash
npx --yes @redocly/cli@2.66.0 lint api/openapi.yaml
```

The command fails on validation errors. The current document has one advisory
warning because the `info` object has no `license`; the repository does not
declare one. That warning does not make the document invalid.

### Task endpoints

Base URL for the examples: `http://localhost:8080`.

| Method | Path | Purpose | Success | Request body |
| --- | --- | --- | --- | --- |
| `POST` | `/task` | Create a task | `201` with the task and a `Location: /tasks/{id}` header | Required |
| `GET` | `/tasks` | List every task ordered by `id` ascending | `200` with a JSON array, `[]` when empty | None |
| `GET` | `/tasks/{id}` | Retrieve one task | `200` with the task | None |
| `PUT` | `/tasks/{id}` | Replace the mutable fields of a task | `200` with the replaced task | Required |
| `DELETE` | `/tasks/{id}` | Delete a task | `204` with no body | None |

Task representation:

```json
{
  "id": "6ba7b810-9dad-41d1-80b4-00c04fd430c8",
  "title": "Write the report",
  "description": "Cover the API contract",
  "status": "todo"
}
```

Fields and rules:

| Field | Type | Rules |
| --- | --- | --- |
| `id` | string | Server-generated version 4 UUID in canonical form. Clients never send it. |
| `title` | string | Required. Leading and trailing whitespace is trimmed. Empty or whitespace-only titles are rejected. |
| `description` | string | Optional; defaults to `""`. Stored as sent, without trimming. |
| `status` | string | One of `todo`, `in_progress`, `done`. Defaults to `todo` when omitted. |

Additional behavior:

- Request bodies are decoded strictly. Unknown fields, trailing content, wrong
  types, empty bodies, and JSON arrays return `400 invalid_request`.
- `PUT` is a full replacement. `title` is required, omitted `description`
  becomes `""`, and omitted `status` resets to `todo`. A missing task is never
  created by `PUT`.
- A malformed `{id}` returns `400 validation_error`; a well-formed UUID that is
  not stored returns `404 not_found`.
- `GET /tasks` returns every row ordered by `id` ascending. The order is stable
  across calls but is not creation order.
- No authentication, authorization, filtering, pagination, or sorting controls
  are implemented.

#### Create a task

```bash
curl -i -X POST http://localhost:8080/task \
  -H 'Content-Type: application/json' \
  -d '{"title":"Write the report","description":"Cover the API contract","status":"in_progress"}'
```

```http
HTTP/1.1 201 Created
Location: /tasks/6ba7b810-9dad-41d1-80b4-00c04fd430c8
Content-Type: application/json; charset=utf-8
```

```json
{
  "id": "6ba7b810-9dad-41d1-80b4-00c04fd430c8",
  "title": "Write the report",
  "description": "Cover the API contract",
  "status": "in_progress"
}
```

#### List, get, replace, and delete

```bash
curl -i http://localhost:8080/tasks

curl -i http://localhost:8080/tasks/6ba7b810-9dad-41d1-80b4-00c04fd430c8

curl -i -X PUT http://localhost:8080/tasks/6ba7b810-9dad-41d1-80b4-00c04fd430c8 \
  -H 'Content-Type: application/json' \
  -d '{"title":"Report published","description":"Done","status":"done"}'

curl -i -X DELETE http://localhost:8080/tasks/6ba7b810-9dad-41d1-80b4-00c04fd430c8
```

`DELETE` returns `204 No Content`. A later `GET` for the same ID returns `404`,
and the row is removed from PostgreSQL.

### Error responses

Task failures and route-level `404`/`405` responses use one envelope:

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

`fields` is present only when a validation failure can be attributed to specific
fields. The message is safe to display and never contains SQL, constraint names,
stack traces, or other internal details.

| Status | `code` | When |
| --- | --- | --- |
| `400` | `invalid_request` | Body is not a single valid JSON object, contains unknown fields, or has wrong field types. |
| `400` | `validation_error` | `title` is missing or blank, `status` is not allowed, or `{id}` is not a UUID. |
| `404` | `not_found` | A well-formed task ID does not exist, or a route is unknown. |
| `405` | `method_not_allowed` | The path exists but does not support the HTTP method. |
| `500` | `internal_error` | Unexpected failure, including a database error. Details are logged, not returned. |

### Health endpoints

`GET /health` is the readiness probe. It pings PostgreSQL with a two-second
budget and reports readiness only when the ping succeeds:

```json
{"database":"ok","status":"ok"}
```

It returns `503` with `{"status":"error","error":"database unreachable"}` when
the database cannot be reached or the ping times out. `database not configured`
is only reachable in tests that construct the server without a checker;
`internal/app` always supplies the pool.

`GET /health/live` is the liveness probe. It does not touch the database:

```json
{"status":"ok"}
```

The readiness check verifies connectivity only, not schema or data. Schema
presence is verified once, at startup. Undefined methods on a known health path
return `405`; unknown routes return `404`, both with the error envelope.

### Metrics endpoint

`GET /metrics` returns the standard Prometheus text exposition format
(`text/plain; version=0.0.4`) from a registry dedicated to this service. It is
not a JSON endpoint and is deliberately absent from `api/openapi.yaml`; the
design is recorded in
[ADR 0010](decisions/0010-prometheus-metrics-and-request-correlation.md).

```bash
curl -s http://localhost:8080/metrics
curl -s http://localhost:8080/metrics | grep '^tasks_count'
```

Representative excerpt (series appear once they have been observed):

```text
# HELP requests_total Total number of HTTP requests handled, by method, route, and status code.
# TYPE requests_total counter
requests_total{method="GET",route="/tasks/:id",status="200"} 3
# HELP request_latency_histogram HTTP request duration in seconds, by method, route, and status code.
# TYPE request_latency_histogram histogram
request_latency_histogram_bucket{method="GET",route="/tasks/:id",status="200",le="0.01"} 3
...
# HELP tasks_count Current number of tasks persisted in PostgreSQL.
# TYPE tasks_count gauge
tasks_count 2
# HELP tasks_count_last_refresh_timestamp_seconds Unix timestamp of the last successful tasks_count refresh.
# TYPE tasks_count_last_refresh_timestamp_seconds gauge
tasks_count_last_refresh_timestamp_seconds 1.729500123e+09
```

| Metric | Type | Labels | Unit | Meaning |
| --- | --- | --- | --- | --- |
| `requests_total` | Counter | `method`, `route`, `status` | requests | Completed HTTP requests, including `4xx` and `5xx`. |
| `request_latency_histogram` | Histogram | `method`, `route`, `status` | seconds | Request duration, measured from the start of the middleware to completion. |
| `tasks_count` | Gauge | none | tasks | Last successfully observed `count(*)` of the `tasks` table. |
| `tasks_count_last_refresh_timestamp_seconds` | Gauge | none | Unix seconds | When `tasks_count` was last refreshed successfully. |

Label and measurement rules:

- `route` is the registered Gin pattern (`/tasks/:id`), never the concrete URL,
  so task IDs do not create series. Requests that match no route, including
  `404` and `405` responses, share the single value `unmatched`.
- `method` is a standard HTTP verb; any other token is recorded as `other`.
- `status` is the numeric HTTP status code as a string.
- Latency uses seconds and buckets from `0.001` to `10`. Both successes and
  failures are recorded.
- Scrapes of `/metrics` are excluded from `requests_total` and
  `request_latency_histogram`. The endpoint is still request-logged, and
  `promhttp`'s own handler metrics are not registered, so a scrape cannot
  instrument itself.

`tasks_count` consistency model:

- It comes from PostgreSQL (`SELECT count(*) FROM tasks`), not from a
  process-local counter. Creates, deletes, restarts, and writes from another
  process or a manual statement are all reflected at the next refresh.
- It is refreshed at startup and then every
  `METRICS_TASKS_REFRESH_INTERVAL` (default `15s`), never per request. It is
  therefore eventually consistent, up to one interval plus one query duration
  behind.
- Before the first successful refresh, `tasks_count` and its timestamp are
  absent rather than reporting `0`.
- If PostgreSQL becomes unavailable, the last successful value and timestamp
  remain (stale, not fabricated), a warning is logged on each failed refresh,
  and the timestamp stops advancing. Use
  `time() - tasks_count_last_refresh_timestamp_seconds` to detect staleness.
- Refreshes are serialized, and shutdown cancels an in-flight count and waits
  for the refresh loop before closing the pool.

`/metrics` is unauthenticated because the whole API is. It exposes aggregate
numbers and registered route patterns, never task titles, descriptions,
request bodies, or credentials. Restrict it at the network or proxy layer if
the service is reachable from an untrusted network.

### Request correlation

The service assigns every request an identifier and echoes it in the
`X-Request-ID` response header:

```bash
curl -si -H 'X-Request-ID: client-trace-42' http://localhost:8080/health/live \
  | grep -i '^x-request-id'
# X-Request-ID: client-trace-42

curl -si http://localhost:8080/health/live | grep -i '^x-request-id'
# X-Request-ID: 9f2c4d... (32 lowercase hex characters, generated)
```

- A valid incoming `X-Request-ID` is propagated. Valid means 1–64 characters
  from letters, digits, `.`, `_`, `:`, and `-`, starting with a letter or digit.
- A missing, empty, malformed, or over-long value is replaced with a random
  128-bit identifier encoded as 32 hex characters.
- The final value is stored in the Gin context and available to downstream
  handlers and middleware through `server.RequestID(c)` (or
  `c.GetString(server.RequestIDContextKey)`).
- Each structured request log line carries `request_id`, `method`, `path`,
  `route`, `status`, `duration`, and `bytes`. Panics still produce a completion
  log line and metric before `gin.Recovery` writes the `500`; the log records
  `panic=true` without the panic value or stack.
- This is request correlation within one process, **not distributed tracing**:
  there are no spans, sampling, trace export, or propagation into PostgreSQL or
  other services.

## 6. Architecture and Design

Task Manager uses **Clean Architecture** as its architectural style. The
dependency rule is the important part: domain and application logic must not
depend on Gin, `net/http`, `database/sql`, or pgx. HTTP and PostgreSQL are
adapters around a framework-free core.

The rationale and rejected alternatives are recorded in
[ADR 0001](decisions/0001-clean-architecture.md); package layout and composition
are recorded in [ADR 0002](decisions/0002-package-layout-and-composition-root.md).

### Dependency direction

```text
HTTP client
    |
    v
internal/infra/server (Gin router, health, 404/405)
    |
    v
internal/task/handler (HTTP driving adapter)
    |
    v
internal/task/service (use cases)
    |
    v
service.Repository (interface declared by the service)
    ^
    |
internal/task/repository/postgres (driven adapter, explicit SQL)
```

`internal/task/domain` holds the aggregate and invariants and has no framework
or driver imports. The docs adapter, `internal/infra/swagger`, depends on the
embedded contract in `api` and on Gin; feature packages do not depend on it.
The observability adapter, `internal/infra/metrics`, depends on the Prometheus
client but not on Gin; the HTTP package calls it through a narrow
`server.Instrumentation` interface and mounts its handler. Only the PostgreSQL
repository knows how to count persisted tasks.

### Package responsibilities

| Package | Responsibility |
| --- | --- |
| `cmd/task-manager` | Process entry point; constructs the app, calls `Initialize` and `Start`, and handles returned errors. |
| `cmd/migrate` | Applies embedded migrations and exits non-zero on failure. |
| `internal/app` | Composition root: loads config, builds the logger and pool, verifies the schema, composes HTTP modules through `buildHandlers`, builds the server, and shuts down in reverse order. |
| `internal/infra/config` | Reads environment variables into plain structs and applies defaults. No network or filesystem I/O. |
| `internal/infra/database` | Validates the DSN, opens the pooled `*sql.DB`, embeds and applies migrations, and verifies required tables. |
| `internal/infra/server` | Builds the Gin router, request-ID and completion middleware, health and metrics endpoints, feature modules, and `404`/`405` handlers; owns HTTP lifecycle. Declares the narrow `HealthChecker`, `RouteRegistrar`, and `Instrumentation` interfaces. |
| `internal/infra/handler` | Shared HTTP delivery code: the JSON error envelope and error writer. |
| `internal/infra/metrics` | Prometheus collectors in a dedicated registry, the `tasks_count` refresher, and the exposition handler. No Gin import; the registry is private. |
| `internal/infra/swagger` | Docs adapter: serves the embedded spec and Swagger UI under `/swagger`; registers only `GET`. |
| `api` | Embeds `api/openapi.yaml` as `api.OpenAPI`; no business or transport logic. |
| `internal/task/domain` | `Task`, the closed `Status` set, validation, and UUID identifier format and generation. |
| `internal/task/service` | Create, list, get, update, and delete use cases; declares the `Repository` interface it consumes. |
| `internal/task/handler` | Strict request decoding, path-ID validation, response mapping, and error mapping; declares the `TaskService` interface it consumes. |
| `internal/task/repository/postgres` | Explicit SQL CRUD, identifier generation, `sql.ErrNoRows` to `service.ErrNotFound`, deterministic list order, and the `tasks` row count used by metrics. |

### Wiring and startup

`cmd/task-manager/main.go` only constructs `app.New()` and calls `Initialize`,
then `Start`. The composition root performs the actual wiring:

1. `config.Load()` reads the environment and applies defaults.
2. A `slog` text logger writes to stdout at the configured level.
3. `DATABASE_URL` is required. An empty value aborts startup.
4. `database.New` validates the DSN and opens the pool without dialing.
5. `Initialize` pings with a five-second budget and calls
   `database.VerifySchema`, which requires the `tasks` table. A failure closes
   the pool and stops startup.
6. The task repository is constructed once and shared by the metrics refresher
   and the task module.
7. `metrics.New` builds a dedicated Prometheus registry, and a
   `TaskCountRefresher` is wired to the task repository with the configured
   refresh interval.
8. `buildHandlers` (in `internal/app/wiring.go`) composes the HTTP modules —
   the task handler over the repository port and the docs adapter — and returns
   them as `server.RouteRegistrar` values.
9. `server.New` receives the pool as the health checker, the metrics adapter as
   `Instrumentation`, and the registrars returned by `buildHandlers`. It builds
   the router with `gin.Recovery()`, request-ID and completion middleware,
   health routes, `/metrics`, `/swagger/`, and feature routes.
10. `Start` installs a signal context for `SIGINT`/`SIGTERM`, starts the
    task-count refresher, runs the server, shuts it down with a ten-second
    budget, stops and waits for the refresher, and closes the pool.

The server never imports the task packages or the database adapter. It knows
only the narrow interfaces it declares itself:

```go
type HealthChecker interface {
	PingContext(ctx context.Context) error
}

type RouteRegistrar interface {
	RegisterRoutes(r gin.IRouter)
}
```

`*sql.DB` satisfies `HealthChecker`; the task handler and docs adapter satisfy
`RouteRegistrar`. `internal/task/service` declares `Repository`, and
`internal/task/handler` declares `TaskService`, so each consumer owns its
interface.

### Rules for new code

The task slice is the model for new features:

```text
internal/task/
├── domain/              entities, value types, invariants; no framework imports
├── service/             use cases; declares the repository interface it consumes
├── handler/             driving adapter: Gin, request/response mapping
└── repository/postgres/ driven adapter: explicit SQL
```

1. Declare interfaces where they are consumed, not in a shared interface package.
2. Keep domain and application packages free of transport and driver imports.
3. Wire concrete adapters only in `internal/app`. Register a new HTTP module
   in `buildHandlers` (`internal/app/wiring.go`); do not add module-specific
   arguments to `server.New`.
4. Keep `main` trivial.
5. Test at the interface boundary with hand-written fakes; keep SQL tests in the
   integration tier.
6. Return errors for configuration and startup failures; let Gin's recovery
   middleware handle programmer panics.
7. Use `internal/infra/handler` and the documented error codes for new HTTP
   errors.
8. Change the schema with a new `NNNN_` migration; never edit an applied one.
9. Update `api/openapi.yaml` in the same change as any API contract change.
10. Keep instrumentation in the infrastructure layer: handlers get request
    metrics and request IDs from the shared middleware. Never import Gin or
    Prometheus from `internal/task/domain` or `internal/task/service`, and do
    not add a second task-count source beside PostgreSQL.

## 7. Database and Migrations

### Schema

The persistence and migration choices are recorded in
[ADR 0003](decisions/0003-postgres-via-database-sql.md) and
[ADR 0006](decisions/0006-postgres-task-persistence.md). PostgreSQL stores one
table:

```sql
CREATE TABLE IF NOT EXISTS tasks (
    id          UUID PRIMARY KEY,
    title       TEXT NOT NULL CHECK (btrim(title) <> ''),
    description TEXT NOT NULL DEFAULT '',
    status      TEXT NOT NULL CHECK (status IN ('todo', 'in_progress', 'done'))
);

CREATE INDEX IF NOT EXISTS tasks_status_idx ON tasks (status);
```

The table contains exactly the API fields. The `CHECK` constraints duplicate
the domain rules so a manual statement or a future write path cannot store a
blank title or an unknown status. `GET /tasks` uses `id` ascending for its
stable order, not creation order.

### Migrations

Migration files live in `internal/infra/database/migrations/` and are embedded
in the binary. They are named `NNNN_description.sql`; lexical order is version
order. Each file contains one additive statement, applied versions are recorded
in `schema_migrations`, and each migration runs in its own transaction under an
advisory lock so concurrent runs cannot apply a version twice.

The current migrations are:

| Version | Statement |
| --- | --- |
| `0001_create_tasks.sql` | Creates the `tasks` table and its constraints. |
| `0002_index_tasks_status.sql` | Creates `tasks_status_idx` on `status`. |

Apply them explicitly:

```bash
make migrate
# equivalent:
go run ./cmd/migrate
```

`make migrate` requires `DATABASE_URL` and exits non-zero on failure. Re-running
it is safe: applied versions are skipped and the command prints
`migrate: schema is up to date`. The service never migrates at startup; if the
`tasks` table is missing, startup fails with an error that tells you to run
`make migrate`.

Inside Compose, migrations are the one-shot `migrate` service:

```bash
docker compose run --rm migrate
docker compose logs migrate
```

### Startup and persistence

`sql.Open` is lazy, so startup explicitly pings with a five-second budget and
then verifies the table with `database.VerifySchema`. A reachable server with a
missing table is not considered ready. There is no in-memory fallback and no
automatic database provisioning.

Tasks are durable and shared by every process pointed at the same database.
They survive process restarts, container restarts, image rebuilds, and
`docker compose down`. Only `docker compose down -v` or an explicit volume
removal discards the Compose database volume.

> **Destructive operation:** `docker compose down -v` deletes the
> `task-manager_pgdata` volume and every stored task. `docker compose down`
> without `-v` preserves the data.

### Integration-test database

The repository and migration tests use `TEST_DATABASE_URL` and skip with an
explicit message when it is unset. The repository tests truncate the `tasks`
table between cases, so the integration suites refuse to run against a database
whose name does not contain `test`:

```bash
export TEST_DATABASE_URL='postgres:///task_manager_test?host=localhost&sslmode=disable'
make test-integration
```

Database-backed packages must run sequentially because they share one database;
the Makefile and CI pass `-p 1`. Never point `TEST_DATABASE_URL` at a database
whose data you need to keep.

## 8. Testing and Quality Checks

The CI and coverage decisions are recorded in
[ADR 0007](decisions/0007-ci-with-postgresql-service.md) and
[ADR 0008](decisions/0008-containers-and-coverage-gate.md). The suite has two
tiers:

- **Database-free tests** cover the domain, service use cases, HTTP handler,
  router, request correlation, metrics, docs adapter, configuration, and startup
  error paths. They run without PostgreSQL.
- **PostgreSQL integration tests** cover the repository, migrations, and full
  app wiring. They run only when `TEST_DATABASE_URL` is set; otherwise they use
  `t.Skip` with an explicit message.

The configuration tests clear the variables they assert on, so a preset `PORT`,
`DATABASE_URL`, or logging variable cannot change their result.

| Task | Command |
| --- | --- |
| Database-free tests | `make test` or `go test ./...` |
| Whole suite including PostgreSQL | `make test-integration TEST_DATABASE_URL='postgres:///task_manager_test?host=localhost&sslmode=disable'` |
| One package | `go test ./internal/task/handler` |
| Verbose output | `go test -v ./...` |
| Race detector against PostgreSQL | `TEST_DATABASE_URL=... go test -race -count=1 -p 1 ./...` |
| Coverage summary | `TEST_DATABASE_URL=... go test -cover ./...` |
| Coverage profile | `TEST_DATABASE_URL=... go test -coverprofile=coverage.out ./...` |
| Browse coverage report | `go tool cover -html=coverage.out` |
| Coverage gate (≥70%, requires PostgreSQL) | `make coverage-check TEST_DATABASE_URL=...` |
| Vet | `make vet` or `go vet ./...` |
| Formatting check | `make fmt` or `gofmt -l .` |
| Apply formatting | `gofmt -w .` |
| Build the binary | `make build` or `go build -o task-manager ./cmd/task-manager` |
| Compile every package | `go build ./...` |
| Apply migrations | `make migrate` or `go run ./cmd/migrate` |
| Validate the OpenAPI document | `npx --yes @redocly/cli@2.66.0 lint api/openapi.yaml` |

Important details:

- `make fmt` and `gofmt -l .` only list files whose formatting differs; they do
  not rewrite files or fail. Use `gofmt -w .` to fix them. CI fails when any file
  is listed.
- `coverage.out` matches `*.out` in `.gitignore`.
- Measured statement coverage on this tree is **77.8%** with PostgreSQL and
  **59.0%** without it. The PostgreSQL adapter only executes in the integration
  tier.
- `make coverage-check` enforces `COVERAGE_MIN` (default `70`) on the combined
  suite and refuses to run without `TEST_DATABASE_URL`. `make coverage` writes a
  profile and prints the total but does not enforce the threshold.
- Service and handler tests use hand-written fakes, not a mocking library. The
  PostgreSQL adapter is verified against a real server, not with SQL mocks.
- `internal/infra/metrics` tests cover success and failure counters, histogram
  units and buckets, bounded method/route labels, exposition output, scrape
  exclusion, and registry isolation. Task-count tests cover the initial value,
  refresh failures (no fabricated zero, stale value retained), cancellation,
  periodic refresh, and serialized concurrent refreshes.
- `internal/infra/server` tests cover generated and propagated request IDs,
  invalid and oversized replacement, the response header on errors, log fields,
  route-pattern labels, and panic recovery. A panicking handler must still
  produce a completion log and a `500` metric observation.
- The PostgreSQL suite includes `CountTasks` against a real table, including a
  row inserted outside the repository, so the gauge's source of truth is
  exercised rather than mocked.
- `internal/app` tests cover a missing `DATABASE_URL`, a malformed DSN, an
  unreachable server, a missing schema, and successful PostgreSQL wiring.
- `internal/infra/swagger` tests cover the exact embedded spec bytes, the UI
  index and `/swagger/index.html` alias, embedded CSS and JavaScript, the
  `GET`-only contract, unknown assets, and the trailing-slash redirect.
- `cmd/task-manager`, `cmd/migrate`, `api`, and `internal/infra/handler` have no
  test files of their own. The envelope is exercised through handler and server
  tests; the embedded spec through the Swagger adapter; migrations through the
  integration suite and CI.
- Redocly is a development-only Node.js tool. It is not a Go dependency and is
  not needed to build, test, or run the service. The current document has one
  advisory warning about its absent `license` field, not a validation error.

### Continuous integration

[`.github/workflows/ci.yml`](.github/workflows/ci.yml) runs on pushes to `main`,
every pull request, and manual dispatch. It provisions a real PostgreSQL service
(`postgres:16.4-alpine`) with an ephemeral `task_manager_test` database, then
runs:

1. A formatting check that fails on any file `gofmt` would change.
2. `go vet ./...` and `go build ./...`.
3. Database-free tests with `TEST_DATABASE_URL` cleared.
4. `go run ./cmd/migrate` with `DATABASE_URL`.
5. `make test-integration` with `TEST_DATABASE_URL`.
6. An assertion that representative repository and migration tests report
   `--- PASS` and that no `--- SKIP` occurred.
7. `go test -race -count=1 -p 1 ./...` against the service database.
8. `make coverage-check`, enforcing the 70% combined-suite threshold.
9. A Docker image build, a non-root UID assertion, and
   `docker compose config --quiet`.

The workflow has read-only repository permissions, uses throwaway service
credentials, and performs no deployment, publishing, or push.

## 9. Docker Compose

`compose.yaml` defines three services under the project name `task-manager`.
The container and Compose decision is recorded in
[ADR 0008](decisions/0008-containers-and-coverage-gate.md).

| Service | Image / command | Responsibility |
| --- | --- | --- |
| `postgres` | `postgres:16.4-alpine` | Stores the `tasks` table and `schema_migrations` in the named `pgdata` volume; health-checked with `pg_isready`. |
| `migrate` | Same app image, entrypoint `/app/migrate` | Waits for PostgreSQL, applies pending migrations, and exits. It runs once, not as a long-lived service. |
| `app` | Same app image, entrypoint `/app/task-manager` | Serves the API after PostgreSQL is healthy and migrations completed successfully. |

Start, inspect, and stop the stack:

```bash
docker compose up -d --build
docker compose ps
docker compose logs -f app
docker compose down
```

The API is published on `${APP_PORT:-8080}`; the container listens on `8080`.
PostgreSQL is not published on the host. The `app` service reaches it at the
service name `postgres:5432` using a DSN assembled from the `POSTGRES_*`
variables. This avoids collisions with a local PostgreSQL on port 5432.

Metrics use the same application port: `curl -s http://localhost:8080/metrics`
(or `:${APP_PORT}` when overridden). No additional port is published, so
PostgreSQL and other internal services stay unreachable from the host.

If the migration fails, the `app` service is not started. Inspect the migration
logs and re-run it:

```bash
docker compose logs migrate
docker compose run --rm migrate
```

Inspect PostgreSQL through its container:

```bash
docker compose exec postgres psql -U task_manager -d task_manager
```

The image is a multi-stage build. The build stage uses
`golang:1.26.2-alpine`; the runtime stage uses `alpine:3.22` and contains only
the two application binaries, `/app/task-manager` and `/app/migrate`. It runs as
UID/GID `10001`, exposes `8080`, and uses an exec-form entrypoint so `SIGTERM`
reaches the service. Its `HEALTHCHECK` calls `/health`, which means unhealthy
when PostgreSQL is unreachable.

PostgreSQL data lives in the named volume `task-manager_pgdata`. It survives
container restarts, `docker compose down`, image rebuilds, and host reboots.
`docker compose down -v` is the explicit destructive command and removes the
volume and all tasks. To build the image without Compose:

```bash
docker build -t task-manager:local .
```

## 10. Design Trade-offs and Limitations

| Decision | Benefit | Cost / limitation |
| --- | --- | --- |
| PostgreSQL is the only task store, with no fallback | Data is durable and every process shares one source of truth | The service cannot run or serve a request without PostgreSQL; local setup needs a database |
| PostgreSQL via `database/sql` and pgx, without an ORM | Explicit SQL, standard-library interfaces, and no ORM mapping layer | SQL is written and maintained by hand; repository behavior is only proven with integration tests |
| UUID identifiers instead of integers | IDs are opaque, unguessable, and generated without coordination | IDs are larger, and list order by UUID is not creation order |
| Strict JSON decoding and full-replacement `PUT` | Typos and contract drift fail loudly; replacement semantics are unambiguous | Clients must send every replacement field, and extra metadata is rejected |
| Fail-closed startup: ping and schema verification | The process never starts half-wired or serves requests that can only return `500` | Startup depends on database availability; there is no retry loop, so an orchestrator must restart it |
| Migrations are explicit and not run by the app | Schema changes are reviewed and controlled independently of deploys | Deployments need a migration step; Compose models this as a one-shot service |
| Hand-written OpenAPI contract with embedded Swagger UI | Clients get a machine-readable contract and offline docs; no CDN or asset directory | The spec can drift from handlers; updates require discipline and linting |
| Pull-based Prometheus metrics in-process | Standard format and no agent or metrics backend is required; any scraper can consume it | The service does not store, aggregate, or alert on the data; `/metrics` is unauthenticated like the rest of the API |
| `tasks_count` refreshed on an interval instead of per request | No table scan on the request path; the value reflects PostgreSQL rather than process memory | Eventually consistent up to one interval; stale but timestamped when PostgreSQL is unavailable, and absent before the first success |
| Request correlation via `X-Request-ID` instead of OpenTelemetry | Cheap, dependency-free log correlation; every response carries the ID | It is not distributed tracing: no spans, sampling, propagation into PostgreSQL, or trace backend |
| 70% coverage gate on the combined suite | The gate measures the PostgreSQL adapter as well as unit tests | It requires a live test database and a longer CI job |
| Small, single-aggregate service | Easy to understand, test, and deploy | No authentication, pagination, filtering, sorting, search, timestamps, or multi-tenancy |

Intentional scope limits are listed in [Overview](#1-overview). They are not
claims that those features are partially implemented.

A few operational constraints are worth repeating:

- The readiness check is shallow: it reports database connectivity, not schema
  state. Schema presence is checked once at startup.
- The API is unauthenticated. Do not expose it directly to an untrusted network
  without adding authentication, authorization, and appropriate network
  controls.
- The OpenAPI linter emits one advisory warning because the repository declares
  no license. The specification is still structurally valid.
- The `coverage.out` profile is ignored by Git and is not a committed artifact.
- `/metrics` reports aggregate values and route patterns, not task content, but
  it has no authentication. Treat it like the rest of the API and restrict it
  at the network edge. It is not part of the OpenAPI document.
- `tasks_count` is not a transactional inventory. Do not use it for billing or
  exact reconciliation; query PostgreSQL when exact state is required.

## 11. Troubleshooting

| Symptom | Likely cause | Action |
| --- | --- | --- |
| `initialize: app: initialize database: DATABASE_URL is required` | `DATABASE_URL` is unset or empty | Export a valid DSN before `make run`; inside Compose, check the `app` service environment |
| `ping: ... connection refused` or `database unreachable` | PostgreSQL is not running, the host/port is wrong, or Compose is using the wrong host | Run `pg_isready`, check `docker compose logs postgres`, and remember that the Compose host is `postgres`, not `localhost` |
| `role "..." does not exist` or authentication failure | The DSN role/database does not exist or the password is wrong | Create the role/database or correct `DATABASE_URL`; do not commit credentials |
| `schema is not initialized: table "tasks" is missing` | Migrations were never applied, or were applied to another database | Run `make migrate` locally or `docker compose run --rm migrate` |
| Integration tests skip with `TEST_DATABASE_URL is not set` | The integration tier was not enabled | Export `TEST_DATABASE_URL`; use `make test-integration` to fail instead of skipping |
| `make coverage-check` refuses to run | `TEST_DATABASE_URL` is unset | Provide the test DSN; the gate intentionally requires the database tier |
| `bind: address already in use` on `:8080` | Another process uses the port | Run `PORT=9090 make run`, or publish `APP_PORT=9090` with Compose |
| `docker compose` cannot connect the app to PostgreSQL | The `migrate` service failed or PostgreSQL is unhealthy | Check `docker compose ps`, `docker compose logs postgres`, and `docker compose logs migrate`; the app waits for both |
| Tasks disappear after `docker compose down` | The volume was removed with `-v` | Use `docker compose down` without `-v`; `down -v` is intentionally destructive |
| Requests return `500` after PostgreSQL restarts | The process is still running but the database is unavailable | Check database health and logs; restart the service once PostgreSQL is ready |
| Swagger UI returns `404` | The path is not under `/swagger/`, or the binary is old | Use `/swagger/` or `/swagger/index.html`; rebuild the binary if the docs route is absent |
| `/metrics` returns `404` | The binary predates the observability phase, or a test server was built without instrumentation | Rebuild/restart the service; `internal/app` always wires the endpoint |
| `tasks_count` is missing from `/metrics` | No refresh has succeeded yet (database down, schema missing, or first refresh still running) | Check `/health`, the application log for `task count refresh failed`, and the refresh interval |
| `tasks_count` looks stale | PostgreSQL became unavailable, or the refresh interval is large | Compare `time()` with `tasks_count_last_refresh_timestamp_seconds`; check database connectivity and `METRICS_TASKS_REFRESH_INTERVAL` |
| Every response has an unexpected `X-Request-ID` | A proxy supplied an invalid or oversized header, so the service replaced it | Expected; the response header always contains the final identifier, which also appears in the request log |
| Redocly reports a missing `license` warning | The repository does not declare a license | This is advisory; validation still succeeds. Do not add a false license just to silence it |

## 12. Project Structure

```text
.
├── api/
│   ├── openapi.go                  embeds the contract as api.OpenAPI
│   └── openapi.yaml                canonical OpenAPI 3.0.3 contract
├── cmd/
│   ├── migrate/                    applies embedded migrations and exits
│   └── task-manager/               process entry point
├── decisions/                      Architecture Decision Records and index
├── internal/
│   ├── app/                        composition root, logger, startup/shutdown
│   ├── infra/
│   │   ├── config/                 environment variables and defaults
│   │   ├── database/               PostgreSQL pool, migrations, schema checks
│   │   ├── handler/                shared HTTP error envelope
│   │   ├── metrics/                Prometheus registry, collectors, task-count refresher
│   │   ├── server/                 Gin router, middleware, request IDs, health, /metrics, 404/405
│   │   └── swagger/                embedded spec and Swagger UI adapter
│   └── task/
│       ├── domain/                 Task aggregate, statuses, validation, UUIDs
│       ├── handler/                HTTP driving adapter
│       ├── repository/postgres/    explicit-SQL driven adapter
│       └── service/                use cases and repository interface
├── .github/workflows/ci.yml        CI with PostgreSQL, tests, coverage, Docker
├── .env.example                    core environment template
├── .dockerignore                   build-context exclusions
├── Dockerfile                      multi-stage, non-root image
├── Makefile                        build, run, migrate, test, coverage, vet, fmt
├── compose.yaml                    PostgreSQL, one-shot migration, API
├── go.mod / go.sum                 module and dependency checksums
└── README.md                       this guide
```

The `coverage.out` profile, `task-manager` binary, and `.env` file are generated
locally and ignored by Git.

## 13. Architecture Decisions

Architecture Decision Records live in [`decisions/`](decisions/README.md). Each
record captures context, the decision, rejected alternatives, trade-offs, and
consequences. Treat the ADRs as the source of design rationale and this README
as the practical guide for setup, operation, and the current API.

| ADR | Decision | Status |
| --- | --- | --- |
| [0001. Clean Architecture as the service architecture](decisions/0001-clean-architecture.md) | Adopt Clean Architecture and its dependency rule, with interfaces declared by their consumers and adapters pointing inward. | Accepted |
| [0002. Feature-oriented package layout and a single composition root](decisions/0002-package-layout-and-composition-root.md) | Organize code by feature and keep all concrete wiring in `internal/app`. | Accepted |
| [0003. PostgreSQL through `database/sql` and pgx, without an ORM or a shared Database port](decisions/0003-postgres-via-database-sql.md) | Use `database/sql` with pgx instead of an ORM or a broad shared database interface; migration strategy added later. | Accepted (migration strategy amended by 0006) |
| [0004. Startup errors are returned to the process edge; `/health` reports database reachability](decisions/0004-startup-errors-and-health-endpoint.md) | Return startup failures to `main`; expose a shallow readiness check and a separate liveness probe. | Accepted (database requirement and health bodies amended by 0005) |
| [0005. Task CRUD on a service-owned repository port](decisions/0005-task-crud-api.md) | Define the task aggregate, a service-owned repository interface, strict HTTP mapping, UUIDs, and the error contract. | Accepted |
| [0006. Task persistence in PostgreSQL: table, versioned migrations, and an explicit-SQL repository](decisions/0006-postgres-task-persistence.md) | Use one `tasks` table, additive embedded migrations, explicit SQL, duplicate database constraints, and real-PostgreSQL tests. | Accepted |
| [0007. CI runs the suite against a real PostgreSQL service and refuses silent skips](decisions/0007-ci-with-postgresql-service.md) | Provision ephemeral PostgreSQL in CI and assert that database tests actually ran. | Accepted (coverage gate and image checks added by 0008) |
| [0008. Containerized local environment and a 70% coverage gate on the combined suite](decisions/0008-containers-and-coverage-gate.md) | Ship a multi-stage image, model migrations as a Compose service, and enforce 70% coverage on the combined suite. | Accepted |
| [0009. A checked-in OpenAPI document as the HTTP contract](decisions/0009-openapi-contract.md) | Make [`api/openapi.yaml`](api/openapi.yaml) the canonical contract and serve it with embedded Swagger UI; no code generation. | Accepted |
| [0010. Prometheus metrics and request correlation for HTTP observability](decisions/0010-prometheus-metrics-and-request-correlation.md) | Expose bounded request metrics, an eventually consistent PostgreSQL-backed task gauge, and `X-Request-ID` correlation from a dedicated registry; no OpenTelemetry. | Accepted |
| [0011. One composition point for HTTP module handlers](decisions/0011-http-handler-composition.md) | Compose HTTP modules through `buildHandlers` returning `server.RouteRegistrar` values; keep lifecycle in the composition root and avoid a plugin framework. | Accepted |

No record is superseded or deprecated. ADRs 0003, 0004, and 0007 carry amendment
notes because later records refine part of their decision; read the amending ADR
for the current behavior. The ADR index and its status column are maintained in
[`decisions/README.md`](decisions/README.md).
