package api

import (
	"net/http"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

var _ = declareRoute(routeDecl{
	Method:      http.MethodGet,
	Pattern:     "/api/v1/settings/architecture-sources",
	Auth:        authAny,
	Description: "List every project's architecture sources, by project and then in the order each project added them.",
	Handler:     (*server).listArchitectureSources,
	Read:        readEntry{Writes: readWritesNothing},
	Shape:       routeShape{Status: http.StatusOK, Response: []model.ArchitectureSource{}},
})
