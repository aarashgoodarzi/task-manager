BINARY ?= task-manager
CMD    := ./cmd/task-manager

# Coverage gate for the combined unit + integration suite. The threshold is
# enforced by `make coverage-check`, which CI runs against PostgreSQL.
COVERAGE_MIN     ?= 70
COVERAGE_PROFILE ?= coverage.out

# Local Swagger helper (make swagger-local). Matches the default PORT and the
# embedded UI path served by internal/infra/swagger.
SWAGGER_READY_TIMEOUT ?= 60
SWAGGER_UI_PATH       := /swagger/index.html

# OpenAPI contract lint (development-only; not a Go dependency). Pin matches
# README / ADR 0009 so local and CI invoke the same Redocly CLI version.
OPENAPI_LINT ?= npx --yes @redocly/cli@2.66.0 lint api/openapi.yaml

.PHONY: build run app swagger-local migrate test test-integration coverage coverage-check vet fmt openapi-lint

# Optional local env file. The Go process does not read .env itself; these
# targets source it when present so `make app` / `make swagger-local` work after
# `cp .env.example .env`. Already-set variables (e.g. PORT=9090 make ...) win
# over values in .env. Keep as one line so the recipe stays a single shell.
LOAD_DOTENV = if [ -f .env ]; then _port=$${PORT-}; _db=$${DATABASE_URL-}; _redis=$${REDIS_URL-}; set -a; . ./.env; set +a; [ -n "$$_port" ] && PORT=$$_port; [ -n "$$_db" ] && DATABASE_URL=$$_db; [ -n "$$_redis" ] && REDIS_URL=$$_redis; fi

build:
	go build -o $(BINARY) $(CMD)

# Starts the API in the foreground (same as `make run`). Requires DATABASE_URL
# and an applied schema; Redis is optional. See README §3 Option B.
app:
	@set -eu; \
	$(LOAD_DOTENV); \
	if [ -z "$${DATABASE_URL:-}" ]; then \
		echo "app: DATABASE_URL is required." >&2; \
		echo "  export DATABASE_URL='postgres:///task_manager?host=localhost&sslmode=disable'" >&2; \
		echo "  # or: cp .env.example .env  # then edit DATABASE_URL and re-run" >&2; \
		exit 1; \
	fi; \
	go run $(CMD)

run: app

# Runs `make migrate`, then starts the API in the foreground, waits until THIS
# service's OpenAPI document and Swagger UI respond successfully, opens the UI,
# and keeps the process attached. Polling only /health/live is not enough:
# another process on the same port can return 200 while /swagger still 404s.
# Migration failure aborts before startup. Ctrl+C stops the server. Honors PORT
# (default 8080). Does not start Docker Compose or reset the database.
swagger-local: migrate
	@set -eu; \
	$(LOAD_DOTENV); \
	if [ -z "$${DATABASE_URL:-}" ]; then \
		echo "swagger-local: DATABASE_URL is required." >&2; \
		echo "  export DATABASE_URL='postgres:///task_manager?host=localhost&sslmode=disable'" >&2; \
		echo "  # or: cp .env.example .env  # then edit DATABASE_URL and re-run" >&2; \
		exit 1; \
	fi; \
	port="$${PORT:-8080}"; \
	base="http://127.0.0.1:$${port}"; \
	health="$${base}/health/live"; \
	spec="$${base}/swagger/openapi.yaml"; \
	swagger="$${base}$(SWAGGER_UI_PATH)"; \
	timeout="$(SWAGGER_READY_TIMEOUT)"; \
	shell_pid=$$$$; \
	( \
		i=0; \
		while true; do \
			if curl -sf "$$health" 2>/dev/null | grep -q '"status":"ok"' \
				&& curl -sf "$$spec" 2>/dev/null | grep -q 'openapi:' \
				&& curl -sf -o /dev/null -w '%{http_code}' "$$swagger" 2>/dev/null | grep -q '^200$$'; then \
				break; \
			fi; \
			i=$$((i + 1)); \
			if [ "$$i" -ge "$$timeout" ]; then \
				echo "swagger-local: timed out after $${timeout}s waiting for $$swagger (is PORT $${port} free and the app running?)" >&2; \
				kill -TERM "$$shell_pid" 2>/dev/null || true; \
				exit 1; \
			fi; \
			sleep 1; \
		done; \
		echo "swagger-local: opening $$swagger"; \
		case "$$(uname -s)" in \
			Darwin) open "$$swagger" ;; \
			Linux) \
				if command -v xdg-open >/dev/null 2>&1; then xdg-open "$$swagger" >/dev/null 2>&1 || true; \
				elif command -v sensible-browser >/dev/null 2>&1; then sensible-browser "$$swagger" >/dev/null 2>&1 || true; \
				else echo "swagger-local: open $$swagger in your browser (no opener found)" >&2; \
				fi ;; \
			*) echo "swagger-local: open $$swagger in your browser" >&2 ;; \
		esac; \
	) & \
	waiter=$$!; \
	cleanup() { \
		kill "$$waiter" 2>/dev/null || true; \
		pkill -P "$$shell_pid" 2>/dev/null || true; \
		wait "$$waiter" 2>/dev/null || true; \
	}; \
	trap cleanup EXIT INT TERM; \
	go run $(CMD); \
	status=$$?; \
	trap - EXIT INT TERM; \
	cleanup; \
	exit "$$status"

