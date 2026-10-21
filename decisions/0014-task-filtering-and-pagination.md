# 0014. Task filtering and pagination on a paginated collection envelope

* Status: Accepted (page-number pagination superseded by
  [0016](0016-cursor-pagination-for-task-list.md))
* Date: 2026-10-21
* Scope: `internal/task/**`, `internal/infra/database/migrations/`,
  `internal/infra/database/schema.go`, `api/openapi.yaml`, README
* Related: [0005](0005-task-crud-api.md) — the task aggregate and error
  contract; [0006](0006-postgres-task-persistence.md) — versioned migrations
  and explicit SQL; [0012](0012-consumer-owned-ports-dependency-audit.md) —
  consumer-owned ports; [0013](0013-redis-cache-aside-for-task-list.md) — the
  list cache this decision extends;
  [0016](0016-cursor-pagination-for-task-list.md) — replaces `page` /
  `total` / `total_pages` with opaque cursor pagination.

## Context

`GET /tasks` returned every row as a bare JSON array. That was adequate while
the table was small, but it made the endpoint progressively more expensive and
gave clients no way to narrow the result. The next increment needed:

- exact filters by workflow status and by owner;
- deterministic, non-overlapping pages;
- a trustworthy total for the filtered set, so a client can render pagination
  without guessing;
- filtering and pagination in PostgreSQL, never by loading the collection and
  slicing it in the process;
- no regressions in the Redis cache introduced in ADR 0013, whose invalidation
  model assumed one cached list.

The existing aggregate had `id`, `title`, `description`, and `status`. There was
no owner field, so filtering by an assignee that could never be set would have
been a fake feature. The schema also had no timestamp, so "newest first" was not
available without adding a column purely for ordering.

## Decision

### Query parameters and defaults

| Parameter | Type | Default | Rules |
| --- | --- | --- | --- |
| `status` | string | any | Exactly one of `todo`, `in_progress`, `done`. |
| `assignee` | string | any | Exact, case-sensitive match; trimmed; 1–100 characters when present. |
| `page` | integer | `1` | One-based. `< 1`, non-numeric, overflow, or an offset that cannot fit in `int64` is rejected. |
| `page_size` | integer | `20` | 1–100. Above the maximum is rejected, not clamped. |

Unknown query parameters are rejected. The handler owns decoding (integers,
parameter presence, unknown names); the use case owns range rules, defaults,
and normalization, and rejects the same values with a `domain.ValidationError`
so another caller cannot bypass them. Explicit `page=0` is a client error,
distinct from omitting the parameter: the handler distinguishes them with
`url.Values.Has`, which is why the use case's zero value can safely mean "use
the default".

Normalization trims `assignee` and maps omitted `page`/`page_size` to their
defaults before the cache is consulted, so semantically identical requests share
one cache entry.

### Response envelope

`GET /tasks` now returns an object, not an array:

```json
{"items": [...], "page": 2, "page_size": 1, "total": 3, "total_pages": 3}
```

This is a deliberate breaking change. A bare array cannot carry `total`,
`total_pages`, or the effective `page_size`, and returning a bare array for some
requests and an envelope for others would be worse. `items` is always an array
(`[]` when empty), never `null`.

Metadata semantics:

- `total` counts every task matching the filters, before pagination.
- `total_pages` is `ceil(total / page_size)`; it is `0` when nothing matches.
- `page` and `page_size` echo the effective values, including defaults.
- A page beyond the last is valid and returns `items: []` with the correct
  totals, rather than a `404` or an invented page.

### Assignee: the smallest model change

`assignee` is a new optional task field, because filtering requires a value the
API can actually set. It is a free-form label, not an account reference: the
service has no authentication or user directory, so pretending it owns identity
would be misleading.

- Domain: `Task.Assignee`, trimmed, at most 100 characters
  (`domain.MaxAssigneeLength`), empty meaning unassigned.
- Storage: nullable `TEXT` column. NULL is the single stored representation of
  unassigned; the adapter maps it to `""` on read and `""` to NULL on write.
  A check constraint mirrors the domain rule:
  `assignee IS NULL OR (btrim(assignee) <> '' AND char_length(assignee) <= 100)`.
