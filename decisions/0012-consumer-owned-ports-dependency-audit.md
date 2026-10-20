# 0012. Consumer-owned ports remain the dependency-inversion boundary

* Status: Accepted
* Date: 2026-10-21
* Scope: whole repository; audited `internal/task/**`, `internal/infra/**`,
  `internal/app/`, and `cmd/`
* Related: [0001](0001-clean-architecture.md) — the dependency rule this audit
  verifies; [0002](0002-package-layout-and-composition-root.md) — the single
  composition root and the rejection of a shared dependency bag;
  [0005](0005-task-crud-api.md) — the task ports and HTTP contract;
  [0011](0011-http-handler-composition.md) — HTTP modules are composed in
  `internal/app`.

## Context

The service has grown since [ADR 0001](0001-clean-architecture.md): the task
slice now has a domain, use cases, an HTTP adapter, and a PostgreSQL adapter;
observability, embedded documentation, and a handler-composition point were
added around it. Growth is where dependency graphs usually decay, so the
architecture was re-audited rather than assumed to still hold.

The audit inspected imports, constructor parameters, interface declarations,
compile-time implementation assertions, composition-root wiring, and test
substitution. It did not infer correctness from package names. A sibling Go
service, the gateway at `~/Desktop/ighe3-v2/gateway`, was used as a reference
for how a larger service organizes modules; its paths below are relative to
that repository and are not part of this module.

### Reference patterns observed in the gateway

The gateway is a migration facade over a Spring backend. It is larger than
Task Manager (MySQL via GORM, Redis, an auth client, SMS/email clients, jobs,
and a `/v5` API surface), but several patterns map directly:

- **Vertical feature slices.** `internal/banner/domain/title.go` holds a pure
  value type; `internal/banner/service/title.go` implements use cases and
  declares `BannerRepository` at the point of use;
  `internal/banner/repository/sql.go` implements that port and asserts it at
  compile time with `var _ bannerService.BannerRepository = ...`;
  `internal/banner/handler/handler.go` declares the `UseCase` port it needs.
- **Consumer-owned delivery port.** `internal/infra/handler/v5/v5.go` declares
  `Subrouter` and accepts feature routers as a variadic list. The server
  (`internal/infra/server/server.go`) knows only the composed
  `*handler.Handlers`, not feature packages.
- **Module constructors and composition.** `internal/banner/module.go` wires
  repository → service → handler; `internal/app/wiring.go` composes modules;
  `internal/app/app.go` builds shared adapters and passes them inward.
- **Hand-written test doubles.** `internal/banner/handler/handler_test.go` and
  `internal/banner/service/title_test.go` use stubs implementing the
  consumer-declared ports, with no mocking library and no database.

The gateway is not uniformly clean, and its deviations were not copied:

- `internal/banner/service/title.go` imports `internal/infra/repository`
  solely for `ErrNotFound`, so an application package depends on an
  infrastructure package. Task Manager keeps its missing-record sentinel in
  `internal/task/service` and lets the adapter import it, which points inward.
- `internal/infra/repository/database.go` exposes a broad `Database` interface
  and `internal/infra/deps/deps.go` is a dependency bag shared by many
  consumers. Task Manager deliberately has neither: persistence contracts are
  narrow and consumer-owned, and the composition root passes concrete adapters
  as the exact ports their consumers declare.
- `internal/infra/handler/v5`, `internal/infra/deps`, and the `/v5`
  public/authenticated/admin groupings earn their complexity from multiple API
  versions and authorization tiers. Task Manager has one transport and one
  flat route namespace, so those abstractions would be speculative
  ([ADR 0011](0011-http-handler-composition.md) already rejected a handler
  facade for the same reason).
- Some gateway handlers (`internal/otp/handler.go`, `internal/adminops/handler.go`,
  `internal/paymentproxy/proxy.go`) call infrastructure clients or read
  configuration directly because they are thin migration endpoints. That is
  not a model for Task Manager, whose task rules belong in the service layer.

### Rules used for the assessment

1. Source dependencies point inward. Domain and application packages must not
   import HTTP, SQL, driver, cache, or metrics packages.
2. A port is declared by its consumer and sized to the consumer's needs.
3. Application code depends on persistence abstractions, not the PostgreSQL
   adapter or `database/sql`.
4. Driven adapters implement the ports required by inner layers and may import
   those inner packages; the reverse is forbidden.
5. HTTP handlers depend on an application contract, not on the concrete
   adapter that happens to implement it.
6. Only the composition root connects concrete adapters to consumers.
7. Domain types and rules do not depend on Gin, PostgreSQL, Redis, HTTP
   transport types, or infrastructure-specific errors.
8. Interfaces exist only where they create a real decoupling or test seam.
9. Tests can replace external dependencies without real infrastructure in the
   unit tiers.

## Decision

### Audit findings

No dependency-rule violations were found. Production code was therefore left
unchanged. The verified graph is:

