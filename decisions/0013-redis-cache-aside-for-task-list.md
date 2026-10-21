# 0013. Redis cache-aside for task listing with generation-token invalidation

* Status: Accepted
* Date: 2026-10-21
* Scope: `internal/task/service/`, `internal/task/cache/redis/`,
  `internal/infra/config/`, `internal/app/`, `compose.yaml`, CI, README
* Related: [0001](0001-clean-architecture.md) — the cache port follows the
  consumer-owned interface rule; [0002](0002-package-layout-and-composition-root.md) —
  the composition root constructs the adapter; [0003](0003-postgres-via-database-sql.md) —
  PostgreSQL remains the source of truth; [0012](0012-consumer-owned-ports-dependency-audit.md) —
  ports live with their consumers; [0014](0014-task-filtering-and-pagination.md) —
  extends the list key with a per-query variant fingerprint under the same
  generation token.

## Context

`GET /tasks` is the only read that returns a collection, and it is the most
expensive routine query: every call scans and sorts the `tasks` table. The
service is small and PostgreSQL must remain authoritative, so the goal is to
remove repeated list queries without introducing a second source of truth or
turning Redis into a startup dependency.

The requirements that shape the design are:

- **PostgreSQL is authoritative.** Every mutation commits there first. A
  cache entry is disposable and may be lost at any time.
- **The API stays usable without Redis.** Redis outages must not fail reads or
  writes. A cache miss, failed read, failed write, or failed invalidation falls
  back to PostgreSQL.
- **Invalidation happens after a successful mutation, never before.** A failed
  mutation must not evict a valid list. A successful mutation must not be
  reported as failed because Redis is unavailable.
- **A slow reader must not repopulate stale data after an invalidation.** The
  classic cache-aside race is: GET misses and reads an old list; a concurrent
  PUT/DELETE commits and invalidates; the GET then writes its old result into
  the cache. A delete-on-write design permits that stale result to become the
  next answer.
- **The architecture from ADR 0001/0012 holds.** The cache contract is declared
  by the application service, the Redis adapter implements it, and only
  `internal/app` connects the concrete adapter.

## Decision

### Cache-aside at the use-case boundary

`internal/task/service` declares the port it consumes:

```go
type ListCache interface {
	Get(ctx context.Context, query ListQuery) (ListSnapshot, error)
	Set(ctx context.Context, query ListQuery, snapshot ListSnapshot) error
	Invalidate(ctx context.Context) error
}
```

`ListSnapshot` carries the cached page (tasks plus pagination metadata), the
generation token observed by the read, and whether it was a hit. The token is
opaque to the service; it exists only to bind a later `Set` to the generation
the read actually saw. The `ListQuery` argument selects which query variant is
read or written ([ADR 0014](0014-task-filtering-and-pagination.md)).

`internal/task/cache/redis` implements the port with `github.com/redis/go-redis/v9`.
It is a driven adapter in the feature slice, not a generic cache framework, and
it imports the service package (the port owner) rather than the reverse. The
composition root constructs it and passes it to `service.New`; a nil cache
disables the feature without changing use-case behavior.

### Key layout and TTL

| Key | Contents | TTL |
| --- | --- | --- |
| `task-manager:tasks:list:generation` | Current generation token (hex) | None |
| `task-manager:tasks:list:<token>:<variant>` | JSON page envelope (schema version 3) | `REDIS_CACHE_TTL`, default `60s` |

`<variant>` is an 8-byte hex fingerprint of the normalized query
(`status`, `assignee`, `cursor`, `page_size`; see
[ADR 0016](0016-cursor-pagination-for-task-list.md)). Equivalent requests share
one entry; a mutation advances the generation so every variant becomes
unreachable together.

- The generation token is initialized with `SET NX` on the first read, so
  concurrent first readers converge on one token without a lock.
- Each list entry has a finite TTL. A configurable TTL (`REDIS_CACHE_TTL`,
  default `60s`) bounds how long a cached list can survive a process that dies
  after a committed write but before invalidation. Zero, negative, and
  unparsable values fall back to `60s`; the adapter rejects non-positive TTLs.
- An empty page is stored with `"tasks":[]` (and pagination metadata) and is a
  valid hit. It is never treated as a miss or as `null`.
- Schema version **3** carries `page_size`, `next_cursor`, and `has_more`
  alongside `tasks` ([ADR 0016](0016-cursor-pagination-for-task-list.md)).
  Older versions and corrupt JSON are treated as misses after a
  compare-and-delete of the exact bytes observed, so a concurrent valid refill
  is not removed and the service can repopulate immediately.

### Mutation invalidation

