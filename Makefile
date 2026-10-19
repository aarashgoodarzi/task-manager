BINARY ?= task-manager
CMD    := ./cmd/task-manager

.PHONY: build run test vet fmt

build:
	go build -o $(BINARY) $(CMD)

run:
	go run $(CMD)

test:
	go test ./...

vet:
	go vet ./...

fmt:
	gofmt -l .