| Package | Imports from this module | Assessment |
| --- | --- | --- |
| `internal/task/domain` | none (standard library only) | The aggregate, validation, and UUID rules have no framework or driver dependency. `crypto/rand` is a standard-library capability, not an infrastructure adapter. |
| `internal/task/service` | `internal/task/domain` | Use cases depend only on the aggregate and declare `Repository` beside their only consumer. `service.ErrNotFound` is an application error; adapters translate driver errors into it. |
| `internal/task/handler` | `internal/task/domain`, `internal/task/service`, `internal/infra/handler` | A driving adapter importing inward. It declares `TaskService` itself, uses Gin only for transport, and uses the shared HTTP envelope for responses. `service.Input` is an application input contract, not a concrete adapter. |
| `internal/task/repository/postgres` | `internal/task/domain`, `internal/task/service` | The driven adapter implements `service.Repository` and asserts it at compile time. Importing the port owner is the correct direction; the port owner does not import this package. |
| `internal/infra/database` | none (standard library and pgx only) | Owns DSN validation, the pool, and migrations. It exposes `*sql.DB` rather than forcing feature repositories through a broad interface, consistent with [ADR 0003](0003-postgres-via-database-sql.md). `VerifySchema` declares the one-method `SchemaQuerier` it needs. |
| `internal/infra/server` | `internal/infra/handler`, `internal/infra/metrics` | Owns router, middleware, health, and lifecycle. It declares `HealthChecker`, `RouteRegistrar`, and `Instrumentation`; it imports no feature package and no database adapter. |
| `internal/infra/metrics` | none (Prometheus client only) | Declares the one-method `TaskCounter` port and does not import Gin; the HTTP layer drives it through `server.Instrumentation`. |
| `internal/infra/swagger` | `api` | A documentation adapter over the embedded contract; no feature or business package depends on it. |
| `internal/infra/config` | none (standard library only) | Produces plain values; imports no client, server, or driver. |
| `internal/app` | config, database, metrics, server, swagger, task handler, task repository, task service | The composition root, and the only package that imports all concrete adapters together. It passes `postgres.Repository` as `service.Repository` and composes HTTP modules through `buildHandlers` ([ADR 0011](0011-http-handler-composition.md)). |
| `cmd/task-manager` | `internal/app` | Process edge only. |
| `cmd/migrate` | `internal/infra/config`, `internal/infra/database` | A separate deployment entry point that constructs only the adapters a migration needs. It contains no business policy and is not a second application composition root. |

The ports and their owners:

| Port | Declared by (consumer) | Implemented by | Methods |
| --- | --- | --- | --- |
| `service.Repository` | `internal/task/service` | `postgres.Repository` | 5, one per task use case |
| `handler.TaskService` | `internal/task/handler` | `service.Service` | 5, one per HTTP operation |
| `server.HealthChecker` | `internal/infra/server` | `*sql.DB` | 1 |
| `server.RouteRegistrar` | `internal/infra/server` | `task/handler.Handler`, `swagger.Handler` | 1 |
| `server.Instrumentation` | `internal/infra/server` | `*metrics.Metrics` | 2 |
| `metrics.TaskCounter` | `internal/infra/metrics` | `postgres.Repository` | 1 |
| `database.SchemaQuerier` | `internal/infra/database` | `*sql.DB`, `*sql.Conn` | 1 |
| `postgres.scanner` (unexported) | `internal/task/repository/postgres` | `*sql.Row`, `*sql.Rows` | 1 |

Two observations are implementation details, not violations:

- `internal/infra/server` imports the concrete `internal/infra/metrics` for the
  `/metrics` path and the bounded `"unmatched"` route label. Both packages are
  in the outer infrastructure ring, the behavioral dependency is expressed
  through `Instrumentation`, and the constants are a shared endpoint contract
  ([ADR 0010](0010-prometheus-metrics-and-request-correlation.md)). The rule
  constrains inner policy, not cooperation between outer adapters.
- `internal/task/handler` imports `internal/task/service` for `Input` and
  `ErrNotFound`. The direction is outer → inner and the types are application
  contracts. Moving them to a shared package would create a second owner for
  contracts that already have a natural home.

### Reaffirmed rules

Retain the current implementation. The rules above are reaffirmed as the
standard for future code:

- **Where a port goes.** Declare every new capability interface in the package
  that consumes it — a new use-case port in `internal/task/service`, a new
  delivery port in `internal/infra/server`, a new observability port in
  `internal/infra/metrics`. Do not add a shared `ports`, `contracts`, or
  `deps` package.
- **How an adapter connects.** A driven adapter lives outside the consumer
  (`internal/task/repository/postgres` or a future `internal/infra/<adapter>`)
  and imports the consumer package to implement its interface. Use a
  compile-time assertion (`var _ service.Repository = (*Repository)(nil)`)
  where a concrete type satisfies a port. Never move the interface into the
  adapter.
