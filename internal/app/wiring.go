package app

import (
	"github.com/aarashgoodarzi/task-manager/internal/infra/server"
	"github.com/aarashgoodarzi/task-manager/internal/infra/swagger"
	"github.com/aarashgoodarzi/task-manager/internal/task/handler"
	"github.com/aarashgoodarzi/task-manager/internal/task/service"
)

// buildHandlers composes the HTTP modules the process serves. Each module is a
// server.RouteRegistrar that owns the routes it exposes; the server mounts
// them between its shared middleware and its 404/405 handlers.
//
// Adding a module means adding one entry here. Shared dependencies arrive as
// arguments or App fields, so this function, not Initialize, changes when a
// feature is added.
func (a *App) buildHandlers(tasks service.Repository) []server.RouteRegistrar {
	return []server.RouteRegistrar{
		handler.New(service.New(tasks), a.log),
		swagger.New(),
	}
}
