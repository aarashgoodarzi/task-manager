# 0008. Containerized local environment and a 70% coverage gate on the combined suite

* Status: Accepted
* Date: 2026-10-20
* Scope: `Dockerfile`, `.dockerignore`, `compose.yaml`, `Makefile`,
  `.github/workflows/ci.yml`, README
* Related: [0003](0003-postgres-via-database-sql.md) (no migrations at
  startup), [0006](0006-postgres-task-persistence.md) (schema, repository,
  integration testing), [0007](0007-ci-with-postgresql-service.md) (CI)

## Context

Three gaps remained after the PostgreSQL cutover:

- **No way to run the stack without a hand-built environment.** The service
  requires PostgreSQL, a migrated schema, and a `DATABASE_URL`. A developer had
  to install PostgreSQL, create databases, export variables, and remember to
  migrate before starting. CI had a service container, but nothing reproducible
  existed for local use or for shipping the binary.
- **No production-shaped artifact.** The repository had no image, so nothing
  proved the binaries could run without a Go toolchain, a writable filesystem,
  or root.
- **Coverage was reported, not enforced.** [ADR 0006](0006-postgres-task-persistence.md)
  recorded 77.4% total coverage with the database tier and 58.4% without it.
  CI printed the number ([ADR 0007](0007-ci-with-postgresql-service.md)) but a
  regression below any threshold would still pass.

The existing decisions constrain the answer: PostgreSQL is the only task
storage, the service never migrates at startup, migrations are additive and
recorded in `schema_migrations`, and the integration suite is gated by
`TEST_DATABASE_URL`.

## Decision

### Image

A single multi-stage `Dockerfile`:

- **Build stage** on `golang:1.26.2-alpine`, matching `go.mod`. Modules are
  downloaded in their own layer before the source is copied, so source edits do
  not invalidate the module cache. `CGO_ENABLED=0`, `GOTOOLCHAIN=local`,
  `-trimpath`, `-buildvcs=false`, and `-ldflags="-s -w"` produce static,
  reproducible binaries and avoid toolchain downloads or `.git` dependency.
- **Runtime stage** on `alpine:3.22` with two binaries, `/app/task-manager` and
  `/app/migrate`, and nothing else: no source, tests, Go toolchain, or `.git`.
- **Non-root.** A dedicated `app` group and user at uid/gid `10001`; the
  binaries are readable and executable but the filesystem stays read-only for
  the process.
- **Health check** on `/health` through busybox `wget`, shell form so `${PORT}`
  follows runtime configuration. `/health` is the readiness probe: it fails when
  PostgreSQL is unreachable, which is the condition that actually stops the
  service from serving requests. `/health/live` is not used because container
  health should not be green while the database is gone.
- **Signals.** Exec-form `ENTRYPOINT ["/app/task-manager"]` keeps the service as
  PID 1, so `docker stop` sends `SIGTERM` to the process and its existing
  graceful shutdown runs. No init shim is needed.
- **No secrets.** Nothing is baked in; `DATABASE_URL`, `PORT`, and pool settings
  come from the environment exactly as they do outside a container.
- **Migrations are not run by the image's entrypoint.** `/app/migrate` exists so
  a deployment or Compose can invoke it explicitly, preserving ADR 0003's rule
  that starting the service never changes the schema.
- **`.dockerignore`** keeps `.git`, `.github`, `.freebuff`, `.env*`, coverage and
  binary artifacts, documentation, and local orchestration files out of the
  build context.

Alpine rather than `scratch` or distroless: the runtime needs an HTTP client for
`HEALTHCHECK`, and busybox provides `sh` and `wget` in ~7 MB. The static Go
binaries would run on `scratch`, but a health check without a client is not
possible there.

### Compose environment

`compose.yaml` defines three services under the project name `task-manager`:

| Service | Responsibility |
| --- | --- |
| `postgres` | `postgres:16.4-alpine`, named volume `task-manager_pgdata`, `pg_isready` health check, `restart: unless-stopped`. |
| `migrate` | One-shot run of the same image with `entrypoint: ["/app/migrate"]`; waits for PostgreSQL to be healthy and exits with the migration result (`restart: "no"`). |
| `app` | The API on host port `${APP_PORT:-8080}`, depending on a healthy database **and** a successfully completed migration (`condition: service_completed_successfully`). |

- Database connectivity is a single `DATABASE_URL` built from
  `POSTGRES_USER`, `POSTGRES_PASSWORD`, and `POSTGRES_DB`, with
  local-development defaults and the service name `postgres` as the host.
- The PostgreSQL port is **not** published: the API and the migration runner
  reach it over the Compose network, so the environment does not collide with a
  local PostgreSQL on 5432 or expose the database on the host. Inspection uses
  `docker compose exec postgres psql`.
- Data lives in a named volume and survives `restart`, `down`, and rebuilds.
  `docker compose down -v` is documented as the explicit destructive command.
- Credentials are placeholders for local development, overridable through an
  untracked `.env` file; they are not production values and are not used by CI.

### Coverage gate

- `make coverage` runs the combined suite (`-p 1`, one shared database) with
  `-coverprofile` and prints the total.
