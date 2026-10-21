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
| Redis cache-aside for `GET /tasks` with generation-token invalidation | Implemented |
| Filtering by `status` and `assignee`, plus deterministic pagination | Implemented |
| CRUD endpoints: `POST /task` and `GET/PUT/DELETE /tasks` | Implemented |
| Uniform JSON error envelope and `404` / `405` handling | Implemented |
| OpenAPI 3.0.3 contract and embedded Swagger UI | Implemented |
| Unit, service, HTTP, PostgreSQL integration, Redis integration, and docs tests | Implemented |
| GitHub Actions CI with PostgreSQL, Redis, coverage gate, and image build | Implemented |
| Multi-stage Docker image and Docker Compose environment | Implemented |
| Authentication, authorization, and multi-tenancy | Not implemented |
| Sorting controls and free-text search | Not implemented (ordering is fixed to `id`; filters are exact matches) |
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
| Cache | [Redis](https://redis.io/) via [go-redis](https://github.com/redis/go-redis) v9.23.0; Compose pins `redis:7.4-alpine` |
| Migrations | Embedded `NNNN_description.sql` files applied by `cmd/migrate` |
| API documentation | OpenAPI 3.0.3 at [`api/openapi.yaml`](api/openapi.yaml); embedded [Swagger UI](https://swagger.io/tools/swagger-ui/) via `swaggest/swgui` |
| Containers | Multi-stage [`Dockerfile`](Dockerfile) (Alpine runtime, non-root) and [`compose.yaml`](compose.yaml) |
| Logging | `log/slog` with a text handler; one structured line per request, including `request_id` |
| Metrics | [Prometheus Go client](https://github.com/prometheus/client_golang) with a dedicated registry, served at `/metrics` |
| Request correlation | `X-Request-ID` propagation or generation; identifier stored in the Gin context |
| Configuration | Standard-library `os.Getenv`, read once at startup |

No ORM, external migration tool, or hosted documentation service is required.
Redis is an optional cache, not a second source of truth: without `REDIS_URL`
the service runs database-only, and a Redis outage falls back to PostgreSQL.

### Scope and limitations

This is a single-aggregate REST service. The task resource has five fields,
`id`, `title`, `description`, `status`, and `assignee`, and there are no
timestamps or ownership enforcement: `assignee` is a free-form label, not an
account reference. The API is unauthenticated and has no sorting controls,
free-text search, or transition rules for status values. Filtering is limited
to exact `status` and `assignee` matches, and tasks cannot be filtered for
being unassigned.

The service is intended to run against PostgreSQL. It has no volatile storage
mode, no automatic database provisioning, and no migration-at-startup behavior.
Redis caches only `GET /tasks`, one entry per filter/cursor/page_size combination; task
writes always commit to PostgreSQL first. Cache entries expire after a finite
TTL, and a mutation advances a generation token so every variant is
invalidated at once and a slow reader cannot republish a pre-mutation page.
The consistency limits are documented in
[ADR 0013](decisions/0013-redis-cache-aside-for-task-list.md); filtering and
cache-key rules begin in
[ADR 0014](decisions/0014-task-filtering-and-pagination.md) and cursor
pagination in [ADR 0016](decisions/0016-cursor-pagination-for-task-list.md).
Production concerns such as authentication, authorization, rate limiting,
metrics storage and dashboards, distributed tracing, and horizontal
coordination are outside the current scope. The metrics endpoint is
pull-based and in-process: the service exposes numbers, it does not store,
aggregate, or alert on them.

## 2. Prerequisites

Choose one of the two setup paths:

- **Docker path:** Docker Engine with Docker Compose v2. Compose supplies
  PostgreSQL and Redis, runs migrations, and starts the API.
- **Local path:** Go and a PostgreSQL server, plus `make` if you want to use the
  Makefile targets. Redis is optional for the process; without it the API
  serves from PostgreSQL and the cache is disabled.

Tool requirements:

| Tool | Requirement | Needed for |
| --- | --- | --- |
| Go | 1.26.2 or a compatible newer toolchain; `go.mod` declares `go 1.26.2` | Building and running locally |
| PostgreSQL | A reachable server; the repository is verified locally against 14.13 and Compose pins 16.4 | Runtime, migrations, integration tests |
| Redis | Optional at runtime; required for the Redis integration suite; Compose pins 7.4 | Task-list cache and cache tests |
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
starts PostgreSQL and Redis, waits for PostgreSQL to become healthy, runs the
one-shot `migrate` service, and only then starts `app`. Redis only has to be
started: the API is fail-open, so an unhealthy Redis never blocks startup.

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
# {"items":[],"next_cursor":null,"has_more":false,"page_size":20}
# on a fresh database; this GET creates one cache entry for that query

docker compose exec redis redis-cli --scan --pattern 'task-manager:tasks:list:*'
# task-manager:tasks:list:generation and one task-manager:tasks:list:<token>:<query> per variant

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

# Optional: start only Redis from Compose and point the process at it.
# Without REDIS_URL the service runs normally with caching disabled.
docker compose up -d redis
export REDIS_URL='redis://localhost:6379/0'
export TEST_REDIS_URL='redis://localhost:6379/15'

make migrate
make app
```

`make app` (equivalent to `make run`) starts the Go process in the foreground so
logs stay visible. Stop it with `Ctrl+C`. To apply migrations, start the API,
and open Swagger UI once the listener is ready, use a single command:

```bash
make swagger-local
```

That target runs `make migrate` first (the same non-destructive migrate
workflow as the standalone target). If migrations fail, startup stops and the
browser is not opened. On success it starts the API, waits until
`GET /health/live`, `GET /swagger/openapi.yaml`, and the Swagger UI all succeed
for this service, then opens <http://localhost:8080/swagger/index.html> (or the
same path on `PORT` if you override it). If another process already owns the
port, bind fails and the browser is not opened—use a free port, for example
`PORT=9090 make swagger-local`. It does not start Docker Compose or reset the
database; PostgreSQL must already be reachable via `DATABASE_URL`, and Redis
remains optional. `make migrate` remains available on its own.

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
running in the foreground. With Redis configured, it also logs that the cache
is enabled:

```text
level=INFO msg="redis cache enabled" list_ttl=1m0s
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
for up to 10 seconds, closes the Redis client if configured, and then closes the
database pool.

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
| `REDIS_URL` | No | *(empty)* | go-redis connection URL, for example `redis://localhost:6379/0`. Empty disables the task-list cache. An invalid URL disables caching but does not stop startup. |
| `REDIS_CACHE_TTL` | No | `60s` | Lifetime of a cached `GET /tasks` page. Every query variant uses it. Must be positive; zero, negative, and unparsable values fall back to `60s`. |
| `LOG_LEVEL` | No | `info` | `debug`, `info`, `warn`, or `error`; any other value falls back to `info`. |
| `METRICS_TASKS_REFRESH_INTERVAL` | No | `15s` | How often `tasks_count` is re-read from PostgreSQL. Zero, negative, and unparsable values fall back to `15s`. |
| `PPROF_ADDR` | No | *(empty)* | Opt-in `net/http/pprof` listener, for example `127.0.0.1:6060`. Empty disables profiling. Non-loopback values abort startup. It is a separate listener, never a route on the API port, and is not wired into `compose.yaml`. |
| `TEST_DATABASE_URL` | No | *(empty)* | Test-only DSN for PostgreSQL integration tests; the service ignores it. |
| `TEST_REDIS_URL` | No | *(empty)* | Test-only Redis URL for integration tests; the service ignores it. Point it at a disposable server. |
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
- `REDIS_URL` is validated with `go-redis`; an invalid URL disables the cache
  with a warning instead of aborting startup. Redis is never required for
  PostgreSQL-backed reads or writes, and cache failures fall back to the
  database.
- `PPROF_ADDR` must name a loopback host and port (`127.0.0.1:6060`,
  `[::1]:6060`, `localhost:6060`). `0.0.0.0:6060` and `:6060` are rejected
  because they would expose profiles on every interface. The listener is
  closed during shutdown, and an in-flight capture is given five seconds to
  finish.

### Docker Compose variables

`compose.yaml` reads these optional variables:

| Variable | Default | Purpose |
| --- | --- | --- |
| `POSTGRES_USER` | `task_manager` | Role created in the Compose PostgreSQL container and used in its DSN. |
| `POSTGRES_PASSWORD` | `task_manager` | Local-development password for that role. Do not reuse it elsewhere. |
| `POSTGRES_DB` | `task_manager` | Database created and migrated in the container. |
| `APP_PORT` | `8080` | Host port published for the API. The container always listens on `8080`. |
| `REDIS_PORT` | `6379` | Loopback host port for the Redis container; used by host integration tests and `redis-cli`. |
| `REDIS_CACHE_TTL` | `60s` | Passed through to the API container as the list-cache TTL. |
| `LOG_LEVEL` | `info` | Passed through to the API container. |

Inside Compose, `DATABASE_URL` is assembled from the PostgreSQL variables and
points at the service name `postgres` on port `5432`. PostgreSQL is not
published on the host; inspect it through the container:

```bash
docker compose exec postgres psql -U task_manager -d task_manager
```

Substitute your `POSTGRES_USER` and `POSTGRES_DB` values if you overrode them.
The API reaches Redis at `redis:6379`. Redis is published only on
`127.0.0.1:${REDIS_PORT:-6379}` so host-side integration tests and `redis-cli`
can reach it; it is not exposed on another interface and has no persistent
volume.

```bash
docker compose exec redis redis-cli ping
# PONG
```

### Using `.env.example`

[`.env.example`](.env.example) is a starting template for the core runtime and
test variables. It is not read by the service. For a local process, copy and
edit it, then export it:

```bash
cp .env.example .env
# Edit DATABASE_URL and TEST_DATABASE_URL for your PostgreSQL instance.
make migrate
make app
```

`make app`, `make run`, `make swagger-local`, and `make migrate` source `.env`
when that file exists (the Go binary still does not read `.env` on its own).
You can also `export` the variables in your shell, or use
`set -a && . ./.env && set +a` before other commands.

`.env` is ignored by Git and must not be committed. The values in
`.env.example` are placeholders; real credentials belong in your shell,
Compose environment, or a secret manager. For a one-off override:

```bash
PORT=9090 make app
# Swagger UI then opens at http://localhost:9090/swagger/index.html:
PORT=9090 make swagger-local
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
| `GET` | `/tasks` | List, filter, and cursor-paginate tasks ordered by `id` ascending | `200` with a cursor envelope, `items: []` when empty | None |
| `GET` | `/tasks/{id}` | Retrieve one task | `200` with the task | None |
| `PUT` | `/tasks/{id}` | Replace the mutable fields of a task | `200` with the replaced task | Required |
| `DELETE` | `/tasks/{id}` | Delete a task | `204` with no body | None |

Task representation:

```json
{
  "id": "6ba7b810-9dad-41d1-80b4-00c04fd430c8",
  "title": "Write the report",
  "description": "Cover the API contract",
  "status": "todo",
  "assignee": "alice"
}
```

Fields and rules:

| Field | Type | Rules |
| --- | --- | --- |
| `id` | string | Server-generated version 4 UUID in canonical form. Clients never send it. |
| `title` | string | Required. Leading and trailing whitespace is trimmed. Empty or whitespace-only titles are rejected. |
| `description` | string | Optional; defaults to `""`. Stored as sent, without trimming. |
| `status` | string | One of `todo`, `in_progress`, `done`. Defaults to `todo` when omitted. |
| `assignee` | string | Optional free-form label, at most 100 characters. Trimmed; empty means unassigned and is returned as `""`. Not an account reference and not authenticated. |

Additional behavior:

- Request bodies are decoded strictly. Unknown fields, trailing content, wrong
  types, empty bodies, and JSON arrays return `400 invalid_request`.
- `PUT` is a full replacement. `title` is required, omitted `description`,
  `status`, and `assignee` reset to `""`, `todo`, and `""`. A missing task is
  never created by `PUT`.
- A malformed `{id}` returns `400 validation_error`; a well-formed UUID that is
  not stored returns `404 not_found`.
- `GET /tasks` returns one cursor page ordered by `id` ascending. The identifier is
  unique, so for a fixed data set pages never overlap; it is
  not creation order.
- No authentication, authorization, or sorting controls are implemented.

#### Create a task

```bash
curl -i -X POST http://localhost:8080/task \
  -H 'Content-Type: application/json' \
  -d '{"title":"Write the report","description":"Cover the API contract","status":"in_progress","assignee":"alice"}'
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
  "status": "in_progress",
  "assignee": "alice"
}
```

#### List, filter, and paginate

`GET /tasks` returns a cursor-paginated envelope (not a bare array, and not
page-number metadata):

```json
{
  "items": [
    {
      "id": "6ba7b810-9dad-41d1-80b4-00c04fd430c8",
      "title": "Write the report",
      "description": "Cover the API contract",
      "status": "done",
      "assignee": "alice"
    }
  ],
  "next_cursor": "eyJ2IjoxLCJhZnRlcl9pZCI6IjZiYTdiODEwLTlkYWQtNDFkMS04MGI0LTAwYzA0ZmQ0MzBjOCIsInN0YXR1cyI6ImRvbmUiLCJhc3NpZ25lZSI6ImFsaWNlIiwicGFnZV9zaXplIjoxfQ",
  "has_more": true,
  "page_size": 1
}
```

| Query parameter | Type | Default | Rules |
| --- | --- | --- | --- |
| `status` | string | *(any)* | Exactly one of `todo`, `in_progress`, `done`. Case-sensitive; any other value is a `400`. |
| `assignee` | string | *(any)* | Exact, case-sensitive label match. Trimmed; 1–100 characters when present. Omitted returns assigned and unassigned tasks. |
| `cursor` | string | *(first page)* | Opaque token from a previous `next_cursor`. Malformed or filter/page-size-incompatible values are `400` (never silently treated as the first page). |
| `page_size` | integer | `20` | 1–100. Values above 100 are rejected, not clamped. |

Metadata semantics:

- `items` holds the page's tasks, ordered by `id` ascending. It is always an
  array, `[]` when empty.
- `has_more` is true when another page exists after this one.
- `next_cursor` is an opaque continuation token when `has_more` is true, and
  `null` when the walk is finished. Pass it as `cursor` on the next request
  with the same filters and `page_size`.
- `page_size` echoes the effective value, including the default.
- The cursor is bound to status, assignee, and page size. Changing those while
  reusing a cursor is a `400`.
- Unknown query parameters (including the removed `page`) are rejected with
  `400`, so a typo cannot silently return an unfiltered page.
- Concurrent writes between pages can change later membership; the cursor
  avoids offset-shift skips/duplicates but is not a multi-request snapshot.
  See [ADR 0016](decisions/0016-cursor-pagination-for-task-list.md).

**Migration from page-number pagination:** stop sending `page` and stop reading
`total` / `total_pages`. Request the first page without `cursor`, then follow
`next_cursor` until `has_more` is false.

```bash
# First page, defaults
curl -i http://localhost:8080/tasks

# Alice's completed tasks, first page of size 2
curl -i 'http://localhost:8080/tasks?status=done&assignee=alice&page_size=2'

# Subsequent page (use next_cursor from the previous response)
curl -i 'http://localhost:8080/tasks?status=done&assignee=alice&page_size=2&cursor=OPAQUE_CURSOR'

curl -i http://localhost:8080/tasks/6ba7b810-9dad-41d1-80b4-00c04fd430c8

curl -i -X PUT http://localhost:8080/tasks/6ba7b810-9dad-41d1-80b4-00c04fd430c8 \
  -H 'Content-Type: application/json' \
  -d '{"title":"Report published","description":"Done","status":"done","assignee":"bob"}'

curl -i -X DELETE http://localhost:8080/tasks/6ba7b810-9dad-41d1-80b4-00c04fd430c8
```

`DELETE` returns `204 No Content`. A later `GET` for the same ID returns `404`,
and the row is removed from PostgreSQL.

An invalid query string uses the same envelope:

```json
{
  "error": {
    "code": "validation_error",
    "message": "invalid query parameters",
    "fields": {
      "page_size": "must be at most 100"
    }
  }
}
```

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
internal/task/service (use cases; declares Repository and ListCache)
    |                         |
    v                         v
service.Repository        service.ListCache
    ^                         ^
    |                         |
internal/task/repository/postgres   internal/task/cache/redis
    (driven adapter, explicit SQL)   (driven cache adapter)
```

`internal/task/domain` holds the aggregate and invariants and has no framework
or driver imports. The docs adapter, `internal/infra/swagger`, depends on the
embedded contract in `api` and on Gin; feature packages do not depend on it.
The observability adapter, `internal/infra/metrics`, depends on the Prometheus
client but not on Gin; the HTTP package calls it through a narrow
`server.Instrumentation` interface and mounts its handler. Only the PostgreSQL
repository knows how to count persisted tasks.

`internal/task/cache/redis` is the cache-aside driven adapter. It implements
the `service.ListCache` port, owns the Redis key layout and generation token,
and handles Redis failures; the service and domain packages never import
Redis. The cache stores `GET /tasks` pages, one entry per normalized query
(status, assignee, cursor, page size) under the current generation. PostgreSQL
remains authoritative, and a cache read, write, or invalidation failure falls
back to the database.

### Package responsibilities

| Package | Responsibility |
| --- | --- |
| `cmd/task-manager` | Process entry point; constructs the app, calls `Initialize` and `Start`, and handles returned errors. |
| `cmd/migrate` | Applies embedded migrations and exits non-zero on failure. |
| `internal/app` | Composition root: loads config, builds the logger and pool, verifies the schema, constructs the optional Redis cache, composes HTTP modules through `buildHandlers`, builds the server, and shuts down in reverse order. |
| `internal/infra/config` | Reads environment variables (including Redis URL and cache TTL) into plain structs and applies defaults. No network or filesystem I/O. |
| `internal/infra/database` | Validates the DSN, opens the pooled `*sql.DB`, embeds and applies migrations, and verifies required tables. |
| `internal/infra/server` | Builds the Gin router, request-ID and completion middleware, health and metrics endpoints, feature modules, and `404`/`405` handlers; owns HTTP lifecycle. Declares the narrow `HealthChecker`, `RouteRegistrar`, and `Instrumentation` interfaces. |
| `internal/infra/handler` | Shared HTTP delivery code: the JSON error envelope and error writer. |
| `internal/infra/metrics` | Prometheus collectors in a dedicated registry, the `tasks_count` refresher, and the exposition handler. No Gin import; the registry is private. |
| `internal/infra/swagger` | Docs adapter: serves the embedded spec and Swagger UI under `/swagger`; registers only `GET`. |
| `api` | Embeds `api/openapi.yaml` as `api.OpenAPI`; no business or transport logic. |
| `internal/task/domain` | `Task`, the closed `Status` set, validation, and UUID identifier format and generation. |
| `internal/task/service` | Create, list (filtered and cursor-paginated), get, update, and delete use cases; normalizes list queries and cursors, declares the `Repository` and `ListCache` interfaces it consumes, and invalidates every cached page after a committed mutation. |
| `internal/task/cache/redis` | Cache-aside adapter for task-list pages: one entry per normalized query (including cursor) under the generation token, finite TTL, versioned JSON payload with cursor metadata, malformed-data handling, and fail-open cache failures. |
| `internal/task/handler` | Strict request and query decoding, query/path validation, the paginated envelope, response and error mapping; declares the `TaskService` interface it consumes. |
| `internal/task/repository/postgres` | Explicit SQL CRUD, identifier generation, `sql.ErrNoRows` to `service.ErrNotFound`, parameterized filtering with a matching count, deterministic `id` order, and the `tasks` row count used by metrics. |

### Wiring and startup

`cmd/task-manager/main.go` only constructs `app.New()` and calls `Initialize`,
then `Start`. The composition root performs the actual wiring:

1. `config.Load()` reads the environment and applies defaults.
2. A `slog` text logger writes to stdout at the configured level.
3. `DATABASE_URL` is required. An empty value aborts startup.
4. `database.New` validates the DSN and opens the pool without dialing.
5. `Initialize` pings with a five-second budget and calls
   `database.VerifySchema`, which requires the `tasks` table and the columns the
   binary reads (`assignee`). A failure closes the pool and stops startup.
6. If `REDIS_URL` is set, the Redis cache adapter is constructed. It does not
   dial at construction. One best-effort generation reset runs at startup; an
   invalid URL or an unavailable server is logged, and the service continues
   with caching disabled or reads bypassed. Redis never stops startup.
7. `metrics.New` builds a dedicated Prometheus registry, and a
   `TaskCountRefresher` is wired to its own task-repository adapter over the
   shared pool with the configured refresh interval.
8. `buildHandlers` (in `internal/app/wiring.go`) constructs the task
   repository from the App pool, wires the task service (repository + optional
   list cache + logger) into the task HTTP handler, and returns that handler
   plus the docs adapter as `server.RouteRegistrar` values. Feature repository
   construction for HTTP modules lives here so `server.New` never enumerates
   repositories ([ADR 0017](decisions/0017-encapsulate-handler-repository-wiring.md)).
9. `server.New` receives timeouts, the pool as the health checker, the metrics
   adapter as `Instrumentation`, and the registrars from `buildHandlers()`. It
   builds the router with `gin.Recovery()`, request-ID and completion
   middleware, health routes, `/metrics`, `/swagger/`, and feature routes.
10. `Start` installs a signal context for `SIGINT`/`SIGTERM`, starts the
    task-count refresher, runs the server, shuts it down with a ten-second
    budget, stops and waits for the refresher, closes the Redis client if one
    is configured, and then closes the PostgreSQL pool.

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
11. Cache invalidation is part of a mutation's contract: after a committed
    task-list mutation, invalidate through the service. Do not delete keys in a
    handler or bypass the use case. A new list-shaped read needs its own key
    and invalidation decision; never reuse another query's variant key.

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
    status      TEXT NOT NULL CHECK (status IN ('todo', 'in_progress', 'done')),
    assignee    TEXT CHECK (assignee IS NULL
                            OR (btrim(assignee) <> '' AND char_length(assignee) <= 100))
);

CREATE INDEX IF NOT EXISTS tasks_status_idx ON tasks (status);
CREATE INDEX IF NOT EXISTS tasks_assignee_idx ON tasks (assignee);
```

The table contains exactly the API fields. The `CHECK` constraints duplicate
the domain rules so a manual statement or a future write path cannot store a
blank title, an unknown status, or a blank/over-long assignee. `assignee` is
NULL when unassigned, which is the single stored representation; the API
always renders it as `""`. `GET /tasks` uses `id` ascending for its stable
order, not creation order, and filters on the two indexed columns before
applying `LIMIT`/`OFFSET`.

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
| `0003_add_task_assignee.sql` | Adds the nullable `assignee` column and its check constraint. |
| `0004_index_tasks_assignee.sql` | Creates `tasks_assignee_idx` on `assignee`. |

`0003` and `0004` must be applied before running a binary that filters by
assignee: startup verifies that the column exists and fails with
`schema is out of date: column "assignee" on table "tasks" is missing`
otherwise. Applying them is additive and backward compatible: an older binary
ignores the column, and existing rows read as unassigned (`""`).

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
then verifies the required table and columns with `database.VerifySchema`. A
reachable server with a missing table or a missing `assignee` column is not
considered ready. There is no in-memory fallback and no automatic database
provisioning.

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
docker compose up -d redis
export TEST_DATABASE_URL='postgres:///task_manager_test?host=localhost&sslmode=disable'
export TEST_REDIS_URL='redis://localhost:6379/15'
make test-integration
```

The Make target requires both URLs; the Redis setup is described in
[Integration-test Redis](#integration-test-redis) below. Database-backed
packages must run sequentially because they share one database; the Makefile
and CI pass `-p 1`. Never point `TEST_DATABASE_URL` at a database whose data
you need to keep.

### Integration-test Redis

The cache adapter tests use `TEST_REDIS_URL` and skip with an explicit message
when it is unset. Each test uses a unique key prefix and cleans up after itself,
but the URL should still point at a disposable Redis instance:

```bash
docker compose up -d redis
export TEST_REDIS_URL='redis://localhost:6379/15'
make test-integration \
  TEST_DATABASE_URL='postgres:///task_manager_test?host=localhost&sslmode=disable' \
  TEST_REDIS_URL='redis://localhost:6379/15'
```

The suite covers miss-then-hit, empty-list caching, expiration, generation
invalidation, malformed payloads, concurrent first reads, and the stale-refill
ordering. `make test-integration` refuses to run without both URLs; CI asserts
that representative Redis tests ran rather than silently skipping.

Never point `TEST_REDIS_URL` at a shared Redis database whose data you need.

## 8. Testing and Quality Checks

The CI and coverage decisions are recorded in
[ADR 0007](decisions/0007-ci-with-postgresql-service.md),
[ADR 0008](decisions/0008-containers-and-coverage-gate.md), and
[ADR 0013](decisions/0013-redis-cache-aside-for-task-list.md). The suite has
three tiers:

- **Infrastructure-free tests** cover the domain, service use cases and cache
  decisions, HTTP handler, router, request correlation, metrics, docs adapter,
  cache generation logic with a fake store, configuration, and startup error
  paths. They run without PostgreSQL or Redis.
- **PostgreSQL integration tests** cover the repository, migrations, and full
  app wiring. They run only when `TEST_DATABASE_URL` is set; otherwise they use
  `t.Skip` with an explicit message.
- **Redis integration tests** cover cache commands against a real server:
  miss-then-hit, empty lists, TTL expiry, mutation invalidation, malformed
  payloads, concurrent generation initialization, and the stale-refill race.
  They run only when `TEST_REDIS_URL` is set.

The configuration tests clear the variables they assert on, so a preset `PORT`,
`DATABASE_URL`, `REDIS_URL`, or logging variable cannot change their result.

| Task | Command |
| --- | --- |
| Infrastructure-free tests | `make test` or `go test ./...` |
| Whole suite including PostgreSQL and Redis | `make test-integration TEST_DATABASE_URL='postgres:///task_manager_test?host=localhost&sslmode=disable' TEST_REDIS_URL='redis://localhost:6379/15'` |
| One package | `go test ./internal/task/handler` |
| One cache package | `TEST_REDIS_URL=... go test ./internal/task/cache/redis` |
| Verbose output | `go test -v ./...` |
| Race detector against PostgreSQL and Redis | `TEST_DATABASE_URL=... TEST_REDIS_URL=... go test -race -count=1 -p 1 ./...` |
| Coverage summary | `TEST_DATABASE_URL=... TEST_REDIS_URL=... go test -cover ./...` |
| Coverage profile | `TEST_DATABASE_URL=... TEST_REDIS_URL=... go test -coverprofile=coverage.out ./...` |
| Browse coverage report | `go tool cover -html=coverage.out` |
| Coverage gate (≥70%, requires both) | `make coverage-check TEST_DATABASE_URL=... TEST_REDIS_URL=...` |
| Vet | `make vet` or `go vet ./...` |
| Formatting check | `make fmt` or `gofmt -l .` |
| Apply formatting | `gofmt -w .` |
| Build the binary | `make build` or `go build -o task-manager ./cmd/task-manager` |
| Compile every package | `go build ./...` |
| Run the API locally (foreground) | `make app` or `make run` |
| Migrate, run the API, and open Swagger UI | `make swagger-local` → runs `make migrate`, then opens <http://localhost:8080/swagger/index.html> |
| Apply migrations only | `make migrate` or `go run ./cmd/migrate` |
| Validate the OpenAPI document | `npx --yes @redocly/cli@2.66.0 lint api/openapi.yaml` |
| Load-test `GET /tasks` | `go run ./cmd/loadtest -base-url http://127.0.0.1:8080 -seed 2000` |
| Capture and inspect a CPU profile | See [Performance testing and profiling](#performance-testing-and-profiling) |

Important details:

- `make fmt` and `gofmt -l .` only list files whose formatting differs; they do
  not rewrite files or fail. Use `gofmt -w .` to fix them. CI fails when any file
  is listed.
- `coverage.out` matches `*.out` in `.gitignore`.
- Measured statement coverage on this tree is **77.9%** with PostgreSQL and
  Redis, and lower without them. The PostgreSQL and Redis adapters only execute
  in their integration tiers.
- `make coverage-check` enforces `COVERAGE_MIN` (default `70`) on the combined
  suite and refuses to run without both `TEST_DATABASE_URL` and
  `TEST_REDIS_URL`. `make coverage` writes a profile and prints the total but
  does not enforce the threshold.
- Service, handler, and cache-decision tests use hand-written fakes, not a
  mocking library. The PostgreSQL and Redis adapters are verified against real
  servers, not with SQL or Redis mocks.
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
- `internal/task/domain` tests cover `ParseStatusFilter` directly (accepted
  statuses, empty-as-any, and invalid values), separately from `ParseStatus`.
- `internal/task/service` tests prove cache hits skip the repository, empty
  pages are hits, misses populate the cache, read/write failures fall back, and
  only successful create/update/delete operations invalidate. They also prove
  invalidation survives a canceled request context, that filters are applied
  before keyset pagination, that cursors encode/decode and reject incompatible
  queries, and that the cache receives the normalized query (including cursor).
- `internal/task/handler` tests cover every query-parameter failure (unknown
  status, empty or oversized assignee, malformed or incompatible cursor, page
  size above the maximum, obsolete `page`, unknown parameters), both filters
  alone and combined, deterministic non-overlapping cursor pages, end-of-list
  via `has_more`/`next_cursor`, default and custom page sizes, and the cursor
  envelope metadata.
- `internal/task/cache/redis` unit tests cover generation initialization, key
  layout and TTL, empty and malformed payloads, wrong-version payloads,
  compare-and-delete recovery that does not remove a concurrent refill,
  degraded reads after a failed invalidation, the rate-limited recovery probe
  (success and continued failure), and the stale-refill ordering. They also
  prove that every distinct query (including distinct cursors) produces a
  distinct variant key, that equivalent queries share one key, and that two
  variants cannot answer each other. Integration tests repeat the command-level
  behavior against a real Redis server, including expiration, variant
  isolation, whole-family invalidation, and concurrent first reads.
- `cmd/loadtest` tests cover nearest-rank percentiles, failure accounting,
  request timeouts, exit-status mapping (including usage vs evidence
  categories), option validation (including invalid base URLs), warmup abort
  behavior, miss-mode generation replacement, dataset seeding/cleanup
  bookkeeping, and base-URL handling. The live scenario and the numbers in this
  README are produced manually against a running service.
- The PostgreSQL suite covers filtering by status and assignee (alone and
  combined), deterministic non-overlapping keyset pages, insert/delete between
  pages (including the offset-shift contrast), filter-leaving updates, SQL
  parameter binding for filter values, and the assignee column constraints.
- `internal/app` tests cover a missing `DATABASE_URL`, a malformed DSN, an
  unreachable server, a missing schema, and successful PostgreSQL wiring with
  caching disabled when `REDIS_URL` is empty.
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

### Performance testing and profiling

`cmd/loadtest` is a small Go load generator for `GET /tasks`. It exists to answer
one question reproducibly: how does the task-list endpoint behave under a fixed
request count and concurrency, and what changes when the Redis cache is bypassed?
It is a measurement tool, not a service dependency, and it makes no attempt to
be a general benchmarking framework. The decision is recorded in
[ADR 0015](decisions/0015-load-testing-and-profiling.md).

**Prerequisites**

- A running service with a migrated database, reached over HTTP.
- Go toolchain on the host (the tool is built from this repository).
- Optional: Redis for the warm/miss comparison and `curl` plus Go tooling for
  profiles.
- A disposable local database. The tool creates and deletes its own tasks; do
  not point `-seed` at shared or production data.

**Dataset preparation**

`-seed 2000` first counts tasks by walking `GET /tasks` cursors, creates
2000 tasks through `POST /task` with a run-unique title
(`loadtest-<unix-nano>-<n>`, assignee `loadtest`), then re-counts via cursors and
aborts if the count is not `baseline + 2000`. After the measured phase it
deletes exactly the IDs it created (a `404` is accepted) and reports how many
were removed. Tasks that already exist are never touched, and a second run does
not accumulate rows. If the process is killed mid-run, the leftover rows are
identifiable by the `loadtest-` title prefix and the `loadtest` assignee; the
next run still measures its own 2000 new tasks on top of them, and `total` in
the output makes that visible. `-keep` skips the deletion when you want to
inspect the rows.

Mutations are refused against a non-loopback host unless
`-allow-remote-mutations` is passed.

**Running the scenario**

```bash
# Warm: one query variant, cache primed by a short warmup, then measured.
# Use Redis logical database /1 for benchmarks (distinct from the service
# default /0 and from integration tests on /15) so keyspace counters are quiet.
go run ./cmd/loadtest \
  -base-url http://127.0.0.1:8080 \
  -query 'page_size=20' \
  -requests 50000 -concurrency 16 -timeout 5s \
  -seed 2000 \
  -redis-url redis://localhost:6379/1

# Cache-miss: every request must reach PostgreSQL.
go run ./cmd/loadtest \
  -base-url http://127.0.0.1:8080 \
  -query 'page_size=20' \
  -mode miss -concurrency 1 -requests 1000 -timeout 5s \
  -seed 2000 \
  -redis-url redis://localhost:6379/1
```

Flags: `-base-url`, `-query`, `-requests`, `-concurrency`, `-timeout`,
`-warmup`, `-mode warm|miss`, `-redis-url`, `-redis-prefix`, `-seed`, `-keep`,
`-allow-remote-mutations`. `-requests` is capped at one million and
`-concurrency` at 4096, so the latency and success bookkeeping (about 9 bytes
per request) and the goroutine count stay bounded. Each worker is a fixed goroutine that performs
requests sequentially; no request spawns another goroutine.

The report contains total, successful, and failed requests, wall-clock duration
from `time.Since` around the measured phase, throughput, mean, nearest-rank p50,
p95, p99, and maximum latency. Latency samples are per-request observed
durations of successful requests only; failures are counted and excluded so a
fast rejection cannot flatter the percentiles. The first three failure messages
(status code or transport error) are printed; response bodies are never read
into the report. The process exits `0` only when every request succeeded; `1`
when measured requests failed, warmup failed, cleanup did not finish, or a
required Redis evidence check was missing or contradicted the mode; and `2` for
unusable options (including a base URL that contains a path).

**Measured results**

Measured on 2026-10-21 against a local build of this tree. Dataset: 2000 tasks
created by the tool, first page, `page_size=20`, no filter. Redis list cache
enabled (`REDIS_CACHE_TTL=60s`).

| Scenario | Requests | Concurrency | Throughput | Mean | p50 | p95 | p99 | Max | Failed |
| --- | --- | --- | --- | --- | --- | --- | --- | --- | --- |
| Warm cache | 200000 | 16 | 13239 req/s | 1.21 ms | 1.17 ms | 1.61 ms | 2.10 ms | 7.13 ms | 0 |
| Warm cache | 50000 | 16 | 12937 req/s | 1.23 ms | 1.18 ms | 1.69 ms | 2.39 ms | 6.10 ms | 0 |
| Warm cache | 1000 | 1 | 2470 req/s | 0.40 ms | 0.40 ms | 0.48 ms | 0.54 ms | 1.14 ms | 0 |
| Cache miss | 1000 | 1 | 933 req/s | 0.90 ms | 0.84 ms | 1.27 ms | 1.34 ms | 3.76 ms | 0 |

The single-threaded pair is the only directly comparable cache comparison: both
send the same 1000 requests to the same 2000-row dataset, one from Redis and one
from PostgreSQL. Warm was 2.65× the throughput and had a p50 0.44 ms lower
(400 µs versus 840 µs) in this environment. The 16-way runs reach 13.2k req/s
with a p99 of 2.1–2.4 ms, but they are not compared against a concurrent miss
scenario, because one request repopulating the cache would turn later requests
into hits and the run could not be labelled a miss.

**How the cache path was verified**

When `-redis-url` is supplied the tool reads Redis `keyspace_hits` and
`keyspace_misses` before and after the measured phase, and counts list keys
(`<prefix>:tasks:list:*`) before and after:

- Warm run of 200000: `hits +400000, misses +0`, list keys unchanged. Two hits
  per request is the generation read plus the list read, so every measured
  request was served from Redis.
- Warm run of 1000 at concurrency 1: `hits +2000, misses +0`.
- Miss run of 1000: `hits +1000, misses +1000`, list keys `10 -> 1010`. Each
  request replaced the generation token with 128 fresh random bits before its
  timed GET, so the following lookup addressed a generation that had never been
  cached; the single generation read is a hit, the variant read is a miss, and
  exactly one new list key is written per request. `-mode miss` requires
  `-concurrency 1` for that reason: with parallel requests the first miss would
  repopulate the cache and later requests would be hits.

Redis counters are server-wide, so the numbers would include any other client
of that Redis instance. The measurements above used Redis logical database
`/1` (the same database the commands above pass via `-redis-url`), not the
service's habitual `/0` and not integration-test database `/15`, and no other
client traffic was generated during the measured phases.

**Profiling**

`PPROF_ADDR` enables `net/http/pprof` on its own loopback listener. It is empty
by default, rejected for non-loopback addresses, never registered on the Gin
router, and not set in `compose.yaml`. Keep it unset for anything but local
diagnostics.

```bash
# Terminal 1: build once so profiles resolve against the exact binary, then
# run it with profiling enabled against a disposable database.
make build
export DATABASE_URL='postgres:///task_manager_bench?host=localhost&sslmode=disable'
export REDIS_URL='redis://localhost:6379/1'
export PPROF_ADDR='127.0.0.1:6060'
PORT=8080 ./task-manager

# Terminal 2: start the capture, then generate load for at least as long.
curl -sS -o cpu-warm.pprof 'http://127.0.0.1:6060/debug/pprof/profile?seconds=20' &
go run ./cmd/loadtest -base-url http://127.0.0.1:8080 -requests 200000 -concurrency 16 \
  -seed 2000 -redis-url redis://localhost:6379/1
wait

# Inspect the captured profile with the binary under test.
go tool pprof -top -nodecount=20 ./task-manager cpu-warm.pprof
go tool pprof -top -cum -focus='task-manager/internal' ./task-manager cpu-warm.pprof
go tool pprof -http=127.0.0.1:8084 ./task-manager cpu-warm.pprof
```

`*.pprof` is ignored by Git so profiles and binaries are not committed.

**What the captured profile showed**

The capture above ran for 20.07 s and recorded 28.03 s of samples (139.7% of
wall time, about 1.4 cores) during the 200000-request warm run. The window also
contains about 0.75 s of dataset creation and the start of cleanup, a small
fraction of the samples. Top entries by flat time:

| Frame | Flat | Meaning |
| --- | --- | --- |
| `syscall.rawsyscalln` | 46.8% | Socket reads and writes for HTTP and Redis, i.e. I/O time |
| `runtime.pthread_cond_wait` | 14.3% | Blocked goroutines in the runtime and netpoller |
| `runtime.kevent` | 10.8% | kqueue wait on the loopback sockets |
| `runtime.usleep` | 6.8% | Runtime scheduling waits |
| `runtime.pthread_cond_signal` | 5.4% | Waking goroutines for completed I/O |

Cumulative application time: `Handler.list` 29.2%, `cache.Get` / `Service.List`
27.0%, `go-redis` `Client.Process` 21.1%, and `encoding/json` (request parse plus
response encoding) 7.1%. Garbage collection is small (`gcDrain` about 2.5% cum).
In other words, a warm list request is dominated by socket I/O and Redis client
work, not by JSON, allocation, or routing; there is no single hot function to
optimize in the handler. The profile measures this service and this client on
the same machine, so client CPU and server CPU share the host.

A heap profile is a separate measurement and was not needed here. If you want
one, capture it while the load runs and inspect it the same way:

```bash
curl -sS -o heap.pprof 'http://127.0.0.1:6060/debug/pprof/heap'
go tool pprof -top ./task-manager heap.pprof
```

**Environment and limits**

- macOS 26.3, arm64, 10 logical CPUs, 16 GB RAM; Go 1.26.2 (`darwin/arm64`).
- PostgreSQL 14.13 (Homebrew) on loopback with `shared_buffers=128MB`,
  `max_connections=100`, and `fsync=on`; the service used its default pool
  (10 open, 5 idle).
- Redis 7.4.11 in the Compose container (Linux VM, published on
  `127.0.0.1:6379`, no `maxmemory`); the benchmark used logical database `/1`
  for both `REDIS_URL` and `-redis-url` (not `/0`, not test DB `/15`).
- Service run as a local process (not the container image) with default
  `LOG_LEVEL=info`, so per-request logging was active, on the same host as the
  load generator. Client and server therefore compete for the same cores.
- Redis traffic crosses the Docker VM boundary on macOS, which inflates warm
  latency relative to a native Redis.
- Results depend on dataset size, hardware, PostgreSQL configuration, log level,
  page_size/filters, and whether the client is co-located. Re-run the commands on
  your machine instead of treating these numbers as a general benchmark.

### Continuous integration

[`.github/workflows/ci.yml`](.github/workflows/ci.yml) runs on pushes to `main`,
every pull request, and manual dispatch. It provisions a real PostgreSQL service
(`postgres:16.4-alpine`) with an ephemeral `task_manager_test` database and a
throwaway Redis service (`redis:7.4-alpine`), then runs:

1. A formatting check that fails on any file `gofmt` would change.
2. `go vet ./...` and `go build ./...`.
3. Infrastructure-free tests with both integration URLs cleared.
4. `go run ./cmd/migrate` with `DATABASE_URL`.
5. `make test-integration` with `TEST_DATABASE_URL` and `TEST_REDIS_URL`.
6. Assertions that representative PostgreSQL and Redis tests report `--- PASS`
   and that no `--- SKIP` occurred.
7. `go test -race -count=1 -p 1 ./...` against both services.
8. `make coverage-check`, enforcing the 70% combined-suite threshold.
9. A Docker image build, a non-root UID assertion, and
   `docker compose config --quiet`.

The workflow has read-only repository permissions, uses throwaway service
credentials, and performs no deployment, publishing, or push.

## 9. Docker Compose

`compose.yaml` defines four services under the project name `task-manager`.
The container and Compose decision is recorded in
[ADR 0008](decisions/0008-containers-and-coverage-gate.md); the cache design is
in [ADR 0013](decisions/0013-redis-cache-aside-for-task-list.md).

| Service | Image / command | Responsibility |
| --- | --- | --- |
| `postgres` | `postgres:16.4-alpine` | Stores the `tasks` table and `schema_migrations` in the named `pgdata` volume; health-checked with `pg_isready`. |
| `redis` | `redis:7.4-alpine`, persistence disabled | Caches `GET /tasks` only. Health-checked with `redis-cli ping`; no volume, because PostgreSQL is authoritative. |
| `migrate` | Same app image, entrypoint `/app/migrate` | Waits for PostgreSQL, applies pending migrations, and exits. It runs once, not as a long-lived service. |
| `app` | Same app image, entrypoint `/app/task-manager` | Serves the API after PostgreSQL is healthy and migrations completed. It waits for Redis only to be started, not to be healthy, because it can serve every request from PostgreSQL. |

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
variables. Redis is reached at `redis:6379` through `REDIS_URL`. This avoids
collisions with a local PostgreSQL on port 5432.

Redis publishes only a loopback port, `127.0.0.1:${REDIS_PORT:-6379}`, for
host-side integration tests and `redis-cli`; it is not reachable on another
interface. It uses no volume and disables RDB/AOF persistence, so a Redis
restart or removal only causes cache misses.

Metrics use the same application port: `curl -s http://localhost:8080/metrics`
(or `:${APP_PORT}` when overridden). No additional API port is published, so
PostgreSQL and Redis stay unreachable from another host.

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
Redis intentionally has no volume: losing the cache is always recoverable from
PostgreSQL and simply causes misses. `docker compose down -v` is the explicit
destructive command and removes the PostgreSQL volume and all tasks. To build
the image without Compose:

```bash
docker build -t task-manager:local .
```

## 10. Design Trade-offs and Limitations

| Decision | Benefit | Cost / limitation |
| --- | --- | --- |
| PostgreSQL is the only task store, with no fallback | Data is durable and every process shares one source of truth | The service cannot run or serve a request without PostgreSQL; local setup needs a database |
| Redis cache-aside for `GET /tasks`, invalidated by a generation token | Repeated list reads skip PostgreSQL; a slow reader cannot republish a pre-mutation list | One extra Redis round trip on successful writes; a failed invalidation disables cache reads until a recovery probe succeeds (about five seconds later) or a mutation invalidates again; cached data is eventually consistent within the TTL |
| PostgreSQL via `database/sql` and pgx, without an ORM | Explicit SQL, standard-library interfaces, and no ORM mapping layer | SQL is written and maintained by hand; repository behavior is only proven with integration tests |
| UUID identifiers instead of integers | IDs are opaque, unguessable, and generated without coordination | IDs are larger, and list order by UUID is not creation order |
| Strict JSON decoding and full-replacement `PUT` | Typos and contract drift fail loudly; replacement semantics are unambiguous | Clients must send every replacement field, and extra metadata is rejected |
| Fail-closed startup: ping and schema verification | The process never starts half-wired or serves requests that can only return `500` | Startup depends on database availability; there is no retry loop, so an orchestrator must restart it |
| Migrations are explicit and not run by the app | Schema changes are reviewed and controlled independently of deploys | Deployments need a migration step; Compose models this as a one-shot service |
| Hand-written OpenAPI contract with embedded Swagger UI | Clients get a machine-readable contract and offline docs; no CDN or asset directory | The spec can drift from handlers; updates require discipline and linting |
| Pull-based Prometheus metrics in-process | Standard format and no agent or metrics backend is required; any scraper can consume it | The service does not store, aggregate, or alert on the data; `/metrics` is unauthenticated like the rest of the API |
| `tasks_count` refreshed on an interval instead of per request | No table scan on the request path; the value reflects PostgreSQL rather than process memory | Eventually consistent up to one interval; stale but timestamped when PostgreSQL is unavailable, and absent before the first success |
| Request correlation via `X-Request-ID` instead of OpenTelemetry | Cheap, dependency-free log correlation; every response carries the ID | It is not distributed tracing: no spans, sampling, propagation into PostgreSQL, or trace backend |
| 70% coverage gate on the combined suite | The gate measures the PostgreSQL and Redis adapters as well as unit tests | It requires live test services and a longer CI job |
| Cursor-paginated collection envelope | One response carries items plus `next_cursor`/`has_more` without offset-shift skips | Breaking change from page/total metadata; clients must follow `next_cursor` |
| Exact-match filters with a fixed `id` ordering, no sort parameter | Bounded query shapes, simple parameterized SQL, and an indexable plan | Clients cannot sort or search; ordering is by UUID, not creation time |
| One cached entry per normalized query under a shared generation | Filtered and cursor-paginated reads are cached without collisions, and one mutation invalidates them all | Each distinct filter/cursor/page_size combination occupies its own key until the TTL expires |
| Keyset page fetch with `page_size+1` (no `COUNT(*)`) | End-of-list is cheap and exact for the page; no offset math | Clients that need a global total must count locally or use another endpoint later |
| Small, single-aggregate service | Easy to understand, test, and deploy | No authentication, sorting, search, timestamps, or multi-tenancy |

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
- The task list cache is not a transactional view. A `GET /tasks` that began
  before a mutation may return the pre-mutation list; requests that begin after
  the mutation use the new generation. If invalidation fails, the process serves
  PostgreSQL directly and logs the failure rather than continuing to serve the
  old generation.

## 11. Troubleshooting

| Symptom | Likely cause | Action |
| --- | --- | --- |
| `initialize: app: initialize database: DATABASE_URL is required` | `DATABASE_URL` is unset or empty | Export a valid DSN before `make run`; inside Compose, check the `app` service environment |
| `ping: ... connection refused` or `database unreachable` | PostgreSQL is not running, the host/port is wrong, or Compose is using the wrong host | Run `pg_isready`, check `docker compose logs postgres`, and remember that the Compose host is `postgres`, not `localhost` |
| `role "..." does not exist` or authentication failure | The DSN role/database does not exist or the password is wrong | Create the role/database or correct `DATABASE_URL`; do not commit credentials |
| `schema is not initialized: table "tasks" is missing` | Migrations were never applied, or were applied to another database | Run `make migrate` locally or `docker compose run --rm migrate` |
| `schema is out of date: column "assignee" ... is missing` | The binary expects migration `0003`, but the database has only older versions | Run `make migrate` (or `docker compose run --rm migrate`) before starting the new binary |
| Integration tests skip with `TEST_DATABASE_URL is not set` | The PostgreSQL integration tier was not enabled | Export `TEST_DATABASE_URL`; use `make test-integration` to fail instead of skipping |
| Integration tests skip with `TEST_REDIS_URL is not set` | The Redis integration tier was not enabled | Start Redis (`docker compose up -d redis`) and export `TEST_REDIS_URL` |
| `make coverage-check` refuses to run | `TEST_DATABASE_URL` or `TEST_REDIS_URL` is unset | Provide both test URLs; the gate intentionally requires both adapter tiers |
| `bind: address already in use` on `:8080` | Another process uses the port | Run `PORT=9090 make run`, or publish `APP_PORT=9090` with Compose |
| Startup fails with `PPROF_ADDR "0.0.0.0:6060" must bind a loopback address` | Profiling was enabled on a non-loopback address | Use `PPROF_ADDR=127.0.0.1:6060`; the listener is deliberately loopback-only and separate from the API port |
| `docker compose` cannot connect the app to PostgreSQL | The `migrate` service failed or PostgreSQL is unhealthy | Check `docker compose ps`, `docker compose logs postgres`, and `docker compose logs migrate`; the app waits for both |
| Tasks disappear after `docker compose down` | The volume was removed with `-v` | Use `docker compose down` without `-v`; `down -v` is intentionally destructive |
| Requests return `500` after PostgreSQL restarts | The process is still running but the database is unavailable | Check database health and logs; restart the service once PostgreSQL is ready |
| Swagger UI returns `404` | The path is not under `/swagger/`, or the binary is old | Use `/swagger/` or `/swagger/index.html`; rebuild the binary if the docs route is absent |
| `/metrics` returns `404` | The binary predates the observability phase, or a test server was built without instrumentation | Rebuild/restart the service; `internal/app` always wires the endpoint |
| `tasks_count` is missing from `/metrics` | No refresh has succeeded yet (database down, schema missing, or first refresh still running) | Check `/health`, the application log for `task count refresh failed`, and the refresh interval |
| `tasks_count` looks stale | PostgreSQL became unavailable, or the refresh interval is large | Compare `time()` with `tasks_count_last_refresh_timestamp_seconds`; check database connectivity and `METRICS_TASKS_REFRESH_INTERVAL` |
| `redis cache disabled` or `redis cache unavailable at startup` in the log | `REDIS_URL` is malformed or Redis is unreachable | The API still serves from PostgreSQL. Correct the URL or start Redis; a restart re-attempts the generation reset |
| `task list cache failure` warnings on every GET | Redis is down, or invalidation failed earlier and the adapter is degraded | Check Redis health. The process bypasses the cache and serves PostgreSQL. A recovery probe retries every five seconds and a successful mutation invalidates immediately, so caching resumes once Redis answers |
| `GET /tasks` keeps returning an old list after a write | The mutation failed (so it was not committed), or invalidation failed and was logged | Check the write response and the `task list cache invalidation failed` log. The cache is bypassed after that error; investigate Redis before restarting |
| `GET /tasks` returns `400` with `invalid query parameters` | An unknown parameter (`staus` or obsolete `page`), an empty `assignee`, an unsupported `status`, a malformed/incompatible `cursor`, or `page_size` outside 1–100 | Send only `status`, `assignee`, `cursor`, and `page_size` with the documented ranges; the `fields` object names the offending parameter |
| `has_more` is true and `items` has fewer than `page_size` rows | Not expected for a healthy response; `has_more` means another page exists after a full page | Follow `next_cursor` with the same filters and `page_size` until `has_more` is false |
| Filtering returns nothing for a task that exists | The filter is an exact, case-sensitive match, and the task may be unassigned | Check the stored `assignee` on the task; `assignee` filters cannot select unassigned tasks |
| Compose starts `app` but the log shows `redis cache unavailable at startup` | Redis was started but not ready, or is down; `app` waits only for the container to start | Expected. The API serves PostgreSQL and the adapter retries every five seconds, so caching resumes without a restart |
| Every response has an unexpected `X-Request-ID` | A proxy supplied an invalid or oversized header, so the service replaced it | Expected; the response header always contains the final identifier, which also appears in the request log |
| Redocly reports a missing `license` warning | The repository does not declare a license | This is advisory; validation still succeeds. Do not add a false license just to silence it |

## 12. Project Structure

```text
.
├── api/
│   ├── openapi.go                  embeds the contract as api.OpenAPI
│   └── openapi.yaml                canonical OpenAPI 3.0.3 contract
├── cmd/
│   ├── loadtest/                   HTTP load generator for GET /tasks
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
│       ├── cache/redis/            Redis list-cache adapter and generation token
│       ├── domain/                 Task aggregate, statuses, validation, UUIDs
│       ├── handler/                HTTP driving adapter
│       ├── repository/postgres/    explicit-SQL driven adapter
│       └── service/                use cases, repository and list-cache ports
├── .github/workflows/ci.yml        CI with PostgreSQL, Redis, tests, coverage, Docker
├── .env.example                    core environment template
├── .dockerignore                   build-context exclusions
├── Dockerfile                      multi-stage, non-root image
├── Makefile                        build, run, migrate, test, coverage, vet, fmt
├── compose.yaml                    PostgreSQL, Redis, one-shot migration, API
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
| [0011. One composition point for HTTP module handlers](decisions/0011-http-handler-composition.md) | Compose HTTP modules through `buildHandlers` returning `server.RouteRegistrar` values; keep lifecycle in the composition root and avoid a plugin framework. | Accepted (amended by 0017) |
| [0017. Encapsulate repository construction inside handler composition](decisions/0017-encapsulate-handler-repository-wiring.md) | Construct feature repositories inside `buildHandlers` so `server.New` never enumerates them. | Accepted |
| [0012. Consumer-owned ports remain the dependency-inversion boundary](decisions/0012-consumer-owned-ports-dependency-audit.md) | Audit the import graph against the dependency rule: ports stay with consumers, adapters and the composition root point inward, and no production code needed to change. | Accepted |
| [0013. Redis cache-aside for task listing with generation-token invalidation](decisions/0013-redis-cache-aside-for-task-list.md) | Cache `GET /tasks` in Redis with a generation token; invalidate after committed creates, updates, and deletes, and fail open to PostgreSQL when Redis is unavailable. | Accepted |
| [0014. Task filtering and pagination on a paginated collection envelope](decisions/0014-task-filtering-and-pagination.md) | Add `status`/`assignee` filters and original page-number pagination (later replaced by cursor pagination). | Accepted (pagination superseded by 0016) |
| [0016. Cursor (keyset) pagination for GET /tasks](decisions/0016-cursor-pagination-for-task-list.md) | Replace `page`/`total`/`total_pages` with opaque keyset cursors bound to filters; update Redis variant keys and drop per-page `COUNT(*)`. | Accepted |
| [0015. Load testing `GET /tasks` with a small Go runner and opt-in profiling](decisions/0015-load-testing-and-profiling.md) | Measure the list endpoint with a reproducible, self-contained HTTP runner and a loopback-only pprof listener; report measured numbers instead of targets. | Accepted |

No record is superseded or deprecated. ADRs 0003, 0004, and 0007 carry amendment
notes because later records refine part of their decision; read the amending ADR
for the current behavior. The ADR index and its status column are maintained in
[`decisions/README.md`](decisions/README.md).