`Service.Create`, `Update`, and `Delete` call `Invalidate` only after the
repository reports success. Validation failures and repository errors return
before invalidation, so a failed mutation leaves the cache alone.

`Invalidate` generates a new random token and writes it to the generation key
with a single atomic `SET`. That is the whole invalidation: readers name list
keys by the token they read, so the old entry becomes unreachable immediately
and expires on its own. The old entry is not deleted; deleting it would not
improve correctness and would add another command that can fail.

The invalidation runs with `context.WithoutCancel` and a two-second timeout. A
client disconnect after PostgreSQL committed must not skip invalidation. A slow
or unavailable Redis is bounded by the adapter's own 500 ms timeouts.

If invalidation fails, the service logs one structured warning with the
operation (`create`, `update`, or `delete`), the task ID, and the error, then
returns the successful mutation response. The API never reports a database
success as a cache failure.

### Stale-refill race

The generation token removes the classic stale-refill race:

1. GET reads generation `A` and misses `list:A`; it queries PostgreSQL and gets
   the old list.
2. A PUT commits and invalidates by writing generation `B` to the generation
   key.
3. The GET calls `Set` with its snapshot token `A`; the write lands at
   `list:A`, which no new reader can address.
4. The next GET reads generation `B`, misses, reads PostgreSQL, and writes
   `list:B`. The stale write at `list:A` expires unused.

A reader that observed `A` before the mutation may still answer that request
with the pre-mutation list; that request began before the write committed and
does not poison later reads. No reader that starts after the invalidation can
observe a pre-invalidation list. The design does not need a distributed lock or
a Lua script: the atomicity required is one `SET` of the generation key, which
Redis provides on a single key.

### Redis failure and recovery policy

The cache is fail-open for availability and fail-closed for reads after a
failed invalidation:

- **Startup:** an invalid URL or invalid TTL logs a warning and disables the
  cache; the API still starts. A valid URL creates a lazy client; no ping is
  required. The composition root performs one best-effort generation reset at
  startup so a previous process that died between commit and invalidation
  cannot leave a stale list readable. Failure is logged and non-fatal.
- **Read failure:** the service logs a warning and queries PostgreSQL.
- **Write failure:** the service logs a warning and returns the database result.
- **Malformed or version-mismatched payload:** the adapter discards the observed
  value only when it is unchanged (compare-and-delete), reports a miss with the
  observed generation token, and lets the service refill from PostgreSQL. The
  poison entry is not returned as a hit.
- **Invalidation failure:** the adapter marks itself degraded and reads report
  an error without calling Redis, so the service serves PostgreSQL. A
  rate-limited recovery probe then attempts a new generation at most once
  every five seconds; on success caching resumes with a cold start (the
  possibly-stale list is discarded, never trusted), and on failure the adapter
  stays degraded and retries later. A successful mutation also invalidates
  immediately. This deliberately favors correctness over hit rate: the process
  must not serve a generation that may have missed a committed write, and the
  probe exists so a single failed invalidation cannot disable caching until the
  next write or a restart.
- **Shutdown:** `App.Start` closes the Redis client after the HTTP server and
  metrics refresher stop, before the PostgreSQL pool is closed. A close error
  is logged.

The startup reset and the TTL bound the restart case. If a process invalidates,
then crashes, the new generation is already durable in Redis; if it fails to
invalidate and exits, the next process's startup reset advances the generation.
If the next process cannot reach Redis either, it degrades to PostgreSQL and
recovers through the probe once Redis answers. A configured TTL is the final
bound if a generation key is evicted or a future change bypasses the reset.

One window is genuinely bounded rather than removed: when invalidation fails
in a multi-process deployment, the other processes still read the generation
that was never advanced, so they may serve the pre-mutation list until the TTL
expires. Only the process that observed the failure knows the cache is
suspect. A single shared generation key cannot express "this entry is stale"
without a compare-and-set or a Lua script, which this scope does not justify.
The bound is `REDIS_CACHE_TTL` (default 60s), and Compose runs one API process,
so the single-process case has no such window.

Redis is a cache, not a source of truth. Compose and CI both run
`redis-server --save "" --appendonly no` (Compose via `command`, CI via
service `entrypoint`/`command`) and give the service no Redis volume; losing
all Redis data only causes misses.

## Alternatives considered

1. **Delete the list key after each mutation.** Rejected: it has the classic
   stale-refill race in which a reader that started before the mutation writes
   its old result after the delete. It also offers no ordering between the
   delete and an in-flight `SET` without a lock or a compare-and-set.
2. **A Lua script or distributed lock around read-fill and invalidation.**
   Rejected for this scope: the generation key already makes the dangerous
   write unreachable, and a lock adds contention, failure modes, and a script
   to review and test. A lock would be justified only if the mutation had to
   atomically update several keys.