- API: `assignee` is writable on create and replace, always present in task
  responses, and defaults to `""`. `PUT` remains a full replacement, so omitting
  it clears it.
- Migration `0003_add_task_assignee.sql` is additive and backward compatible:
  an older binary ignores the column, and existing rows read as unassigned.
  `0004_index_tasks_assignee.sql` adds `tasks_assignee_idx`.

Startup verification now also checks the column. A binary that expects
`assignee` refuses to start against a database where `0003` was never applied,
instead of accepting traffic and failing every list request with a `500`.

### SQL: filtering before pagination

The service port moved from `List(ctx)` to:

```go
ListPage(ctx context.Context, filter Filter) (TaskPage, error)
```

`Filter` carries the typed status, the trimmed assignee, `Limit int`, and
`Offset int64`. `TaskPage` carries the page's tasks and the filtered total. The
adapter runs two parameterized queries through one shared WHERE builder:

```sql
SELECT count(*) FROM tasks [WHERE ...];
SELECT id, title, description, status, assignee
  FROM tasks [WHERE ...]
  ORDER BY id
  LIMIT $n OFFSET $n+1;
```

Both queries share `filterClause(filter)`, so the count and the page cannot
disagree about filter semantics. Every value, including `LIMIT` and `OFFSET`, is
a bound parameter; nothing is concatenated from input. The offset is computed in
the use case as `(page-1) * page_size` in `int64` after a division-based overflow
check, so a huge page number is rejected rather than wrapping.

`ORDER BY id` is deterministic and unique, so pages cannot overlap for a fixed
data set. `id` is a UUID, so this is *not* creation order; adding a timestamp
merely to sort would have changed the schema for presentation, and the
identifier is already the existing contract's stable key.

Indexes: `tasks_status_idx` already existed on `status`. `tasks_assignee_idx` on
`assignee` is the only new index, matching the existing single-column style and
serving both the equality filter and the count. A composite `(status, id)` was
not added because `0002` already indexes `status`; duplicating it to save a sort
on a small table is not justified. The ADR records that this is a deliberate
scale assumption.

### Cache: one entry per normalized query

The cache key gained a query variant component:

```text
task-manager:tasks:list:<generation>:<query-fingerprint>
task-manager:tasks:list:generation
```

`<query-fingerprint>` is a truncated SHA-256 of a canonical, length-safe
encoding of the normalized status, assignee, `page`, and `page_size`. Hashing
avoids delimiter injection through an assignee value and keeps key length
bounded; because the service normalizes before calling the cache, equivalent
requests map to the same fingerprint.

Invalidation is unchanged and still correct: a mutation advances the single
generation token, which makes *every* variant unreachable at once. The old
variants expire with `REDIS_CACHE_TTL` and are never addressed again. There is
no variant index to maintain, no per-variant delete, and the stale-refill
protection from ADR 0013 (a `Set` is bound to the token observed by its `Get`)
applies per variant unchanged.

The cached payload (`version: 2`) now stores the full page: tasks, page,
page_size, total, and total_pages. Bumping the version makes version-1 entries a
miss instead of a silently incomplete response. Redis failure behavior is
unchanged: reads and writes fall back to PostgreSQL, and a failed invalidation
degrades reads until the recovery probe or the next mutation.

### Validation behavior

Query-string failures use the existing envelope with
`code: validation_error` and `message: invalid query parameters`; `fields`
names the offending parameter:

```json
{"error":{"code":"validation_error","message":"invalid query parameters","fields":{"page_size":"must be at most 100"}}}
```

No query error changes the shape of the task or error schemas, and no invalid
value is silently dropped or clamped.

## Alternatives considered

1. **Keep the bare array and put totals in headers.** Rejected: pagination
   metadata is part of the resource representation, headers are invisible to
   many clients and to the OpenAPI schema, and a page's `total` would be harder
   to keep consistent with its `items`. The envelope is the explicit, documented
   change the requirement allows.
