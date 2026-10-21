# 0005. Task CRUD on a service-owned repository port

* Status: Accepted
* Date: 2026-10-20
* Scope: `internal/task/**`, `internal/infra/handler/`,
  `internal/infra/server/`, `internal/app/`
* Amends: [0004](0004-startup-errors-and-health-endpoint.md) — the database is
  required, startup verifies the schema, and health is split into readiness and
  liveness.
* Later amendments: [0014](0014-task-filtering-and-pagination.md) — `GET /tasks`
  gained filters, pagination, and a page envelope; create moved to `POST /task`
  in a follow-up fix so the collection path stays read-oriented.

## Context

PostgreSQL persistence is in place
([ADR 0006](0006-postgres-task-persistence.md)): the task aggregate, the
`service.Repository` port, an explicit-SQL adapter, and versioned migrations.
The process still exposes only health endpoints, so the task contract —
resource shape, status values, validation, error format — is the remaining
design work.

Persistence was built first for two reasons: the schema follows the aggregate
instead of a guessed API, and the adapter is tested against the real engine
before any HTTP mapping exists. The architecture rules in
[ADR 0001](0001-clean-architecture.md) and
[ADR 0002](0002-package-layout-and-composition-root.md) require the core to be
testable with stubs and the composition root to own wiring; this decision
applies them to the first feature.

## Decision

### Task model

The aggregate lives in `internal/task/domain` and has exactly four fields:

| Field | Type | Rules |
| --- | --- | --- |
| `ID` | string | Assigned by the repository, never by clients. A version 4 UUID in canonical form. |
| `Title` | string | Required. Trimmed before storage and validation; a whitespace-only title is invalid. |
| `Description` | string | Optional. May be empty; otherwise stored as sent. |
| `Status` | `domain.Status` | Closed set: `todo`, `in_progress`, `done`. Default `todo` when the request omits it. |

No timestamps, ownership, priority, tags, or ordering fields are added. They
have no use case in this phase, and each would need its own contract and
storage decision.

Status is a label, not a workflow: any allowed value may be set on create or
update, and no transition rules (for example `todo → in_progress → done` only)
are enforced in this phase. A transition model would need its own decision and
tests.

The identifier format is validated by `domain.IsValidID` with a UUID pattern.
Malformed identifiers are rejected as invalid input (`400`) rather than
reported as missing (`404`), because no record with such an identifier can
exist.

### Ports and use cases

`internal/task/service` implements create, list, get, update, and delete and
declares the port it consumes:

```go
type Repository interface {
	Create(ctx context.Context, task domain.Task) (domain.Task, error)
	List(ctx context.Context) ([]domain.Task, error)
	Get(ctx context.Context, id string) (domain.Task, error)
	Update(ctx context.Context, task domain.Task) (domain.Task, error)
	Delete(ctx context.Context, id string) error
}
```

The adapter owns identifier generation, so `Create` receives a task without an
ID and returns the stored copy. `service.ErrNotFound` is the single
missing-record sentinel; adapters return it and the HTTP layer maps it with
`errors.Is`. Validation failures are `*domain.ValidationError` values carrying
a field name.

The service performs validation (`domain.ParseStatus` plus `domain.Validate`)
before touching the repository, and wraps unexpected repository failures with
context while preserving them for `errors.Is`. It imports neither Gin nor
`database/sql`.

### HTTP contract

`internal/task/handler` registers five routes on `gin.IRouter`. The paths below
are the **current** contract (create is not under the collection path; list is
paginated — see [ADR 0014](0014-task-filtering-and-pagination.md)):

| Method | Path | Success |
| --- | --- | --- |
| `POST` | `/task` | `201` with the created task and a `Location: /tasks/{id}` header |
| `GET` | `/tasks` | `200` with a page envelope (`items`, `page`, `page_size`, `total`, `total_pages`); `items` is `[]` when empty |
| `GET` | `/tasks/:id` | `200` with the task |
| `PUT` | `/tasks/:id` | `200` with the replaced task |
| `DELETE` | `/tasks/:id` | `204` with no body |

`POST /tasks` is not registered; an unsupported method on `/tasks` returns
`405 method_not_allowed`. Filtering (`status`, `assignee`) and pagination
(`page`, `page_size`) are defined in ADR 0014; this record retains the
resource-level CRUD rules.

Request bodies are decoded strictly: unknown fields, trailing content, wrong
types, and empty bodies are `400 invalid_request`. Validation failures are
`400 validation_error` with a `fields` map. Unknown IDs and missing tasks are
`404 not_found`. Unexpected failures are `500 internal_error` with a generic
message; the original error is logged, never returned.

Every API error uses the envelope in `internal/infra/handler`, the shared HTTP
delivery package. Domain and service code never import it; only the HTTP
adapters write responses through it. The package is named `handler` because
HTTP adapters, shared or feature-local, live in a `handler` package.

```json
{
  "error": {
    "code": "validation_error",
    "message": "invalid task data",
    "fields": {"title": "title is required"}
  }
}
```

`fields` is omitted for non-validation failures. Codes are stable identifiers:
`invalid_request`, `validation_error`, `not_found`, `method_not_allowed`,
`internal_error`. Unknown routes answer `404` and unsupported methods answer
`405` with the same envelope, configured once on the router
(`HandleMethodNotAllowed`, `NoRoute`, `NoMethod`).

`PUT` is a full replacement of the mutable fields: `title` is required,
`description` is replaced (empty when omitted), `status` defaults to `todo`
when omitted, and `assignee` defaults to empty (unassigned), exactly as for
creation. A missing ID is never upserted.

List order is defined by the adapter: ascending `id`. The order is stable
across calls but is not creation order; clients must not treat it as such.
When the result is paginated, `total` / `total_pages` describe the filtered
set before pagination (ADR 0014).

