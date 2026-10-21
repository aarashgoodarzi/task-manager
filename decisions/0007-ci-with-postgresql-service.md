# 0007. CI runs the suite against a real PostgreSQL service and refuses silent skips

* Status: Accepted
* Date: 2026-10-20
* Scope: `.github/workflows/ci.yml`, `Makefile`, README testing documentation
* Related: [0006](0006-postgres-task-persistence.md) — the integration suite
  this workflow runs.

## Context

[ADR 0006](0006-postgres-task-persistence.md) made persistence verifiable, but
deliberately kept `go test ./...` runnable without PostgreSQL: the repository and
migration suites read `TEST_DATABASE_URL` and call `t.Skip` when it is unset.
The trade-off recorded there is exactly the problem this decision addresses — a
broken SQL change can pass a green local run because the only tests that execute
SQL never ran. On this tree the gap is measurable: **58.4%** statement coverage
without the database tier and **77.4%** with it.

There was also no CI at all: the repository had a Makefile, no `.github/`
directory, and the README listed a CI pipeline as not implemented. The upstream
remote is GitHub, so the available provider is GitHub Actions.

Four requirements shape the answer:

1. Run the database tier against a real PostgreSQL, not a mock.
2. Fail when the database is unavailable instead of succeeding with skips.
3. Keep the database-free tier honest and separately identifiable.
4. Never use production credentials, and never leave state behind for the next
   run.

## Decision

A single GitHub Actions workflow, [`.github/workflows/ci.yml`](../.github/workflows/ci.yml),
runs on pushes to `main`, on every pull request, and on manual dispatch:

- **Ephemeral PostgreSQL service.** The job declares
  `services.postgres` with the pinned image `postgres:16.4-alpine`, a
  `pg_isready` health check (5s interval, 20 retries), a dedicated database
  `task_manager_test`, and throwaway credentials supplied through the service
  environment. The runner starts an empty cluster per job and discards it, so
  runs cannot leak state into each other.
- **Credentials stay in the workflow, not in the repository.** The literals
  grant access only to that ephemeral container. No repository secret is
  required, and no production DSN is ever referenced.
- **`TEST_DATABASE_URL` is job-level; `DATABASE_URL` is not.** Configuration
  tests clear the variables they assert on, so a global `DATABASE_URL` would
  not change their result; keeping it step-scoped still makes the migration's
  dependency explicit and exercises the documented `cmd/migrate` path.
- **The database-free tier stays explicit.** The unit-test step clears
  `TEST_DATABASE_URL`, so it runs the same suite a developer runs without
  PostgreSQL; the integration step then runs everything against the service.
- **No silent skips.** `make test-integration` already refuses to start when
  `TEST_DATABASE_URL` is empty. Because `t.Skip` still exits 0, a separate step
  reruns representative database tests and requires `--- PASS` for
  `TestCreatePersistsTask` and `TestMigrateIsIdempotent` while rejecting any
  `--- SKIP`. Renaming those tests must update this assertion.
- **Ordered checks.** Formatting (fail on diff), `go vet`, `go build`,
  database-free tests, migrations, integration tests, execution assertion, race
  detector, coverage summary. Formatting fails the job instead of merely
  listing files, which is what `make fmt` does locally.
- **Sequential packages for database runs.** `make test-integration`, the race
  step, and the coverage step all pass `-p 1`. Packages share one database, and
  parallel package processes could interleave migrations and `TRUNCATE`s even
  though no test uses `t.Parallel`. The Makefile target carries the flag so a
  local integration run behaves like CI.
- **Read-only and non-deploying.** The workflow requests `contents: read`, sets
  a concurrency group that cancels superseded runs, and never deploys, pushes,
  or publishes anything.

## Alternatives considered

1. **Keep the integration suite local and manual.** Rejected: the status quo
   already allows a green suite with unverified SQL, which is the failure mode
   this work exists to remove.
2. **Testcontainers-go.** Rejected: it adds a Go dependency, requires Docker
   access inside the job, and duplicates what a native service container does
   with less code. It would be worth revisiting only if tests needed several
   database engines or per-test isolation that the service cannot express.
3. **A `docker-compose.yml` shared by CI and developers.** Rejected for now:
   the repository has no container workflow, the service block already declares
   the image, port, credentials, and health check, and a second source of truth
   would drift. A compose file can be added later for developers without
   changing CI semantics.
4. **A version matrix (for example PostgreSQL 14 and 16).** Deferred: the SQL
   uses only long-stable features, CI runs one pinned version, and a matrix
   doubles runner time before there is a compatibility claim to support. Adding
   `strategy.matrix.postgres` is the natural extension.
