# syntax=docker/dockerfile:1

# ---- build stage ----------------------------------------------------------
# The builder carries the toolchain; the runtime stage carries two static
# binaries and nothing else.
FROM golang:1.26.2-alpine AS build

WORKDIR /src

# Dependencies are downloaded in their own layer so source edits do not
# invalidate the module cache.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

# Static binaries: the runtime image has no libc toolchain and nothing here uses
# cgo. -trimpath and -buildvcs=false keep the build reproducible and independent
# of a .git directory; GOTOOLCHAIN=local pins the build to the image's Go.
ENV CGO_ENABLED=0 \
    GOTOOLCHAIN=local
RUN go build -trimpath -buildvcs=false -ldflags="-s -w" -o /out/task-manager ./cmd/task-manager \
 && go build -trimpath -buildvcs=false -ldflags="-s -w" -o /out/migrate ./cmd/migrate

# ---- runtime stage --------------------------------------------------------
FROM alpine:3.22 AS runtime

# A fixed numeric non-root identity. No shell account, no home directory.
RUN addgroup -S -g 10001 app \
 && adduser -S -D -H -u 10001 -G app app

WORKDIR /app
COPY --from=build /out/task-manager /app/task-manager
COPY --from=build /out/migrate /app/migrate

USER 10001:10001

# The service reads PORT from the environment (default :8080) and binds all
# interfaces, so it is reachable through a published port.
ENV PORT=8080
EXPOSE 8080

# /health is the readiness probe: it fails when PostgreSQL is unreachable, so
# the container is only healthy when it can actually serve task requests.
# It runs through a shell so ${PORT} follows the runtime configuration.
HEALTHCHECK --interval=10s --timeout=3s --start-period=10s --retries=5 \
    CMD ["/bin/sh", "-c", "wget -q -O /dev/null \"http://127.0.0.1:${PORT:-8080}/health\""]

# Exec form keeps the process as PID 1, so SIGTERM from `docker stop` reaches
# the service and its graceful shutdown path runs.
ENTRYPOINT ["/app/task-manager"]
