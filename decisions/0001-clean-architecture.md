# 0001. Clean Architecture as the service architecture

* Status: Accepted
* Date: 2026-10-19
* Scope: whole repository; enforced by package boundaries under `cmd/` and
  `internal/`; summarized in `README.md`

## Context

Task Manager is a single Go service with one bounded context (tasks) and a
small amount of infrastructure: configuration, a PostgreSQL pool, an HTTP
server, and a health endpoint. The service is expected to grow — task
persistence, request validation, migrations, later observability — without
turning into a set of handlers that mix HTTP, SQL, and business rules.

The forces that matter:

- **Testability without infrastructure.** Service logic must be testable with
  stubs, not a live PostgreSQL server or a listening socket.
- **Replaceable infrastructure.** The HTTP framework and the database driver
  are implementation choices. A change to either should not force changes in
  business rules.
- **Small scope.** The architecture must not cost more than the service it
  describes. Generic layer scaffolding for a single entity is a net loss.
- **Growth without rewrites.** Adding a feature should add files, not reshape
  the dependency graph.

The failure mode to avoid is the common one: HTTP handlers that call the
database directly and accumulate validation, transaction handling, and
response shaping in the same function. Once several features share that shape,
neither the rules nor the transport can be tested or changed independently.

## Decision

Adopt **Clean Architecture** as the architectural style, applied at the scope
the service actually has:

1. **Ports are declared at the point of use.** An interface exists only
   because a real consumer needs it, and it lives in the package of that
   consumer. `HealthChecker` in `internal/infra/server` is the existing
   example: the server needs only `PingContext`, so that is all it declares.
2. **Adapters implement ports or drive the core.** The HTTP server is a
   driving adapter; the PostgreSQL package is a driven adapter. Adapters may
   import the standard library and third-party frameworks, and they point
   inward.
3. **The composition root is the only wiring point.** `internal/app`
   constructs concrete adapters and passes them inward as the ports the core
   declares. No other package assembles config, database, and server.
4. **Business rules live in the core.** For the task feature this means:
   `internal/task/domain` (types and invariants), `internal/task/service`
   (the repository port the use cases will call),
   `internal/task/handler` (HTTP mapping), and `internal/task/repository`
   (explicit SQL). Since ADR 0006 the persistence side exists as `domain`,
   `service.Repository`, and `internal/task/repository/postgres`; the HTTP side
   is added later. The rule applies to every package added later.

The dependency rule, stated as a testable constraint: no package that contains
domain or application logic may import Gin, `net/http`, `database/sql`, or
`pgx`.

### Relationship to layer folders

Clean Architecture is usually drawn as concentric layers — entities, use
cases, interface adapters, frameworks and drivers — with dependencies
pointing inward. This decision keeps that dependency rule but not the layer
folder names: the packages already express the same boundaries more
concretely (`handler → service (port) → repository adapter`). The architecture
is defined by the direction of dependencies, not by a fixed folder taxonomy,
and nothing here requires the layer-folder ceremony that sometimes accompanies
Clean Architecture.

## Alternatives considered

1. **Clean Architecture layer folders** — `entities`, `usecases`,
   `interfaceadapters`, `frameworks`. Rejected: the names are more ceremonial
   than the boundaries they represent for a service with one aggregate, and
   they do not map onto Go's package-per-vertical-slice idiom.
2. **Layered MVC with handlers calling the database directly.** Rejected:
   business rules would couple to Gin and SQL, becoming hard to unit-test and
   hard to reuse outside HTTP. It also makes a second entry point (a worker, a
   CLI) require duplicating logic.
3. **A shared, service-wide infrastructure surface** — a generic `Database`
   port, a dependency bag, versioned handler facades — copied from a larger
   service. Rejected: the service has one aggregate and one transport, so
   these abstractions would be speculative. ADR 0003 rejects the persistence
   subset of this option in detail.
4. **No interfaces at all until a second implementation appears.** Rejected as
   a general rule, but applied locally: the project avoids interfaces that
   have exactly one caller and no testing benefit. Ports are introduced where
   they buy decoupling or a test seam, not by default.

## Trade-offs

- **Gain:** the core can be tested without PostgreSQL or HTTP; adapters can be
  replaced behind ports; the boot path is explicit; new features have an
  obvious location.
- **Cost:** more packages and constructor parameters than a single-file CRUD
  demo, and a reader must follow wiring in `internal/app` to see the whole
  system.
- **Cost:** interfaces at the point of use can look duplicated if two packages
  need the same capability — each declares its own narrow port, which is
  intentional but occasionally verbose.
- **Mitigation:** no interfaces before a consumer exists, no empty placeholder
  packages (ADR 0002), and no framework-like internal layers.

## Consequences

- Handlers and SQL adapters contain no business rules; domain and service
  packages contain no framework or driver imports.
- Task CRUD must follow the vertical-slice shape above, with the repository
  port declared by the service that calls it.
- Tests at the service boundary use stubs; only repository tests need a real
  database.
- Cross-cutting concerns — authentication, metrics, tracing, OpenAPI — are
  implemented as middleware or adapter/bootstrap code, never inside domain
  packages.
- The rule is verifiable by inspecting imports, which makes it reviewable in
  code review rather than aspirational.
