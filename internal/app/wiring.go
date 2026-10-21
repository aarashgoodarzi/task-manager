package app

import (
	"github.com/aarashgoodarzi/task-manager/internal/infra/server"
	"github.com/aarashgoodarzi/task-manager/internal/infra/swagger"
	"github.com/aarashgoodarzi/task-manager/internal/task/handler"
	"github.com/aarashgoodarzi/task-manager/internal/task/repository/postgres"
	"github.com/aarashgoodarzi/task-manager/internal/task/service"
)

// buildHandlers composes the HTTP modules the process serves. Each module is a
// server.RouteRegistrar that owns the routes it exposes; the server mounts
// them between its shared middleware and its 404/405 handlers.
//
// Repository construction and use-case wiring for HTTP modules belong here, so
// the server.New call site stays free of feature-specific adapters. Adding a
// module means adding one entry here. Shared process-level dependencies (the
// pool, cache, logger) come from App fields.
func (a *App) buildHandlers() []server.RouteRegistrar {
	tasks := postgres.New(a.db)
	return []server.RouteRegistrar{
		handler.New(service.New(tasks, a.cache, a.log), a.log),
		swagger.New(),
	}
}