2. **Return an array for unfiltered requests and an envelope otherwise.**
   Rejected: two response shapes for one operation make every client branch and
   make the contract impossible to describe honestly.
3. **Clamp `page_size` above the maximum instead of rejecting it.** Rejected:
   the client would receive a different page than it asked for, and silent
   clamping hides a contract violation. The requirement is to reject with the
   error envelope.
4. **Load all matching tasks and paginate in Go.** Rejected: it defeats the
   point, grows with the table, and would be a latency and memory incident
   waiting to happen. Filtering and pagination belong in SQL.
5. **Use a window function (`count(*) OVER ()`) for the total.** Rejected for
   this version: it returns no row for an empty page, so a page beyond the last
   would lose its `total`. Two queries with one shared WHERE builder are simpler
   and stay correct for empty pages.
6. **Order by a new `created_at` column, `id` as tie-breaker.** Rejected: it
   adds a column and migration solely for presentation, and the existing
   contract already orders by the unique identifier. If creation order becomes a
   product requirement, it needs its own decision and migration.
7. **Delete every cached variant on a mutation.** Rejected: the adapter would
   have to enumerate keys (SCAN) or maintain a variant index, both of which are
   more failure modes than advancing one generation token already solves.
8. **Include filter values in the cache key verbatim.** Rejected: an assignee
   can contain the delimiter, so two different queries could produce one key.
   A hash of a canonical encoding avoids that class of bug.
9. **Add an `assignee` table and referential integrity.** Rejected: there is no
   account system to reference. A free-form label is the honest model at this
   scope; a future identity feature would add its own table and migration.

## Trade-offs

- **Gain:** clients can filter and paginate, and `total`/`total_pages` are exact
  for the filtered set.
- **Gain:** every query shape gets cache hits without collisions, and one
  mutation still invalidates all of them atomically.
- **Gain:** deterministic pages with a unique tie-breaker, validated at both the
  HTTP and use-case boundary.
- **Cost:** `GET /tasks` is a breaking response change from `[]` to the
  envelope. Clients must read `items`.
- **Cost:** a cache miss runs two queries (count and page). The count scans the
  filtered rows, which the indexes narrow but do not eliminate.
- **Cost:** each distinct filter/page combination is a separate Redis entry
  until its TTL expires. A client walking many pages creates many small keys.
- **Cost:** `assignee` widens every task row and every response, and requires
  `0003`/`0004` to be applied before the new binary starts.
- **Limitation:** ordering is by UUID, not creation or update time. There is no
  sort parameter, no free-text search, and no filter for unassigned tasks.
- **Limitation:** `assignee` is case-sensitive and unindexed for prefix search;
  `alice` and `Alice` are different labels.
- **Limitation:** the count and page queries are two statements, not one
  snapshot. A concurrent write between them can make `total` differ by one from
  the page contents. The result is still a valid page of a valid total; strict
  cross-statement snapshot consistency would need a repeatable-read transaction.

## Consequences

- `GET /tasks` is the only paginated endpoint. New collection endpoints should
  reuse the envelope and the same validation vocabulary rather than inventing
  a second format.
- New filters must be added to `service.ListQuery`, `service.Filter`, the shared
  `filterClause`, the handler's closed parameter set, the OpenAPI document, and
  the cache fingerprint input. Forgetting the fingerprint would let two queries
  collide; the adapter test that distinct queries produce distinct keys is the
  guard.
- Any new mutation that changes the task list must keep invalidating the single
  generation token; per-variant deletion is explicitly not the design.
- `internal/infra/database/schema.go` must list columns the binary reads, so a
  partially migrated database fails at startup rather than at request time.
- OpenAPI documents the parameters, defaults, bounds, envelope, and the query
  validation example; the embedded Swagger UI serves it unchanged.
- Tests that must keep passing: handler query validation and envelope metadata,
  service normalization/pagination/cache-query propagation, repository
  filtering/pagination/count/parameter-binding against real PostgreSQL, and
  Redis variant isolation and whole-family invalidation against real Redis.