- **How HTTP connects.** A feature handler declares the use-case interface it
  needs, receives it through its constructor, and registers routes through
  `RegisterRoutes(gin.IRouter)`. It may use the shared
  `internal/infra/handler` envelope, but must not import a repository or
  database adapter.
- **How the composition root connects.** `internal/app` is the only place that
  constructs concrete adapters together. `Initialize` owns pool, schema
  verification, repository, service, metrics, refresher, and shutdown order;
  `buildHandlers` composes HTTP modules. A new module adds an entry there, not
  a `server.New` argument.
- **How tests substitute.** Unit tests at a port boundary use hand-written
  stubs implementing the consumer interface. Only adapter tests that exercise
  SQL, migrations, or pool behavior may require `TEST_DATABASE_URL`; the
  database-free tiers must not need a live server.

## Alternatives considered

1. **Introduce a shared `ports`/`contracts` package.** Rejected: it moves
   ownership away from consumers, encourages broad interfaces, and recreates
   the dependency bag that ADR 0002 rejected. It would also make every adapter
   depend on every contract in the package.
2. **Copy the gateway's broad `Database`/`Cache` interfaces and `deps` bag.**
   Rejected: the gateway's breadth is driven by GORM and several shared
   clients. Task Manager uses explicit SQL over `*sql.DB` (ADR 0003) and has
   one aggregate; a broad interface would be speculative and would hide which
   capability each consumer needs.
3. **Move `TaskService` into `internal/task/service`.** Rejected: the handler
   is the consumer, and its port is deliberately narrower in intent than the
   concrete service. Keeping it in the handler preserves the test seam and
   matches the gateway's `UseCase` pattern.
4. **Move `ErrNotFound` to `internal/task/domain` or a shared errors package.**
   Rejected: "no row" is a persistence/application outcome, not an aggregate
   invariant. Defining it in `service` and letting the adapter translate
   `sql.ErrNoRows` into it keeps the driver error confined to the adapter.
5. **Pass `*service.Service` to the handler instead of an interface.**
   Rejected: it would couple HTTP mapping to a concrete application type and
   remove the substitution point the handler tests rely on.
6. **Refactor the server's dependency on the metrics constants.** Rejected as
   cosmetic: it is an outer-ring dependency, and moving the constants would
   either duplicate the route contract or add configuration for a single
   endpoint without changing any inner-layer dependency.
7. **Add an architecture-lint tool (for example `depguard` or `arch-go`).**
   Deferred: the repository is small, the import graph is reviewable in one
   diff, and adding a linter is a new build dependency. The rule is currently
   enforced by review and by the `go list`-based inspection used in this
   audit. Revisit when a second bounded context or transport makes manual
   review unreliable.

## Trade-offs

- **Gain:** the dependency rule is verified against the actual import graph,
  not assumed from package names; no production code was churned for the
  audit.
- **Gain:** consumer-owned ports keep every interface narrow and make each
  adapter's obligations explicit; the compile-time assertions turn a broken
  contract into a build failure.
- **Gain:** unit tests stay database-free because the only unavoidable
  infrastructure tests are the adapter and migration tiers.
- **Cost:** port declarations are distributed across packages, so a reader
  must follow constructors to find them; a shared ports package would be
  easier to browse but would invert ownership.
- **Cost:** the handler depends on `service.Input` and `service.ErrNotFound`,
  so the two packages cannot be read in isolation. The alternative — a shared
  DTO/error package — would add a second source of truth for application
  contracts.
- **Cost:** the server's import of the metrics constants couples two outer
  adapters. This is deliberate and documented, but it means the metrics
  endpoint path is not injectable per server instance.
- **Limitation:** ownership is enforced by review, not by a compiler rule or
  architecture test. The interface declarations are visible, but nothing stops
  a future contributor from adding an inner → outer import.
- **Limitation:** `cmd/migrate` is a second construction site for config and
  the pool. It is a separate process with no business policy, so it is
  acceptable; it should not become a home for application wiring.

## Consequences

- Adding a use case: extend `service.Repository` only if persistence is
  required, implement it in `postgres.Repository`, add the use-case method to
  `service.Service`, and widen `handler.TaskService` only for the routes that
  need it.
- Adding an external capability (cache, queue, another datastore, an auth
  client): declare a narrow interface in the package that calls it, implement
  it under `internal/infra/` or the owning feature slice, and wire it in
  `internal/app`. Do not import the implementation from `domain` or `service`.
- Adding a transport: declare a new consumer-side port; the service and domain
  must remain unchanged. HTTP-specific envelopes and middleware stay in
  `internal/infra`.
- Adding a module's HTTP routes: implement `RegisterRoutes(gin.IRouter)` and
  add the constructor to `buildHandlers` in `internal/app/wiring.go`.
- Re-run this audit when the service gains a second bounded context, a second
  transport, or a shared adapter that several features consume. This record is
  a snapshot of the dependency graph on 2026-10-21; a later change that
  invalidates it needs a new ADR, not an edit to this one.
- No Redis cache, filtering, pagination, or other optional feature is part of
  this decision.