3. **Write-through on mutation (update the list in Redis immediately).**
   Rejected: mutations only affect one row while the endpoint returns a full
   list. Rebuilding that list in the writer duplicates the read path, creates
   more write failures, and still has to handle concurrent readers. Invalidation
   plus lazy refill is smaller and self-correcting.
4. **Use PostgreSQL `LISTEN/NOTIFY` or a version column.** Rejected: the cache
   is per-process convenient state, not a cross-process contract. A database
   version would add a schema object and a query to every read, and would still
   need the same Redis generation logic to avoid stale refills.
5. **Store the generation in the cached value and compare-and-set.** Rejected:
   a plain `SET` of one generation key is atomic; comparing a cached payload
   still needs an atomic primitive or Lua. The separate key also lets a miss
   carry the observed generation to `Set` without inventing a sentinel.
6. **Use a cache library or a generic cache interface in `internal/infra`.**
   Rejected: the service needs three list-specific operations, and a generic
   framework would add configuration and types without a second consumer. The
   port lives at the use-case boundary as ADR 0012 requires.
7. **Add `miniredis` or a mock Redis client as a production dependency.**
   Rejected: the adapter has a tiny internal `store` interface for deterministic
   unit tests, and real Redis integration tests cover command semantics. No new
   test dependency is needed.
8. **Ping Redis during startup and fail fast.** Rejected: the API must remain
   usable with PostgreSQL when Redis is down. Compose therefore starts `app`
   once Redis is started, not once it is healthy, and the adapter recovers by
   itself once the server answers.

## Trade-offs

- **Gain:** repeated `GET /tasks` calls avoid the PostgreSQL table scan; a miss
  and mutation are both one Redis operation plus the database write.
- **Gain:** the generation token prevents stale refills without locks or Lua.
- **Gain:** Redis failure never changes the API contract or prevents PostgreSQL
  reads and writes; the process degrades to database-only behavior.
- **Cost:** a successful mutation performs one extra network round trip before
  returning (bounded by a two-second invalidation timeout). This is the price of
  invalidating before the response is reported.
- **Cost:** the generation key has no TTL and old list entries occupy Redis
  until their TTL expires. For a small task list this is negligible; a high
  write rate with a short TTL could leave several generations resident.
- **Cost:** after a failed invalidation, the process disables cache reads until
  a recovery probe succeeds (up to five seconds later) or the next mutation
  invalidates again. Correctness is preserved at the expense of hit rate.
- **Cost:** the cache payload duplicates the domain field names. A future
  domain field change must bump the payload schema version or be additive.
- **Limitation:** Redis and PostgreSQL are not transactional. A commit can
  succeed and the process can die before invalidation; the startup reset and TTL
  bound that window but do not remove it entirely.
- **Extension:** filtering and pagination (ADR 0014) keep one generation key and
  add a per-query variant fingerprint under it. Invalidation still advances a
  single token; it does not SCAN or delete individual variants.

## Consequences

- `GET /tasks` is cache-aside. Cache hits return the cached ordering; misses
  read PostgreSQL and populate Redis. Empty lists are cached and served as
  `[]`.
- `POST /task`, `PUT /tasks/{id}`, and `DELETE /tasks/{id}` invalidate only
  after a successful repository call. Failed or invalid requests never touch
  the cache.
- A cache failure never changes an HTTP status, body, or error envelope. It is
  visible in structured logs and in the hit-rate impact, not to API clients.
- Redis is optional at runtime. Compose supplies it, but `REDIS_URL` is empty
  by default outside Compose, in which case the service runs database-only.
- New read endpoints must decide whether they share the list generation or
  need their own key; blindly reusing the list key for filtered or paginated
  responses would be incorrect.
- New mutations that affect the task list must call the same invalidation path.
  A future `PATCH` or bulk operation that writes tasks without invalidating
  would reintroduce stale reads.
- Cache behavior is covered by service unit tests with fakes, adapter unit
  tests for generation/failure logic, and real-Redis integration tests for
  expiration, invalidation, malformed payloads, concurrent first reads, and the
  stale-refill ordering. CI provisions Redis with persistence disabled, runs
  the suite under `TEST_REDIS_URL`, and asserts that `TestRedisCache` cases
  report `--- PASS` (including miss-then-hit and stale-refill race) with no
  `--- SKIP`, while preserving `go test`'s exit status so a failure cannot be
  masked by the assertion greps.
- If cache hit-rate metrics become necessary, they are a separate observability
  decision; this ADR does not add metrics to the existing Prometheus registry.
