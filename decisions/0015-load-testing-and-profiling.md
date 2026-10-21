# 0015. Load testing `GET /tasks` with a small Go runner and opt-in profiling

* Status: Accepted
* Date: 2026-10-21
* Scope: `cmd/loadtest/`, `internal/infra/config/`, `internal/app/`,
  `internal/infra/server/` (test guard only), README, CI-adjacent tooling
* Related: [0007](0007-ci-with-postgresql-service.md) — reproducible checks
  against real infrastructure; [0010](0010-prometheus-metrics-and-request-correlation.md)
  — the metrics and correlation already exposed; [0013](0013-redis-cache-aside-for-task-list.md)
  — the cache whose benefit this scenario measures;
  [0014](0014-task-filtering-and-pagination.md) — the query surface the
  scenario exercises.

## Context

The service has filters, pagination, a Redis list cache, Prometheus metrics, and
a test suite that proves behavior, but no measurement of what the list endpoint
costs under load. Three questions could not be answered from the existing
artifacts:

- How much throughput and latency does `GET /tasks` actually deliver on the
  development environment?
- Does the Redis cache change the observable numbers, and can that be
  demonstrated rather than assumed?
- Where does CPU time go during a warm run — the handler, JSON, Redis, or
  socket I/O?

Answering them required a load generator that is reproducible, small enough to
read, safe against a real database, and honest about which cache path it
exercised. It also required pprof, which must not become an always-on surface:
the API is unauthenticated and `net/http/pprof` exposes command lines, stacks,
and CPU profiles.

`GET /tasks` was selected because it is the only read path with filters,
pagination, a cache, and declared metrics; it is also the most frequent read in
any realistic client. Optimizing or adding endpoints was out of scope.

## Decision

### Workload tool

Add `cmd/loadtest`, a single-binary Go program with no new dependencies (the
service already depends on `go-redis`). It sends `GET /tasks` with:

- configurable `-base-url`, `-query`, `-requests`, `-concurrency`, `-timeout`,
  and `-warmup`;
- a fixed number of goroutines equal to `-concurrency`, each issuing requests
  sequentially, so request and goroutine counts are bounded;
- a latency slice sized to `-requests` (capped at one million) and no other
  per-request retention;
- per-request `time.Since` timing and a monotonic wall-clock measurement of the
  measured phase;
- nearest-rank p50/p95/p99 from sorted observed durations, never derived from a
  mean;
- success defined as a 2xx response with a fully drained body; non-2xx statuses
  and transport errors are counted, sampled in the report, and make the process
  exit non-zero;
- warm-mode warmup (when `-warmup > 0`) is required for a valid warm claim: any
  warmup failure aborts the run before measurement;
- exit status `2` for unusable options (including a base URL that contains a
  path), `1` for failed requests, failed warmup, failed cleanup, or required
  cache-evidence checks that are missing or contradict the mode, `0` only for a
  fully successful run.

Response bodies are discarded. The report never includes task content, and
failure samples contain only the status code or transport error text.

### Repeatable, self-owned data

`-seed N` creates tasks through the public `POST /task` API, not SQL, so the
tool also exercises the write contract and invalidates the cache the way real
traffic would. Each run:

1. counts existing tasks by walking `GET /tasks` with cursor pagination
   ([ADR 0016](0016-cursor-pagination-for-task-list.md));
2. creates `N` tasks with a run-unique title prefix (`loadtest-<unix-nano>-`) and
   assignee `loadtest`;
3. re-reads `total` and aborts unless it is `baseline + N`;
4. deletes exactly the IDs it created after measuring (accepting `404`).

Existing rows are never modified or deleted, repeat runs do not accumulate
rows, and an interrupted run leaves identifiable rows that the next run's
`total` output exposes. Mutations are refused on non-loopback hosts unless the
operator passes `-allow-remote-mutations`.

### Cache-warm and cache-miss as distinct, verified scenarios

The service caches one entry per normalized query under a generation token
(ADR 0013). That makes both scenarios verifiable:

- **warm**: a warmup phase sends the same query, then the measured phase reuses
  it. When `-redis-url` is omitted, evidence is skipped and the HTTP timing
  alone is reported. When `-redis-url` is set, evidence is **required**: the
  tool fails the run (exit 1) unless the measured phase shows zero
  `keyspace_misses`, an unchanged list-key count, and at least one
  `<prefix>:tasks:list:*` variant key still present.
