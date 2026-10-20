# 0010. Prometheus metrics and request correlation for HTTP observability

* Status: Accepted
* Date: 2026-10-21
* Scope: `internal/infra/metrics/`, `internal/infra/server/`,
  `internal/task/repository/postgres/`, `internal/infra/config/`,
  `internal/app/`, `go.mod`
* Related: [0004](0004-startup-errors-and-health-endpoint.md) — health checks
  answer "can this process serve?"; these metrics describe how it is serving;
  [0006](0006-postgres-task-persistence.md) — the `tasks` table is the source
  of `tasks_count`; [0009](0009-openapi-contract.md) — `/metrics` is
  deliberately not part of the JSON API contract.

## Context

The service had a health probe and a request log, but no way to answer
operational questions over time: how many requests are failing, how slow the
API is, and how many tasks are stored. Those answers should not require
querying production PostgreSQL by hand or parsing free-form log lines.

The constraints come from the existing architecture and the service's size:

- HTTP instrumentation belongs in the HTTP adapter layer; the domain and use
  cases must not import Gin or a metrics client.
- The service already has a single composition root, one PostgreSQL adapter,
  and a narrow-port style. New wiring should follow those patterns, not add a
  new framework or a parallel lifecycle.
- A small service does not justify a metrics platform, an agent sidecar, or a
  stats pipeline. A pull-based Prometheus endpoint is the smallest useful
  mechanism, and it still works with any Prometheus-compatible scraper.
- Metrics labels are a public cardinality surface. A route label built from a
  raw URL would create a time series per task ID; an indicator built per user
  or per request body would be worse. Bounded labels are a design requirement,
  not a refinement.
- The database can change independently of this process. A count maintained
  only by this process's writes would be wrong after a restart or after any
  external SQL statement, and reporting a fabricated zero while PostgreSQL is
  unavailable would be worse than reporting nothing.

Tracing needs the same restraint. Correlating one request across its log lines
is useful and cheap. Distributed tracing requires propagation of a span
context, sampling decisions, span export, and a backend, none of which exist in
this repository.

## Decision

### Metrics

Prometheus instrumentation is implemented with
`github.com/prometheus/client_golang`. The collectors are defined in
`internal/infra/metrics`, which knows nothing about Gin; the HTTP package calls
`ObserveRequest` and mounts `Handler`.

| Metric | Type | Labels | Unit | Meaning |
| --- | --- | --- | --- | --- |
| `requests_total` | Counter | `method`, `route`, `status` | requests | Every completed HTTP request, successful or not. |
| `request_latency_histogram` | Histogram | `method`, `route`, `status` | seconds | Request duration from the start of the middleware to completion. |
| `tasks_count` | Gauge | none | tasks | Last successfully observed row count of `tasks`. |
| `tasks_count_last_refresh_timestamp_seconds` | Gauge | none | Unix seconds | When `tasks_count` was last refreshed successfully. |

`tasks_count_last_refresh_timestamp_seconds` is an addition to the three
required metrics. Without it, a stale `tasks_count` is indistinguishable from a
fresh one; with it, an alert can fire on `time() - timestamp` exceeding a
threshold.

Latency uses seconds, following Prometheus convention. Buckets are
`0.001, 0.0025, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10`,
which covers sub-millisecond handler work through a slow database round trip
without pretending to resolve finer differences than this service needs.

### Bounded labels

`route` is Gin's registered pattern (`/tasks/:id`), never `c.Request.URL.Path`.
A request that matches no route — including `404` and `405` responses — is
recorded as `route="unmatched"`, so a scanner probing random URLs cannot grow
the series count. The number of registered routes bounds the label set.

`method` is normalized to the standard HTTP verbs; anything else becomes
`other`. A client-chosen verb would otherwise create a new series.

`status` is the numeric response code as a string. Codes are bounded by the
HTTP protocol, and the label keeps failures separable from successes without a
second metric.

The metrics endpoint itself is excluded from `requests_total` and
`request_latency_histogram`. Scrapes are health traffic, not application
traffic; counting them makes request rates depend on the scrape interval. The
endpoint is still request-logged, so a failing scrape is visible in logs.

### `tasks_count` design

`tasks_count` is read from PostgreSQL by `postgres.Repository.CountTasks`,
which executes `SELECT count(*) FROM tasks`. This is the only implementation:
there is no process-local increment on create, update, or delete, because such
a counter is wrong after a restart, after a second application instance writes,
or after a manual statement, and it cannot see rows this process did not
create.

A full-table count is cheap at this scale but is not free. It therefore runs on
a schedule, not per request:

- `internal/infra/metrics.TaskCountRefresher` refreshes once at startup and
  then every `METRICS_TASKS_REFRESH_INTERVAL` (default `15s`).
