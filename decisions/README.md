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
| [0004](0004-startup-errors-and-health-endpoint.md) | Startup errors returned to the process edge; shallow health endpoint | Accepted | `internal/app/`, `cmd/task-manager/`, `internal/infra/server/` |
| [0006](0006-postgres-task-persistence.md) | Task persistence in PostgreSQL: table, versioned migrations, explicit-SQL repository | Accepted | `internal/infra/database/`, `internal/task/repository/postgres/`, `cmd/migrate/` |

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