migrate:
	@set -eu; \
	$(LOAD_DOTENV); \
	if [ -z "$${DATABASE_URL:-}" ]; then \
		echo "migrate: DATABASE_URL is required." >&2; \
		echo "  export DATABASE_URL='postgres:///task_manager?host=localhost&sslmode=disable'" >&2; \
		echo "  # or: cp .env.example .env  # then edit DATABASE_URL and re-run" >&2; \
		exit 1; \
	fi; \
	go run ./cmd/migrate

test:
	go test ./...

# Runs the whole suite, including the PostgreSQL and Redis integration tests.
# Without the variables those tests skip, so the target refuses to run silently.
# Packages run sequentially (-p 1): they share one database, and parallel
# package execution could interleave schema, table, and key state.
test-integration:
	@if [ -z "$(TEST_DATABASE_URL)" ]; then \
		echo "TEST_DATABASE_URL is required, e.g. make test-integration TEST_DATABASE_URL='postgres:///task_manager_test?host=localhost&sslmode=disable' TEST_REDIS_URL='redis://localhost:6379/15'"; \
		exit 1; \
	fi
	@if [ -z "$(TEST_REDIS_URL)" ]; then \
		echo "TEST_REDIS_URL is required, e.g. TEST_REDIS_URL='redis://localhost:6379/15'"; \
		exit 1; \
	fi
	TEST_DATABASE_URL=$(TEST_DATABASE_URL) TEST_REDIS_URL=$(TEST_REDIS_URL) go test -count=1 -p 1 ./...

# Combined statement coverage of every package. Without the integration
# variables the PostgreSQL and Redis adapters never execute, so the total is
# well below the gate.
coverage:
	go test -count=1 -p 1 -coverprofile=$(COVERAGE_PROFILE) ./...
	go tool cover -func=$(COVERAGE_PROFILE) | tail -1

# Enforces $(COVERAGE_MIN) on the combined suite. Both integration variables are
# required so the gate cannot pass on a run that skipped an adapter tier.
coverage-check:
	@if [ -z "$(TEST_DATABASE_URL)" ]; then \
		echo "TEST_DATABASE_URL is required, e.g. make coverage-check TEST_DATABASE_URL='postgres:///task_manager_test?host=localhost&sslmode=disable' TEST_REDIS_URL='redis://localhost:6379/15'"; \
		exit 1; \
	fi
	@if [ -z "$(TEST_REDIS_URL)" ]; then \
		echo "TEST_REDIS_URL is required, e.g. TEST_REDIS_URL='redis://localhost:6379/15'"; \
		exit 1; \
	fi
	TEST_DATABASE_URL=$(TEST_DATABASE_URL) TEST_REDIS_URL=$(TEST_REDIS_URL) go test -count=1 -p 1 -coverprofile=$(COVERAGE_PROFILE) ./...
	@total=$$(go tool cover -func=$(COVERAGE_PROFILE) | tail -1 | awk '{print $$3}' | tr -d '%'); \
	echo "coverage: $$total% (minimum $(COVERAGE_MIN)%)"; \
	if awk -v t="$$total" -v m="$(COVERAGE_MIN)" 'BEGIN { exit !(t + 0 >= m + 0) }'; then \
		echo "coverage threshold met"; \
	else \
		echo "coverage $$total% is below the minimum $(COVERAGE_MIN)%" >&2; \
		exit 1; \
	fi

vet:
	go vet ./...

fmt:
	gofmt -l .

# Validates api/openapi.yaml with the pinned Redocly CLI. Requires Node.js/npx.
openapi-lint:
	$(OPENAPI_LINT)