- **miss**: before each timed request the tool writes 128 fresh random bits to
  `<prefix>:tasks:list:generation`, so the following lookup addresses a
  generation that was never cached. The variant read is then a Redis miss and
  the request reads PostgreSQL. This mode requires `-concurrency 1`, because a
  concurrent request could repopulate the cache and turn later requests into
  hits. `-redis-url` is mandatory; evidence must show at least one new list key
  and one keyspace miss per measured request, or the run fails.

The comparison is therefore stated as a single-threaded pair, not as a claim
about concurrent behavior. Measured numbers live in the README section
"Performance testing and profiling"; this record does not restate them.

### Profiling is opt-in, loopback-only, and off the API router

`PPROF_ADDR` is a new configuration value, empty by default. When set,
`internal/app` binds a separate `http.Server` on that address with an explicit
`net/http/pprof` mux. The address must parse as a loopback host; empty hosts and
wildcard addresses are rejected at startup because they would expose profiles on
every interface. The listener:

- is never registered on the Gin router, so the API port has no pprof routes
  (guarded by a server test);
- is not set in `compose.yaml`, so the containerized service cannot expose it
  accidentally;
- is served by a goroutine started with the API server and shut down after it,
  with a five-second budget for an in-flight capture;
- writes profiles only where the operator points `curl`; `*.pprof` is ignored by
  Git.

The README documents capturing a CPU profile with `curl` during the load phase
and inspecting it with `go tool pprof` against the built binary. A heap profile
is documented as a separate, optional measurement.

## Alternatives considered

- **`hey`, `vegeta`, `wrk`, or `k6`.** Mature tools, but each adds an external
  install or a non-Go runtime, and none knows how to seed and clean up this
  API or how to force a verified cache miss through the generation token. A
  small Go program stays in the repository's own toolchain and can share the
  API contract.
- **A Go benchmark against `httptest`.** It would measure handler and service
  code in-process, bypassing the HTTP server, real sockets, Redis, and
  PostgreSQL. That is a different measurement and was rejected as the primary
  answer; the HTTP scenario is the artifact users run.
- **Seeding with SQL.** Faster, but couples the tool to the schema and skips the
  write path that invalidates the cache. It would also make cleanup less
  obviously scoped to the tool's own rows.
- **Flushing Redis or deleting cache keys to force misses.** A `FLUSHDB` is
  destructive to other clients, and deleting the generation key only races the
  service's initialize-on-read path. Replacing the generation token with a
  fresh random value is a supported write that produces a guaranteed miss for a
  request observed after it.
- **A concurrent miss run.** It cannot be labeled a miss: the first request
  repopulates the variant, so later requests hit. Reporting it as a miss would
  be inaccurate, which is why miss mode is serialized instead.
- **Mounting pprof on the Gin router behind a flag.** One misconfiguration
  exposes profiles on the public port. A separate listener with a loopback
  address check fails closed.
- **Always-on profiling or OpenTelemetry.** Both are broader observability
  infrastructure than this increment needs; the service already has Prometheus
  metrics and request IDs (ADR 0010).

## Trade-offs

- The load generator and the service run on the same machine, so client CPU
  competes with server CPU and loopback removes network latency. Numbers are
  comparative, not capacity planning figures.
- The dataset is small (2000 rows). It is large enough for the count query and
  pagination to be real, but not large enough to expose index or plan behavior
  at scale.
- The warm path crosses a Docker VM boundary on macOS because Redis runs in the
  Compose container; that inflates warm latency relative to native Redis.
- Redis `keyspace_*` counters are server-wide, so evidence assumes no other
  client traffic; the tool runs against a dedicated database and the README
  states the assumption.
- Miss mode is serialized by design, so it cannot report concurrent throughput.
- The profile window may include a fraction of dataset creation and cleanup; the
  README quantifies that and the top frames are dominated by the list path.
- There is no automated performance gate. The scenario is manual, and the
  numbers in the README are environment-specific measurements, not guarantees.

## Consequences

- `cmd/loadtest` is the supported way to repeat the measurement. Changes to the
  list contract, cache key layout, or generation semantics must keep its
  evidence checks meaningful (`task-manager:tasks:list:<token>:<variant>`).
- The pprof listener stays disabled by default and loopback-only. Do not add it
  to Compose or to the public router; a startup rejection or a failing test is
  the intended outcome of such a change.
- Performance claims in documentation must reference measured output and the
  environment it was measured on. If a measurement cannot be reproduced, the
  README must label it as not collected rather than extrapolate.
- Future work, not implemented here: a repeatable comparison matrix over
  `page_size` and dataset sizes, PostgreSQL-side `EXPLAIN (ANALYZE)` for the
  count query, and an optional CI job that records (but does not enforce)
  throughput on a fixed runner.