- The refresher runs in one goroutine started by `internal/app.Start` and
  stopped by the same signal context that stops the HTTP server. Shutdown
  cancels the context and waits for the goroutine before the database pool is
  closed, so an in-flight count is cancelled rather than racing a closed
  handle.
- Refreshes never overlap. The loop consumes ticks sequentially and
  `TaskCountRefresher.refresh` additionally holds a mutex, so a slow query
  delays the next refresh instead of stacking a second one.
- Every refresh uses the caller's context, so cancellation interrupts a query
  that is waiting on the database.

**Consistency model.** `tasks_count` is eventually consistent: it reflects the
database at the last successful refresh, up to one interval plus the duration
of that query behind. A create, delete, or external write is visible after the
next refresh. This is deliberate: per-request counting would couple every HTTP
request to a table scan, and per-write increments would be incorrect after a
restart or an out-of-process change.

**Failure and stale-data behavior.** A failed refresh never publishes a value.
The collector is only updated after a successful count, so:

- before the first successful count, `tasks_count` and its timestamp are
  **absent** from the exposition — not zero;
- after a successful count, a later failure leaves the last known count and its
  last-refresh timestamp in place, and the refresh loop logs a warning each
  time. The value is stale, not fabricated, and the unchanged timestamp makes
  the staleness observable;
- a negative count (which PostgreSQL cannot produce for `count(*)`) is treated
  as a failure rather than published.

`tasks_count` therefore must not be treated as a transactionally consistent
inventory. It is an operational gauge with a documented freshness bound.

### Request correlation

`internal/infra/server` installs a request-ID middleware for every request:

- An incoming `X-Request-ID` is propagated when it is 1–64 characters of
  `[A-Za-z0-9._:-]` and starts with an alphanumeric character. This accepts
  UUIDs, W3C-trace-context-style values, and common proxy identifiers while
  rejecting whitespace, control characters, and unbounded values that could
  forge log lines or inflate every response.
- Otherwise — including a missing, empty, over-long, or malformed value — a
  new 128-bit identifier is generated from `crypto/rand` and encoded as 32
  lowercase hex characters. If the entropy source ever fails, a timestamp plus
  counter fallback is used; correlation IDs need uniqueness, not secrecy, and a
  request must not fail because of observability.
- The final identifier is stored in the Gin context under
  `server.RequestIDContextKey` and exposed by `server.RequestID(c)`, so
  downstream handlers and middleware can use it without re-reading the header.
- The same value is set on the response `X-Request-ID` header before the
  handler runs, so success, `4xx`, `5xx`, and panic responses all carry it.

The request-completion middleware logs one structured line per request with
`request_id`, `method`, `path`, `route`, `status`, `duration`, and `bytes`. It
records `requests_total` and `request_latency_histogram` from the same
observation point. Completion logging is deferred: a panic still produces a log
line and a metric before the panic is re-thrown to `gin.Recovery`. The panic
value and stack are not written to this log line; `gin.Recovery` already emits
the stack, and a panic value can contain data. Request bodies, headers other
than the request ID, credentials, and database connection strings are never
logged.

Elapsed time is measured with `time.Now`/`time.Since`, which use the monotonic
clock, and the duration is recorded for failures as well as successes.

### Request correlation is not distributed tracing

This is correlation within one process and its logs. It does not create spans,
assign span IDs, sample requests, propagate a trace context to other services
or to PostgreSQL, or export to a tracing backend. The `X-Request-ID` header can
be forwarded by a caller, and a gateway that already generates one will have it
propagated, but there is no OpenTelemetry instrumentation, no collector, and no
span store. Calling this "tracing" in a dashboard would overstate it.

OpenTelemetry was rejected for this phase: it adds a dependency tree, a
provider/exporter lifecycle, and configuration for a backend that does not
exist here. The request ID plus structured logs answers the current question
("what happened to this request?") at a fraction of the cost. A future
distributed-tracing decision should be a new ADR, not an extension of this one.

### Registry and endpoint

`metrics.New` constructs a **dedicated** `prometheus.Registry` and registers
exactly the collectors above. Nothing is registered in the global default
registry:

- constructing a second `Metrics` (in a test, or in a second application
  instance in one test binary) cannot panic with `AlreadyRegisteredError`;
- tests observe only their own series, so they are order-independent;
- the process does not accidentally expose Go runtime or process collectors
  that were never reviewed.

`GET /metrics` is served by `promhttp.HandlerFor(registry, HandlerOpts{})`.
Because no `HandlerOpts.Registry` is set, `promhttp` does not register its own
`promhttp_metric_handler_*` metrics; and the server does not route the path
through the request instrumentation. A scrape therefore cannot instrument
itself or recursively count scrapes. The handler writes the standard
Prometheus text exposition format, not the JSON error envelope, and requires no
new dependency at runtime beyond the client library.

The endpoint is registered only when instrumentation is wired; `internal/app`
always wires it. It is available in Docker Compose through the existing
published application port. No PostgreSQL or other internal port is published
to make metrics reachable.

