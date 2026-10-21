# 0018. LIFO ordering for GET /tasks

* Status: Superseded by [0019](0019-fifo-task-list-ordering.md)
* Date: 2026-10-21
* Scope: `internal/task/**`, `internal/infra/database/migrations/`,
  `api/openapi.yaml`, README
* Amends: [0016](0016-cursor-pagination-for-task-list.md) — keyset still applies,
  but the sort key is `(created_at, id)` descending instead of `id` ascending;
  [0005](0005-task-crud-api.md) — list order is LIFO, not ascending `id`;
  [0006](0006-postgres-task-persistence.md) / [0014](0014-task-filtering-and-pagination.md)
  — `created_at` is added for ordering (still omitted from the HTTP task body).

## Context

`GET /tasks` listed rows with `ORDER BY id` ascending. Identifiers are random
version-4 UUIDs, so that order was stable but not creation order. The product
requirement is LIFO: the most recently created matching task must appear first,
and cursor pages must continue toward older rows without the classic offset
skip/duplicate problem.

## Decision

### Schema

Migrations `0005_add_task_created_at.sql` and
`0006_index_tasks_created_at.sql` add:

```sql
ALTER TABLE tasks
    ADD COLUMN IF NOT EXISTS created_at TIMESTAMPTZ NOT NULL DEFAULT now();

CREATE INDEX IF NOT EXISTS tasks_created_at_id_idx
    ON tasks (created_at DESC, id DESC);
```

`created_at` is set by PostgreSQL on insert and never updated. It is scanned
into `domain.Task.CreatedAt` for keyset encoding; create/update HTTP bodies and
JSON responses do not expose it.

### Ordering and SQL

```sql
SELECT id, title, description, status, assignee, created_at
  FROM tasks
  [WHERE status = $n AND/OR assignee = $m
     AND/OR (created_at, id) < ($ts::timestamptz, $id::uuid)]
 ORDER BY created_at DESC, id DESC
 LIMIT $limit;
```

The use case still requests `page_size + 1` rows. When another page exists,
`next_cursor` encodes the last returned `(created_at, id)` pair.

### Cursor

Cursor payload version is **2**. It includes `after_created_at` (RFC3339Nano)
and `after_id`, plus the bound filters and `page_size`. Version 1 (id-only
ascending) cursors are rejected as malformed rather than reinterpreted.
Redis list-cache schema version is **4** so ascending-id pages are not served
after the order change.

## Alternatives considered

1. **`ORDER BY id DESC` only.** Rejected: UUIDv4 order is not insertion order,
   so the list would claim LIFO without delivering it.
2. **UUIDv7 identifiers.** Rejected for this change: it would rewrite identity
   generation and invalidate existing rows; a dedicated timestamp column is
   smaller and explicit.
3. **Expose `created_at` on the HTTP task resource.** Deferred: ordering needs
   the column; the public create/update/get contract does not.

## Trade-offs

- **Gain:** list order matches “newest first”; keyset pagination stays
  skip-safe under concurrent deletes of already-returned rows.
- **Cost:** one column and index; cursors from before this change are invalid;
  cached list pages from schema version 3 are treated as misses.

## Consequences

- New collection endpoints that need “newest first” should reuse
  `(created_at, id)` LIFO keysets rather than inventing a second order.
- Changing list order again is an API change and needs a new ADR plus a cursor
  version bump.