5. **Separate jobs for unit and integration tests.** Rejected at this size:
   both need the same checkout, toolchain, and cache; splitting duplicates
   setup. The tiers stay distinguishable through environment scoping instead.
6. **Clearing `TEST_DATABASE_URL` by default and requiring an opt-in flag.**
   Rejected: it makes the default CI run silently skip the database tier again.
7. **Letting the tests apply migrations only.** Rejected: CI should also prove
   that the documented `cmd/migrate` command works, including its environment
   handling.
8. **Trusting the exit status and dropping the assertion step.** Rejected:
   `go test` exits 0 when tests skip, so exit status alone cannot distinguish
   "passed" from "did not run". The assertion is the only evidence.
9. **Pinning the image by digest instead of tag.** Not adopted yet: the tag is
   pinned to a minor version and the digest differs per platform, which makes
   the file harder to audit. Digest pinning is a reasonable follow-up if
   reproducibility requirements tighten.

## Trade-offs

- **Gain:** a pull request cannot merge with SQL that never ran; the
  `cmd/migrate` path is exercised on every run; the database-free tier remains
  provable; runs are isolated by construction.
- **Cost:** each run spends runner minutes on a container startup, a migration,
  three database-backed test passes (integration, assertion, race, coverage),
  and a service health check.
- **Cost:** the assertion step depends on two test names, so renaming them
  breaks CI until the workflow is updated.
- **Cost:** CI runs PostgreSQL 16.4 while local verification was done on 14.13.
  The schema uses only long-stable features, but the matrix alternative is what
  would turn that gap into coverage.
- **Cost:** `-p 1` makes database-backed runs slower than Go's default
  parallelism. It buys determinism over a shared database.
- **Cost:** a service container is only available on GitHub-hosted runners;
  self-hosted setups would need an equivalent service or a compose file.
- **Mitigation:** keep database tests inside the existing gated packages, so a
  new test is covered by CI automatically without workflow edits.

## Consequences

- The database tier is no longer optional in review: local `go test ./...` may
  skip it, CI may not.
- `.github/workflows/ci.yml` is the single CI definition; changes to the test
  environment (`TEST_DATABASE_URL`, the test database name, the pinned image)
  must be made there and in the README together.
- The integration tests keep two guards that predate this decision and remain
  required: they skip when `TEST_DATABASE_URL` is unset, and they refuse to
  truncate a database whose name does not contain `test`.
- Renaming or removing the two asserted tests requires updating the workflow's
  assertion step; the comment in the workflow says so.
- A PostgreSQL version matrix, digest pinning, and coverage thresholds are
  explicit follow-ups rather than implicit promises.

## Amendment: coverage gate and image checks

[ADR 0008](0008-containers-and-coverage-gate.md) closes the coverage follow-up
and extends this workflow:

- The coverage summary step is replaced by `make coverage-check`, which enforces
  70% on the combined unit + integration suite and fails the job below it. The
  threshold lives in the `Makefile`, not in the workflow.
- The job also builds the Docker image, asserts that it runs as uid `10001`, and
  validates Compose with `docker compose config --quiet`.
- No deployment, publishing, or push step is added.

## Amendment: Redis service and hardened assertion steps

[ADR 0013](0013-redis-cache-aside-for-task-list.md) added a real Redis cache
adapter gated by `TEST_REDIS_URL`. The workflow now provisions Redis the same
way it provisions PostgreSQL:

- **Ephemeral Redis service.** `services.redis` uses `redis:7.4-alpine` with
  `entrypoint: redis-server` and `command: --save "" --appendonly no`, matching
  Compose so RDB snapshots and AOF are off for the job. A `redis-cli ping`
  health check gates readiness; `TEST_REDIS_URL` points at logical database 15.
- **No silent Redis skips.** After `make test-integration`, a dedicated step
  reruns `-run 'TestRedisCache'` and requires `--- PASS` for
  `TestRedisCacheMissThenHit` and `TestRedisCacheStaleRefillRace` while
  rejecting any `--- SKIP`.
- **Assertion steps preserve `go test` exit status.** Both the PostgreSQL and
  Redis assertion steps use `set -euo pipefail`, pipe output through `tee`,
  keep `PIPESTATUS[0]`, require the PASS lines, reject SKIP, then `exit` with
  the original status so a sibling FAIL cannot be masked by greps.
- **OpenAPI lint.** CI runs `make openapi-lint` (pinned Redocly CLI) so the
  contract check documented in [ADR 0009](0009-openapi-contract.md) is enforced
  on every run, not only locally.