### API documentation

`/metrics` is deliberately **not** added to `api/openapi.yaml`. That document
describes the JSON task and health API; the exposition format is a different
media type, is produced by a library, and is not a JSON API response. Documenting
it as an OpenAPI operation would either misrepresent its body or require a
parallel schema for a standard format. The README documents the endpoint, its
metrics, labels, units, and a `curl` example instead. The Swagger UI and the
existing task/health contract are unchanged.

## Alternatives considered

1. **The global default Prometheus registry.** Rejected: registration becomes
   process-global, tests can collide, and any library that registers there
   silently joins the exposed set. A dedicated registry keeps ownership and
   test isolation explicit.
2. **Count tasks per HTTP request.** Rejected: it puts a table scan on the
   request path, making latency depend on table size for a number that changes
   only on writes.
3. **Increment and decrement a local gauge on create/delete.** Rejected: it is
   wrong after a restart, wrong with more than one application instance, wrong
   after manual SQL, and it cannot reflect rows created elsewhere. The task
   specifically requires the persisted state.
4. **Cache the count forever after startup.** Rejected: it would be stale
   immediately after the first write, with no bound and no signal.
5. **Publish zero when the count query fails.** Rejected: zero is a valid and
   alarming-looking value, so it would silently turn an availability problem
   into a false data point. Absent-before-first-success and stale-after-failure
   are honest.
6. **Use `sql.DB.Stats()` or driver-level metrics instead of table state.**
   Rejected as an answer to "how many tasks exist": pool statistics describe
   connections, not rows.
7. **Label by raw URL path or task ID.** Rejected: unbounded cardinality; a
   single client looping over IDs can exhaust memory and scrape size.
8. **Label by client IP, user agent, or request body fields.** Rejected: high
   cardinality and, for body fields, sensitive data in metrics.
9. **Standard Go/process collectors in the same registry.** Deferred: they are
   useful but were not requested and would expose a larger surface than this
   phase needs. Adding them is a small, reviewable follow-up.
10. **OpenTelemetry or a tracing backend.** Rejected for this phase: see
    above. The requirement is correlation, and a new platform would not be
    justified by the current operational need.
11. **Count `/metrics` scrapes in `requests_total`.** Rejected: request rate
    would then depend on the scrape interval and the endpoint would observe
    itself. Scrapes remain visible in logs and in the scrape timestamp.
12. **Add `/metrics` to OpenAPI.** Rejected: it is not a JSON API operation and
    would misdescribe a standard exposition format.

## Trade-offs

- **Gain:** the service exposes request rate, error rate, latency, and task
  inventory in a standard format, with no agent, sidecar, or database query per
  request.
- **Gain:** bounded labels keep memory and scrape size predictable even under
  probing or ID-heavy traffic.
- **Gain:** a dedicated registry makes tests deterministic and duplicate-free.
- **Gain:** request IDs make log correlation possible without a tracing
  platform, and every response carries the identifier.
- **Cost:** `tasks_count` is eventually consistent, up to one refresh interval
  plus query time. It is not suitable for billing or exact inventory.
- **Cost:** the gauge has no history by itself; a scrape must observe it to
  produce a time series. A short-lived process may expose no value before it
  exits.
- **Cost:** the extra timestamp metric is a fourth series to document and
  monitor.
- **Cost:** `/metrics` is unauthenticated like the rest of the API. It exposes
  aggregate counts and route names but no task content or credentials. It must
  not be exposed to an untrusted network without network controls or an
  authenticating proxy.
- **Cost:** the sample count per route is bounded by registered routes times
  methods times status codes; an unusually large API would need a review of the
  label strategy.
- **Cost:** request correlation is not distributed tracing and does not follow
  work into PostgreSQL or another service.

## Consequences

- New HTTP routes are instrumented automatically through the router pattern;
  there is no per-handler metric code to remember. A route added outside the
  shared router would not be observed.
- Any change to metric names, labels, types, buckets, units, or the refresh
  interval is a contract change and must update this record and the README.
- The `tasks` table remains the single source of truth for `tasks_count`;
  create, update, and delete paths must not grow a parallel counter.
- Adding a task-related table or filtering tasks by another process does not
  change the meaning of `tasks_count`: it counts rows in `tasks`.
- The refresh interval is configured with
  `METRICS_TASKS_REFRESH_INTERVAL`; non-positive or unparsable values fall back
  to `15s`, and the refresher itself rejects non-positive intervals.
- Tests must use `metrics.New` (or the composition root) rather than the
  global registry, so they remain isolated.
- `/metrics` is intentionally absent from `api/openapi.yaml`; the README is its
  documentation. Revisit that only if the service gains a formal metrics
  contract.
- Distributed tracing remains unimplemented. If it is added, it needs its own
  ADR covering propagation, sampling, export, and backend choice.
