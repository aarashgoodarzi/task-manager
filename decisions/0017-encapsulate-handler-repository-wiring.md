# 0017. Encapsulate repository construction inside handler composition

* Status: Accepted
* Date: 2026-10-21
* Scope: `internal/app/`
* Related: [0011](0011-http-handler-composition.md) — the `buildHandlers`
  composition point this decision tightens; [0002](0002-package-layout-and-composition-root.md)
  — the composition root owns concrete wiring.

## Context

[ADR 0011](0011-http-handler-composition.md) introduced `buildHandlers` so
HTTP modules are assembled in one place and passed to `server.New` as
`RouteRegistrar` values. That removed handler constructors from the
`server.New` argument list, but `Initialize` still constructed the task
repository at the server call site and passed it in:

```go
taskRepo := postgres.New(a.db)
a.server = server.New(cfg, a.log, a.health, a.metrics, a.buildHandlers(taskRepo)...)
```

As more features add repositories, that pattern puts feature-specific adapter
construction next to process lifecycle and server timeouts. The server call
site then accumulates repository variables it does not need to know about.
The composition root should keep wiring explicit without letting
`server.New` become a feature inventory.

## Decision

`buildHandlers` takes no repository arguments. It constructs the adapters its
HTTP modules need from App-owned shared resources (the pool, optional list
cache, logger) and returns the registrars:

```go
func (a *App) buildHandlers() []server.RouteRegistrar {
	tasks := postgres.New(a.db)
	return []server.RouteRegistrar{
		handler.New(service.New(tasks, a.cache, a.log), a.log),
		swagger.New(),
	}
}
```

`Initialize` keeps constructing non-HTTP consumers of persistence (today the
metrics `TaskCountRefresher`) with their own narrow adapters over the same
pool, then calls:

```go
a.server = server.New(cfg, a.log, a.health, a.metrics, a.buildHandlers())
```

Points that are part of the decision:

1. **Server construction does not instantiate feature repositories.** Timeouts,
   health, metrics, and registrars remain its inputs.
2. **HTTP module dependency wiring lives in `buildHandlers`.** Repository and
   service construction for request handlers belong there, not inline beside
   `server.New`.
3. **No registry or service locator.** The list stays an explicit slice; shared
   process resources stay on `App`.
4. **Non-HTTP adapters may still construct their own ports in `Initialize`.**
   The refresher needs `CountTasks`, not the HTTP stack; sharing one
   `*sql.DB` through separate thin repository values is intentional and cheap.

## Alternatives considered

1. **Keep passing `taskRepo` into `buildHandlers`.** Rejected: it leaves
   repository construction at the server call site, which is the problem being
   removed.
2. **Store every repository on `App` and have `buildHandlers` only read fields.**
   Rejected for now: it widens `App` into a dependency bag for values used only
   by one module. Constructing inside `buildHandlers` keeps the HTTP inventory
   local.
3. **A generic repository registry / service locator.** Rejected by ADR 0002
   and ADR 0011: implicit lookup hides dependencies and makes tests harder.

## Trade-offs

- **Gain:** `server.New` stays readable as HTTP process setup; adding a
  feature repository does not edit that call.
- **Gain:** handler dependency graphs are visible in `wiring.go`.
- **Cost:** the metrics refresher and the task HTTP module each construct a
  `postgres.Repository` over the same pool. Both are thin, stateless wrappers;
  they are not competing connection pools.
- **Cost:** `buildHandlers` still grows when a module needs a new shared
  App field; that growth remains intentional and reviewable.

## Consequences

- New HTTP features: construct their driven adapters inside `buildHandlers`
  (or from App fields already owned for process-wide reasons), then append a
  registrar. Do not introduce repository variables solely to feed `server.New`.
- ADR 0011 remains the decision for module registration; this record amends
  only where repository construction for those modules happens.
