# Architecture Decision Records

This directory records the architectural decisions that shape the Task
Manager service. Each record explains why the codebase is built the way it
is, not merely how it is organized. Records are immutable history once
accepted: a later decision that replaces an earlier one is captured in a new
file and the older record is marked `Superseded by NNNN`.

## Index

| ADR | Title | Status | Scope |
| --- | --- | --- | --- |
| [0001](0001-clean-architecture.md) | Clean Architecture as the service architecture | Accepted | Whole repository; package boundaries under `cmd/` and `internal/` |
| [0002](0002-package-layout-and-composition-root.md) | Feature-oriented package layout and a single composition root | Accepted | `cmd/task-manager/`, `internal/app/`, `internal/infra/*` |
| [0003](0003-postgres-via-database-sql.md) | PostgreSQL through `database/sql` and pgx, without an ORM | Accepted (migration strategy amended by 0006) | `internal/infra/database/`, `internal/task/repository/` |
| [0004](0004-startup-errors-and-health-endpoint.md) | Startup errors returned to the process edge; shallow health endpoint | Accepted (database requirement amended by 0005) | `internal/app/`, `cmd/task-manager/`, `internal/infra/server/` |
| [0005](0005-task-crud-api.md) | Task CRUD on a service-owned repository port | Accepted | `internal/task/**`, `internal/infra/handler/`, `internal/infra/server/`, `internal/app/` |
| [0006](0006-postgres-task-persistence.md) | Task persistence in PostgreSQL: table, versioned migrations, explicit-SQL repository | Accepted | `internal/infra/database/`, `internal/task/repository/postgres/`, `cmd/migrate/` |
| [0007](0007-ci-with-postgresql-service.md) | CI runs the suite against a real PostgreSQL service and refuses silent skips | Accepted (coverage gate and image checks added by 0008) | `.github/workflows/ci.yml`, `Makefile`, README testing docs |
| [0008](0008-containers-and-coverage-gate.md) | Containerized local environment and a 70% coverage gate on the combined suite | Accepted | `Dockerfile`, `.dockerignore`, `compose.yaml`, `Makefile`, CI, README |
| [0009](0009-openapi-contract.md) | A checked-in OpenAPI document as the HTTP contract | Accepted | `api/`, `internal/infra/swagger/`, `internal/infra/server/`, `internal/app/` |
| [0010](0010-prometheus-metrics-and-request-correlation.md) | Prometheus metrics and request correlation for HTTP observability | Accepted | `internal/infra/metrics/`, `internal/infra/server/`, `internal/task/repository/postgres/`, `internal/infra/config/`, `internal/app/` |
| [0011](0011-http-handler-composition.md) | One composition point for HTTP module handlers | Accepted | `internal/app/`, `internal/infra/server/`, `internal/task/handler/`, `internal/infra/swagger/` |
| [0012](0012-consumer-owned-ports-dependency-audit.md) | Consumer-owned ports remain the dependency-inversion boundary | Accepted | Whole repository; audited `internal/task/**`, `internal/infra/**`, `internal/app/`, `cmd/` |
| [0013](0013-redis-cache-aside-for-task-list.md) | Redis cache-aside for task listing with generation-token invalidation | Accepted | `internal/task/service/`, `internal/task/cache/redis/`, `internal/infra/config/`, `internal/app/`, `compose.yaml`, CI, README |
| [0014](0014-task-filtering-and-pagination.md) | Task filtering and pagination on a paginated collection envelope | Accepted | `internal/task/**`, migrations, schema verification, `api/openapi.yaml`, README |

## When to write one

Add a record when a choice:

- constrains how future code must be written (dependency direction, package
  boundaries, error-handling policy);
- is expensive to reverse (database driver, transport, persistence approach);
- rejects an obvious alternative for a reason that will not be obvious later.

Do not add a record for:

- features that are not designed or implemented yet — a record describes a
  decision that is already reflected in the code;
- details that the code states unambiguously on its own (function names,
  formatting, local refactors).

Update an existing record instead of creating a near-duplicate. If a decision
changes, write a new record and mark the old one superseded.

## Format

Every record uses the same structure:

```markdown
# NNNN. Title describing the decision

* Status: Accepted | Superseded by NNNN | Deprecated
* Date: YYYY-MM-DD
* Scope: paths / packages affected

## Context

The forces at play: constraints, requirements, and the problem to solve.

## Decision

What was chosen, stated precisely enough to verify against the code.

## Alternatives considered

Each rejected option with the reason it was rejected.

## Trade-offs

What the decision buys and what it costs.

## Consequences

What follows for future work: rules, obligations, and open follow-ups.
```

Guidelines:

- Titles describe the decision, not the folder it affects.
- Reference real paths and package names; documentation must be verifiable
  against the tree.
- Keep the reasoning technical. Historical context is welcome when it explains
  a constraint, but it should not read as a narrative about people or
  processes.
- Distinguish the current state from future possibilities explicitly. A
  consequence that begins with "when X is implemented" is a plan, not a
  capability.
