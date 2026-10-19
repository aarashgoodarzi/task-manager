# 0002. Feature-oriented package layout and a single composition root

* Status: Accepted
* Date: 2026-10-19
* Scope: `cmd/task-manager/`, `internal/app/`,
  `internal/infra/{config,database,server}/`

## Context

The service has one bounded context (tasks), one transport (HTTP), and one
datastore (PostgreSQL), but its boot path already involves four concerns:
configuration, logging, a connection pool, and an HTTP server. Two problems
have to be solved before the first feature lands:

- **Where does wiring live?** If `main` grows into the assembly point, the
  entry point accumulates policy (what to do on failure, shutdown ordering)
  and becomes hard to test or extend.
- **Where does code go?** A layout that is too flat couples features
  together; a layout with empty speculative directories documents intentions
  that do not exist and invites code into the wrong place.

There is also a concrete boundary requirement from ADR 0001: the HTTP server
must not import the database adapter, even though the two are wired together
at startup. Without an explicit rule, that import is the natural thing to add
when writing the health check.

## Decision

Use a small top-level layout split by **role**, with **features** as vertical
slices beneath it:

```
cmd/task-manager/          process entry point only
internal/app/              composition root (wire config → db → server)
internal/infra/config/     environment loading
internal/infra/database/   PostgreSQL connection pool adapter
internal/infra/server/     Gin router, middleware, HTTP lifecycle
```

Rules:

1. **`main` constructs `app.App` and nothing else.** It calls `Initialize`
   and `Start`, and handles the returned errors by logging and exiting. It
   knows no concrete adapter.
2. **`internal/app` is the only package that imports config, database, and
   server together.** It owns construction order and shutdown order, and it
   passes concrete adapters inward as the ports their consumers declare.
3. **`internal/infra/*` holds adapters shared by the whole process.**
   Feature-specific adapters belong in the feature slice (for example
   `internal/task/repository`), not in `internal/infra`.
4. **Do not create `internal/task/**` until the task feature is implemented.**
   Empty directories and comment-only placeholder files are not added to keep
   a shape in Git.
5. **Adapters define the ports they consume.** `internal/infra/server` declares
   `HealthChecker`; it does not import `internal/infra/database`.
6. **Configuration stays a plain data source.** `internal/infra/config`
   imports only the standard library and produces values; it does not build
   clients or servers.

The layout is deliberately role-first at the top level (`app`, `infra`) and
feature-first below it. A service-wide adapter is not a feature; task-specific
code is not infrastructure.

## Alternatives considered

1. **Create empty `internal/task/{domain,service,handler,repository}` now.**
   Rejected: it invents structure before the design exists, and empty
   directories either disappear from Git or require placeholder files with no
   purpose. It also encourages writing persistence before the domain model.
2. **Put wiring inside `main`.**
   Rejected: `main` would hold construction order, error policy, and shutdown
   sequencing. Keeping those in `internal/app` makes them testable in
   isolation and keeps the entry point to a few lines.
3. **A dependency bag (`internal/infra/deps`) passed everywhere.** Rejected:
   it makes every consumer depend on the whole infrastructure surface,
   defeating the narrow ports from ADR 0001 and hiding which dependency a
   package actually uses.
4. **A single flat `internal` package per layer** (`handlers`, `services`,
   `repositories`, `models`). Rejected: it scales by adding layer folders and
   cross-imports rather than by adding features, and it separates code that
   changes together.
5. **Put the health check in `internal/infra/database`.** Rejected: the
   endpoint is an HTTP concern. The server declares the one method it needs,
   which keeps the dependency pointing inward and lets tests inject a stub.

## Trade-offs

- **Gain:** the boot path is short and explicit; package responsibilities are
  obvious from the path; features can be added without touching unrelated
  packages; the server/database boundary is enforced by the interface, not by
  convention.
- **Cost:** the tree looks incomplete compared with the finished service,
  because the feature slice is intentionally absent until it is written.
- **Cost:** every new top-level dependency requires editing `internal/app`,
  which is the intended friction: wiring changes are visible in one diff.
- **Mitigation:** ADR 0001 and the README describe the intended slice, so the
  missing directories are a documented state rather than an oversight.

## Consequences

- Adding task CRUD means introducing `internal/task/...` and extending the
  `internal/app` wiring — not putting SQL in `server` or business rules in
  handlers.
- `internal/infra/server` accepts a `HealthChecker`, so its tests use a stub
  and need no database.
- `internal/infra/config` remains importable by any package without pulling in
  Gin or pgx.
- Shutdown order lives in exactly one place: the pool is closed after the HTTP
  server stops accepting requests.
- Future shared adapters (for example a metrics registry or a cache client)
  are added under `internal/infra/` and wired in `internal/app`; feature-local
  code stays in its slice.
