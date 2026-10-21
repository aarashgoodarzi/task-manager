# 0016. Cursor (keyset) pagination for GET /tasks

* Status: Accepted
* Date: 2026-10-21
* Scope: `internal/task/**`, `api/openapi.yaml`, `cmd/loadtest/`, README
* Related: [0014](0014-task-filtering-and-pagination.md) — filtering and the
  original page-number envelope (pagination superseded here);
  [0013](0013-redis-cache-aside-for-task-list.md) — generation-token list
  cache; [0006](0006-postgres-task-persistence.md) — explicit SQL.

## Context

**Cursor-based pagination was not necessary to implement for this project;
page-number pagination is the better fit here. It is implemented mainly to
show off. :)**

ADR 0014 introduced `page` / `page_size` offset pagination with `total` and
`total_pages`. Offset pagination is simple, but concurrent inserts or deletes
between requests shift later pages: a client walking with `OFFSET` can skip or
repeat rows solely because earlier rows appeared or disappeared. The API needed
a pagination contract that avoids that class of bug without pretending that
independent HTTP requests share a database snapshot.

The existing ordering key remained suitable: tasks are already ordered by the
immutable UUID primary key (`ORDER BY id`). That key is unique, so it can
anchor a keyset without a composite sort key. UUID order is still not creation
order; that limitation from ADR 0014 is unchanged.

## Decision

### Ordering and SQL

`GET /tasks` uses keyset pagination on `id` ascending:

```sql
SELECT id, title, description, status, assignee
  FROM tasks
  [WHERE status = $n AND/OR assignee = $m AND/OR id > $k::uuid]
 ORDER BY id
 LIMIT $limit;
```

The use case requests `page_size + 1` rows. If more than `page_size` rows
return, `has_more` is true, the extra row is discarded, and `next_cursor`
encodes the last returned id. There is no `COUNT(*)` on the list path: an
exact total is no longer part of the contract, and a count would reintroduce
extra work and a second statement that can disagree with the page under
concurrency.

Filtering still happens in SQL before the cursor bound and limit. The shared
`filterClause` builder adds the optional `id > $n::uuid` predicate alongside
status and assignee.

### Opaque cursor

`next_cursor` is an opaque Base64 URL-safe encoding of a versioned JSON
payload that includes:

- `after_id` — exclusive lower bound for the next page;
- `status`, `assignee`, `page_size` — the canonical query that issued the
  cursor.

Decoding is strict (`DisallowUnknownFields`, single JSON object, version
check, UUID validation). A malformed cursor is a `400` with
`fields.cursor = must be a valid cursor` and is never treated as the first
page. A cursor reused with a different status, assignee, or page size is a
`400` with `fields.cursor = is incompatible with the current query`.

Base64 is encoding for opacity, not encryption or authentication.

### Response envelope

```json
{"items":[...],"next_cursor":"...-or-null","has_more":true,"page_size":20}
```

| Field | Semantics |
| --- | --- |
| `items` | Always an array, never `null`. |
| `next_cursor` | Opaque token or `null` when `has_more` is false. |
| `has_more` | Whether another page exists after this one. |
| `page_size` | Effective page size, including the default of 20. |

Query parameters: `status`, `assignee`, `cursor`, `page_size` (default 20,
max 100). The `page` parameter is removed and rejected as unsupported.

### Cache

The Redis variant fingerprint now hashes
`status`, `assignee`, `cursor`, and `page_size`. Cached payload schema version
is **3** (`page_size`, `next_cursor`, `has_more` instead of
`page`/`total`/`total_pages`). Generation-token invalidation, stale-refill
protection, and fail-open Redis behavior are unchanged.

### Consistency guarantees

What the implementation provides:

- For an unchanged data set, walking with `next_cursor` yields each matching
  row exactly once in `id` order.
- Inserts/deletes before the cursor cannot shift later pages the way `OFFSET`
  does: continuation is `id > after_id`, not "skip N rows".

What it does not provide:

- A multi-request snapshot. Rows inserted after the cursor may appear on a
  later page; rows deleted after they were returned simply do not appear
  again; rows that change status/assignee may enter or leave the filtered
  walk between requests.
- Strict repeatable-read across pages. Holding a long-lived transaction or
  export snapshot across HTTP requests would be a different product decision
  with connection and operational cost this service does not take on.

## Alternatives considered

1. **Keep offset pagination.** Rejected: it is the source of the skip/duplicate
   problem the requirement asks to remove.
2. **Timestamp-only cursors.** Rejected: there is still no `created_at` column,
   and a non-unique timestamp alone cannot define a total order.
3. **Signed or encrypted cursors.** Rejected: opacity plus strict validation is
   enough; cryptography would imply authenticity guarantees the API does not
   need.
4. **Unbound cursors (id only, no filter binding).** Rejected: silently
   continuing under a different filter would change pagination semantics
   without a clear client error.
5. **Retain `total` via `COUNT(*)`.** Rejected for this version: the cursor
   contract does not need it, and the count is an extra scan on every miss.

## Trade-offs

- **Gain:** no offset-shift skips/duplicates; no per-page count query; clear
  end-of-list via `has_more` / `next_cursor: null`.
- **Gain:** filter/page-size binding prevents silent cross-query continuation.
- **Cost:** breaking API change — clients must stop using `page`, `total`, and
  `total_pages`, and must follow `next_cursor`.
- **Cost:** clients that rendered "page N of M" lose an exact total unless they
  count locally or add a separate aggregate later.
- **Limitation:** concurrent mutations can still change membership of later
  pages; document that honestly rather than claiming snapshot isolation.

## Consequences

- ADR 0014's filtering, assignee model, and closed query-parameter set remain;
  only its page-number pagination and total metadata are superseded.
- New filters must still be added to `ListQuery`, `Filter`, `filterClause`, the
  handler whitelist, OpenAPI, the cursor payload, and the cache fingerprint.
- Load-test dataset sizing walks cursors instead of reading `total`.
- OpenAPI, README, and Swagger UI must not advertise `page` or totals, and
  must not declare misleading OpenAPI `default` values for Try it out.
