# 0004. Startup errors are returned to the process edge; `/health` reports database reachability

* Status: Accepted
* Date: 2026-10-19
* Scope: `internal/app/app.go`, `cmd/task-manager/main.go`,
  `internal/infra/server/`

## Context

Two questions have to be settled before the service grows:

- **How does initialization fail?** A common shortcut is to panic on a
  missing or invalid required setting. That fails fast, but it moves policy
  into the composition root, makes failures hard to assert in tests, and
  produces a stack trace where a diagnostic message would be more useful.
- **How do we know the process is wired correctly?** Without any endpoint
  that touches the database, a successfully started process proves only that
  a socket was opened. Configuration could be ignored, the pool could be
  misconfigured, and nothing would reveal it until the first feature request.

The service also has no tables yet, so any check that inspects domain data
would be inventing schema before it is designed.

## Decision

1. **`App.Initialize` and `App.Start` return `error`.** No panics on
   configuration or infrastructure failure. `main` logs with `log.Fatalf` and
   exits with status 1, so the decision to terminate belongs to the process
   edge.
2. **Configuration and connection failures are wrapped with context.**
   `database.New` returns `database: DSN is required` or
   `database: invalid DSN: ...`, and `Initialize` wraps it as
   `app: initialize database: ...` so the operator sees which stage failed.
3. **Expose `GET /health` from the HTTP adapter** with this contract:

   | Condition | Status | Body |
   | --- | --- | --- |
   | Database ping succeeds | `200` | `{"status":"ok"}` |
   | Database ping fails or times out | `503` | `{"status":"error","error":"database unreachable"}` |

   The ping uses a 2-second timeout derived from the request context, so a
   hanging database cannot hold the endpoint open indefinitely.
4. **Declare `HealthChecker` at the point of use.** The port lives in
   `internal/infra/server` and requires only `PingContext(ctx) error`.
   `*sql.DB` already satisfies it, so `internal/app` passes the pool directly
   and the server never imports `internal/infra/database`.
5. **Allow a nil checker for tests.** `server.New` accepts a nil
   `HealthChecker`; in that case `/health` returns `200 {"status":"ok"}`.
   Production always passes a pool.
6. **Shut down in reverse order of construction.** `Start` blocks on the HTTP
   server until `SIGINT`/`SIGTERM`, drains in-flight requests with a 10-second
   budget, then closes the pool and returns any server error to `main`.

## Alternatives considered

1. **Panic inside `Initialize`.** Rejected: a returned error is explicit at
   the call site, testable without recovering panics, and keeps the exit
   decision and exit code in `main`.
2. **No health endpoint until a container or orchestrator needs one.**
   Rejected: the endpoint is the only current runtime evidence that
   config → pool → server wiring works, and it gives tests a real HTTP surface
   to assert against.
3. **A deep health check that queries a domain table.** Rejected: no schema or
   migrations exist; such a check would encode assumptions about persistence
   that have not been decided. It would also conflate application readiness
   with connectivity.
4. **Separate liveness and readiness endpoints now.** Rejected as premature:
   there is one process and one dependency. Splitting the contract before
   there is a deployment environment that distinguishes the two would fix
   semantics without evidence.
5. **Implement the health handler inside `internal/infra/database`.**
   Rejected: the response format, status codes, and timeout are HTTP concerns.
   Putting them in the adapter would invert the dependency direction from
   ADR 0001.

## Trade-offs

- **Gain:** misconfiguration fails at startup with a precise message; the
  process never panics on operator error; `/health` exercises the real pool
  without business logic; a stub makes the handler testable offline.
- **Cost:** `/health` reports infrastructure reachability, not application
  correctness. A reachable database with a missing table still returns `200`.
- **Cost:** the process can be "up but unhealthy" — because pool construction
  is lazy (ADR 0003), startup succeeds while the database is down and
  `/health` returns `503`. This is intentional but must be documented for
  operators.
- **Cost:** a single endpoint serves both liveness and readiness use cases, so
  an orchestrator that restarts on failed liveness probes would restart a
  process that is merely waiting for the database.
- **Mitigation:** keep the check shallow and bounded; revisit the split when a
  deployment environment or schema exists that makes the distinction
  actionable.

## Consequences

- Unit tests cover configuration defaults and overrides, DSN validation
  without dialing, and `/health` with a stub checker returning success and
  failure.
- Integration-level ping behavior against a real PostgreSQL is not automated;
  it is verified manually by running the service against an instance.
- Health has a single implementation; any future readiness/liveness split must
  update this record or supersede it.
- Future middleware (authentication, metrics, tracing) registers beside
  `/health` in the server adapter, not inside domain packages.
- Startup failures never produce a partially initialized process: the HTTP
  server is constructed only after the pool exists.