- `make coverage-check` enforces `COVERAGE_MIN` (default `70`) on that total and
  **refuses to run without `TEST_DATABASE_URL`**, because without the database
  tier the PostgreSQL adapter is never executed and the total is 58.4%.
  Measuring the combined suite is therefore the only consistent definition.
- CI replaces its coverage summary with `make coverage-check`, so the threshold
  lives in one place and a regression below 70% fails the job.
- The no-silent-skip assertion from ADR 0007 stays: a passing gate still has to
  prove the database tests executed.

### CI additions

The existing workflow ([ADR 0007](0007-ci-with-postgresql-service.md)) gains:

1. `make coverage-check` with the service database.
2. `docker build` of the image.
3. An assertion that the image runs as uid `10001`, so a lost `USER` line fails
   the build rather than only being caught in review.
4. `docker compose config --quiet` to validate the Compose file without starting
   containers.

No deployment or image publishing is added.

## Alternatives considered

1. **Single-stage image with the Go toolchain.** Rejected: hundreds of megabytes
   of compilers, a root shell, and build cache shipped to production; the image
   would also need write access for the toolchain.
2. **`scratch` or distroless runtime.** Rejected: no HTTP client for a health
   check and no shell for operational debugging. Distroless could work with a
   custom health prober, which is more moving parts than the busybox client.
3. **Let the app run migrations at startup.** Rejected by ADR 0003 and 0006:
   schema changes must stay an explicit step. The one-shot `migrate` service
   expresses the ordering without coupling migration to process start.
4. **`/docker-entrypoint-initdb.d` SQL scripts.** Rejected: they run only on an
   empty data directory, bypass `schema_migrations`, and silently do nothing
   after a schema change or a volume restore.
5. **Publish PostgreSQL on the host.** Rejected: nothing outside Compose needs
   it, it collides with an existing local server, and it widens the exposure of
   a development database. `docker compose exec` covers inspection.
6. **Bind-mount the database directory instead of a named volume.** Rejected:
   named volumes avoid host filesystem permission differences (especially on
   macOS) and are managed by Docker; bind mounts remain available for anyone who
   prefers them.
7. **`docker compose` with only `depends_on: [postgres]` (no health check).**
   Rejected: the app would race the database startup and fail closed. A health
   condition plus `service_completed_successfully` makes startup deterministic.
8. **A single Compose service that migrates and then serves.** Rejected: it
   reintroduces startup migration and hides the schema step.
9. **Gate coverage on the unit suite only.** Rejected: the unit suite cannot
   exceed the threshold while the SQL adapter is unexecuted (58.4%), so the gate
   would either be unattainable or meaningless. Combined measurement with a
   required `TEST_DATABASE_URL` is the honest definition.
10. **Per-package thresholds.** Rejected at this size: more configuration than
    signal; the total plus targeted tests is enough.
11. **SQL mocks to raise adapter coverage.** Rejected: ADR 0006 verifies the
    adapter against real SQL; a mock would assert the mock, not PostgreSQL.
12. **Pin base images by digest.** Not adopted: minor-version tags are pinned and
    readable; digest pinning remains a follow-up if reproducibility demands it.

## Trade-offs

- **Gain:** one command starts a working, migrated stack; the image is small
  (~48 MB), non-root, health-checked, and signal-correct; the threshold is
  enforced consistently locally and in CI; the migration order is explicit.
- **Cost:** two test passes against PostgreSQL in CI (integration and coverage),
  which adds roughly half a minute.
- **Cost:** `make coverage-check` requires a database; full offline verification
  of the gate is impossible by design.
- **Cost:** Alpine adds busybox, a larger surface than distroless, in exchange
  for a working health check and shell.
- **Cost:** Compose keeps data unless `-v` is used, which is safe by default but
  can surprise someone expecting a clean slate; the README and this record say
  so explicitly.
- **Cost:** image tags (`golang:1.26.2-alpine`, `alpine:3.22`,
  `postgres:16.4-alpine`) can move within their minor line; digests would remove
  that drift at the cost of update friction.
- **Cost:** the 70% total is a floor, not a guarantee: a large untested handler
  could be offset by well-covered SQL. Targeted tests and review remain the
  primary quality mechanism; the gate prevents silent decline.
- **Mitigation:** CI asserts non-root execution and Compose validity, so the two
  most likely container regressions fail the build instead of the deployment.

## Consequences

- `docker build` and `docker compose up -d --build` are the supported ways to
  run the service outside the Go toolchain; `make run` and `make migrate` remain
  the toolchain-based path.
- Adding a schema change means a new migration file; the Compose `migrate`
  service applies it on the next `up`, and the API refuses to start if the
  schema is missing.
- Changing the Dockerfile must preserve the non-root user (`10001`), the
  `/health` health check, and the exec-form entrypoint; CI enforces the first
  and the documentation explains the rest.
- Coverage regressions below 70% now fail both `make coverage-check` and CI; the
  measured total is 77.4%, leaving limited headroom that new code must maintain
  with tests.
- The `Makefile` is the single source of truth for the threshold; CI does not
  duplicate the number.
- New services or dependencies belong in `compose.yaml` with a health condition
  rather than in a growing `depends_on` list.
