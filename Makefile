BINARY ?= task-manager
CMD    := ./cmd/task-manager

# Coverage gate for the combined unit + integration suite. The threshold is
# enforced by `make coverage-check`, which CI runs against PostgreSQL.
COVERAGE_MIN     ?= 70
COVERAGE_PROFILE ?= coverage.out

.PHONY: build run migrate test test-integration coverage coverage-check vet fmt

build:
	go build -o $(BINARY) $(CMD)

run:
	go run $(CMD)

migrate:
	go run ./cmd/migrate

test:
	go test ./...

# Runs the whole suite, including the PostgreSQL integration tests. Without
# TEST_DATABASE_URL those tests skip, so the target refuses to run silently.
# Packages run sequentially (-p 1): they share one database, and parallel
# package execution could interleave schema and table state.
test-integration:
	@if [ -z "$(TEST_DATABASE_URL)" ]; then \
		echo "TEST_DATABASE_URL is required, e.g. make test-integration TEST_DATABASE_URL='postgres:///task_manager_test?host=localhost&sslmode=disable'"; \
		exit 1; \
	fi
	TEST_DATABASE_URL=$(TEST_DATABASE_URL) go test -count=1 -p 1 ./...

# Combined statement coverage of every package. Without TEST_DATABASE_URL the
# PostgreSQL adapter never executes, so the total is well below the gate.
coverage:
	go test -count=1 -p 1 -coverprofile=$(COVERAGE_PROFILE) ./...
	go tool cover -func=$(COVERAGE_PROFILE) | tail -1

# Enforces $(COVERAGE_MIN) on the combined suite. TEST_DATABASE_URL is required
# so the gate cannot pass on a run that skipped the database tier.
coverage-check:
	@if [ -z "$(TEST_DATABASE_URL)" ]; then \
		echo "TEST_DATABASE_URL is required, e.g. make coverage-check TEST_DATABASE_URL='postgres:///task_manager_test?host=localhost&sslmode=disable'"; \
		exit 1; \
	fi
	go test -count=1 -p 1 -coverprofile=$(COVERAGE_PROFILE) ./...
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
