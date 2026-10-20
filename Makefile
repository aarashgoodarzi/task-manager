BINARY ?= task-manager
CMD    := ./cmd/task-manager

.PHONY: build run migrate test test-integration vet fmt

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

vet:
	go vet ./...

fmt:
	gofmt -l .
