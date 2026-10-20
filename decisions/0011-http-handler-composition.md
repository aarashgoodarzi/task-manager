# 0011. One composition point for HTTP module handlers

* Status: Accepted
* Date: 2026-10-21
* Scope: `internal/app/`, `internal/infra/server/`,
  `internal/task/handler/`, `internal/infra/swagger/`
* Related: [0002](0002-package-layout-and-composition-root.md) — a single
  composition root owns concrete wiring; [0005](0005-task-crud-api.md) — the
  task HTTP adapter and its routes; [0009](0009-openapi-contract.md) — the docs
  adapter is registered like a feature handler.

## Context

`internal/infra/server` already declares a narrow `RouteRegistrar` port and
accepts modules as a variadic argument. `server.New` builds the router,
installs the request-ID and completion middleware, mounts `/health`, `/metrics`,
and the `404`/`405` handlers, and then lets each registrar mount its own routes.
The infrastructure side of module registration was therefore solved.

The composition side was not. `internal/app.Initialize` constructed the task
service, the task handler, and the docs adapter inline and passed both handlers
directly in the `server.New` call:

```go
taskRepo := postgres.New(a.db)
taskService := service.New(taskRepo)
taskHandler := handler.New(taskService, a.log)
swaggerHandler := swagger.New()

a.server = server.New(server.Config{ /* timeouts and address */ },
	a.log, a.health, a.metrics, taskHandler, swaggerHandler)
```

Two modules make that readable, but the extension point is unnamed: every new
HTTP module adds another constructor in lifecycle code and another argument to
one call that also sequences the pool, metrics, and refresher. The call site
mixes "which modules exist" with "in what order the process starts", and the
only way to observe which modules are mounted is to run `Initialize`, which
requires PostgreSQL.

The constraints come from the existing decisions:

- The dependency rule from [ADR 0001](0001-clean-architecture.md) still holds:
  the server may not import feature handlers, and domain/service code may not
  import Gin.
- [ADR 0002](0002-package-layout-and-composition-root.md) keeps all concrete
  wiring in `internal/app` and explicitly rejects a shared dependency bag, so
  the composition point must receive only the dependencies its modules need.
- The service has one transport, one bounded context, and a strict shutdown
  order; it does not need a module framework.

## Decision

`internal/app/wiring.go` declares a single composition function:

```go
func (a *App) buildHandlers(tasks service.Repository) []server.RouteRegistrar {
	return []server.RouteRegistrar{
		handler.New(service.New(tasks), a.log),
		swagger.New(),
	}
}
```

`Initialize` constructs the shared adapters — pool, task repository, metrics
registry, and task-count refresher — and hands the result of `buildHandlers` to
the server:

```go
a.server = server.New(cfg, a.log, a.health, a.metrics, a.buildHandlers(taskRepo)...)
```

The following points are part of the decision, not incidental:

1. **`server.New` is unchanged.** It still accepts `...RouteRegistrar`, because
   the server's job is mounting routes between its middleware and its
   `404`/`405` handlers, not assembling modules. The change adds the missing
   composition step rather than a second server abstraction.
2. **`buildHandlers` owns module construction, not module mechanics.** It calls
   the same constructors `Initialize` used to call and returns the modules as a
   slice literal. There is no registry, no `init()` self-registration, and no
   reflection; the list is readable in one place and checked by the compiler.
3. **Only shared dependencies are arguments.** `tasks` is the repository port
   (`service.Repository`) because the task module consumes it; the logger comes
   from the `App`, which already owns it. Nothing module-specific is passed to
   the server.
4. **Modules keep their existing contract.** A module contributes routes by
   implementing `RegisterRoutes(gin.IRouter)`. Route paths, request decoding,
   response mapping, and error mapping stay in the feature slice; the server
   keeps router construction, middleware, health, metrics mounting, and HTTP
   lifecycle.
5. **The scope is HTTP handler composition only.** `buildHandlers` creates no
   goroutines, owns no lifecycle, and returns nothing with a `Start`/`Stop`
   method. Background work (the task-count refresher), pool shutdown, and
   graceful HTTP shutdown remain ordered explicitly in `App.Initialize` and
   `App.Start`.