### PostgreSQL adapter

`internal/task/repository/postgres` implements the port with explicit SQL over
`*sql.DB`. The table, statements, identifier retry policy, and constraint rules
are decided in [ADR 0006](0006-postgres-task-persistence.md); this record takes
the adapter as given and only relies on its contract:

- `Create` generates the identifier and returns the stored copy.
- A missing row becomes `service.ErrNotFound`; malformed identifiers surface as
  decoding errors, not as not-found.
- `ListPage` returns one page ordered by `id` plus the filtered total (ADR 0014).
- Every method returns freshly scanned values, so callers cannot mutate adapter
  state.

The adapter is covered by integration tests gated by `TEST_DATABASE_URL`, which
refuse to run against a database whose name does not contain `test`. Service
and handler tests use a hand-written repository double and need no database.

### Wiring and runtime behavior

`internal/app` is the only wiring point:

1. `DATABASE_URL` is required. An empty value fails startup before any other
   adapter is built.
2. The pool is pinged with a 5-second budget, then `database.VerifySchema`
   requires the `tasks` table. Either failure closes the pool and stops
   startup, so the process is never up but unable to serve a request.
3. `postgres.New(pool)` → `service.New(repo, cache, logger)` → `handler.New(service, logger)`
   (cache may be nil; Redis wiring is [ADR 0013](0013-redis-cache-aside-for-task-list.md))
   builds the task stack, and the handler is passed to `server.New` as a
   `RouteRegistrar`.

`internal/infra/server` depends on a narrow `RouteRegistrar` port
(`RegisterRoutes(gin.IRouter)`) instead of importing feature packages, so route
composition happens in the composition root.

Runtime behavior:

- `GET /health` is the readiness probe. A successful ping returns
  `200 {"status":"ok","database":"ok"}`; a failed ping returns
  `503 {"status":"error","error":"database unreachable"}`.
- `GET /health/live` is the liveness probe: `200 {"status":"ok"}` without
  touching the database.
- A nil health checker — reachable only in tests that construct the server
  directly — returns `503 database not configured` rather than a misleading
  `200`.
- There is no in-memory adapter, no fallback, and no configuration that makes
  the API serve volatile storage.

## Alternatives considered

1. **Build the HTTP API against an in-memory adapter first.** Rejected:
   persistence was already implemented and tested against the real engine
   ([ADR 0006](0006-postgres-task-persistence.md)). A second adapter would have
   to emulate ordering, constraints, and durability, and would exist only to be
   deleted.
2. **A shared `ports` package or a generic CRUD repository.** Rejected by ADR
   0001 and ADR 0003: one consumer, one aggregate, and a port shaped by real
   use cases. `TaskService` and `Repository` are each declared by their
   consumer.
3. **Integer identifiers from a counter.** Rejected: sequential IDs leak
   creation volume and order, and they make identifiers guessable. UUIDs are
   opaque, can be generated by any adapter without coordination, and match the
   PostgreSQL schema.
4. **Upsert semantics for `PUT` on a missing ID.** Rejected: it hides client
   mistakes and makes creation reachable through two endpoints. Creation is
   `POST /tasks`; `PUT` on an unknown ID is `404`.
5. **Partial-update semantics for `PUT` (PATCH-like).** Rejected: a `PUT` body
   is the full representation, so omitting `status` resets it to the default.
   Supporting both replace and merge semantics in one verb would be ambiguous.
6. **Reporting malformed IDs as `404`.** Rejected: the identifier space is
   defined by the server, so a malformed ID is invalid input, not a
   well-formed identifier that happens to be missing.
7. **Permissive JSON decoding.** Rejected: silently ignoring unknown fields
   makes typos (`statuz`) succeed with the default and hides contract drift.
8. **Keeping `DATABASE_URL` optional and falling back to another store.**
   Rejected: a process that switches storage based on configuration is a
   production hazard; the repository is the only task store and startup fails
   closed without it.

## Trade-offs

- **Gain:** the HTTP contract is small and explicit, the core has no framework
  or driver dependency, and the adapter was already proven against PostgreSQL
  before the API was layered on it.
- **Cost:** the API cannot run without PostgreSQL. Local development and every
  integration run need a database, which is why the suite is split into
  database-free and database-backed tiers.
- **Cost:** strict JSON decoding rejects unknown fields, which is less
  forgiving for clients that send extra metadata.
- **Cost:** UUID string identifiers are larger than integers and make list
  output less compact.
- **Cost:** `400` for malformed IDs and `404` for well-formed unknown IDs is
  two failure modes clients must handle; it is more precise but slightly more
  work.
- **Cost:** `PUT` resets omitted fields to their defaults, so clients must send
  the full representation even when changing one field.
- **Mitigation:** the port isolates the adapter, and handler tests use a
  hand-written double, so the HTTP contract is still testable without a
  database.

## Consequences

- The task API is served only from PostgreSQL. A request never reaches a
  volatile store, and a missing or unmigrated database stops startup with an
  actionable error.
- Adding a status value or a task field requires changing
  `internal/task/domain`, the HTTP mapping, the schema via a new migration, and
  this record. Statuses are a closed set by design.
- `GET /tasks` order is ascending `id`; changing it is an API change and must be
  reflected here and in the adapter.
- The test suite is the specification of the API contract: status codes, JSON
  shapes, error codes, validation fields, and the full
  create → get → list → update → get → delete → not-found lifecycle.
- Service and handler tests stay database-free; the adapter and migrations are
  covered only when `TEST_DATABASE_URL` is set, so CI must provide PostgreSQL.
- Route composition stays in `internal/app`; `internal/infra/server` never
  imports `internal/task`.
