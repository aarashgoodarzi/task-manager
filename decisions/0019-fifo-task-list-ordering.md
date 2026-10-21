# 0019. FIFO ordering for GET /tasks

* Status: Accepted
* Date: 2026-10-21
* Scope: `internal/task/**`, `api/openapi.yaml`, README
* Supersedes: [0018](0018-lifo-task-list-ordering.md) — same `created_at`
  keyset, reversed to oldest-first
* Amends: [0016](0016-cursor-pagination-for-task-list.md) — keyset still
  applies on `(created_at, id)`; [0005](0005-task-crud-api.md) — list order
  is FIFO

## Context

ADR 0018 introduced `created_at` and ordered `GET /tasks` LIFO (newest
first). That direction was reversed: the collection should present the oldest
matching task first and walk toward newer rows.

The `created_at` column and composite keyset remain the right mechanism;
only the sort direction and cursor comparison change.

## Decision

### Ordering and SQL

```sql
SELECT id, title, description, status, assignee, created_at
  FROM tasks
  [WHERE status = $n AND/OR assignee = $m
     AND/OR (created_at, id) > ($ts::timestamptz, $id::uuid)]
 ORDER BY created_at ASC, id ASC
 LIMIT $limit;
```

`next_cursor` still encodes the last returned `(created_at, id)` pair.
`created_at` stays persistence-only (not in the HTTP task body).

### Cursor and cache versions

Cursor payload version is **3** (same fields as v2: `after_created_at`,
`after_id`, filters, `page_size`). Version 1 and version 2 cursors are
rejected. Redis list-cache schema version is **5** so LIFO-ordered pages are
not served.

## Alternatives considered

1. **Keep LIFO (0018).** Rejected: product preference is oldest-first.
2. **Drop `created_at` and return to `ORDER BY id`.** Rejected: UUIDv4 order
   is not creation order; the column already exists and is the correct FIFO
   key.

## Trade-offs

- **Gain:** list order is true FIFO by creation time; keyset pagination stays
  skip-safe.
- **Cost:** cursors and cached pages from the brief LIFO window are invalid.

## Consequences

- Collection endpoints that need oldest-first should reuse `(created_at, id)`
  FIFO keysets.
- Changing list order again is an API change and needs a new ADR plus cursor
  and cache version bumps.