`internal/app/wiring_test.go` registers the returned modules on a bare Gin
router and asserts that all task routes and the Swagger route are contributed.
The test needs no database, because it exercises composition rather than
request handling.

## Alternatives considered

1. **Keep passing each handler directly to `server.New`.** This is the state
   before the change and is the smallest possible diff. It was rejected because
   the call site is lifecycle code: every future module edits the same statement
   that also sequences the pool, metrics, and refresher, and the set of mounted
   modules is only discoverable by reading the argument list of a constructor
   call. The cost grows with each module, while the fix is one named function.
2. **A `Handlers` facade struct in `internal/infra/handler`.** A façade with a
   single `RegisterRoutes` method would also mirror the reference gateway's
   handler-set shape, and it would make the server take one argument instead of
   a slice. It was rejected as unnecessary for this service: there is one
   transport and one flat route namespace, so a façade would only re-loop over
   the same `RouteRegistrar` list the server already loops over, and it would
   move `RouteRegistrar` out of the package that declares its consumer
   (ADR 0002, rule 5). A façade earns its place when there are API versions or
   authorization groups to mount differently; this service has neither.
3. **A generic module or plugin framework.** A name-keyed registry, `init()`
   self-registration, or reflection-driven discovery was rejected: control flow
   becomes implicit, registration order depends on package initialization, a
   forgotten blank import silently removes a feature, and tests can no longer
   assert the mounted set by reading a list. Explicit wiring also keeps the
   "wiring changes are visible in one diff" property from ADR 0002.
4. **A dependency bag passed to `buildHandlers`.** Rejected by ADR 0002: a
   struct of every adapter hides which dependency a module actually uses and
   makes every module depend on the whole infrastructure surface. The current
   parameter is the one port the task module needs.
5. **A uniform module lifecycle interface (`Start`/`Stop` per module).**
   Rejected for this change: the process has exactly one HTTP module, one
   background refresher, and one pool, and their ordering is deliberate — the
   refresher is cancelled and awaited before the pool closes
   ([ADR 0010](0010-prometheus-metrics-and-request-correlation.md)). Wrapping
   them in a uniform lifecycle would hide that order to serve a second module
   that does not exist. This is a scope boundary, not a claim that lifecycle
   composition will never be needed.

## Trade-offs

- **Gain:** adding an HTTP module touches one file and one list; `Initialize`
  keeps reading as startup sequencing rather than a feature inventory.
- **Gain:** composition is testable without PostgreSQL, and module construction
  order is separate from process lifecycle order.
- **Gain:** no new dependency, interface, package, or reflection; the server
  contract and its existing tests are untouched.
- **Cost:** `buildHandlers` still grows by one line per module, and its
  signature grows when a module needs a new shared dependency. That growth is
  intentional and visible; the alternative is to hide it in a dependency bag.
- **Cost:** composition is compile-time and manual. The compiler cannot force a
  new handler to be added to the list, so a module can exist without being
  mounted. The route test is the guard, not the type system.
- **Cost:** the server does not skip nil registrars, so `buildHandlers` must
  return only non-nil values; the test states that obligation explicitly.

## Consequences

- **Adding a module:** implement `RegisterRoutes(gin.IRouter)` in the feature's
  handler package, then add its constructor to `buildHandlers` in
  `internal/app/wiring.go`. If it needs a new shared adapter, construct that
  adapter in `Initialize` and pass it as a `buildHandlers` parameter or an
  `App` field — never as a new `server.New` argument.
- **Server changes:** `internal/infra/server` must not learn about feature
  packages. A change that requires importing one is a signal that the module's
  HTTP concern belongs in the module.
- **Route conflicts:** the slice is mounted in order, and Gin panics on a
  duplicate `method + path` registration, so an ambiguous composition fails at
  startup instead of silently overriding a route.
- **Background work:** `buildHandlers` remains free of goroutines and
  lifecycle. A module that needs startup or shutdown work must extend
  `App.Start`'s ordering explicitly; composing such work generically requires a
  new ADR, not an extension of this function.
- **Documentation:** the README's wiring steps and "Rules for new code" name
  `buildHandlers` as the place a new HTTP module is registered, so the
  documented procedure matches the code.
