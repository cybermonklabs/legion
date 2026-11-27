package api

import (
	"net/http"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

var _ = declareRoute(routeDecl{
	Method:      http.MethodGet,
	Pattern:     "/api/v1/projects/{key}/architecture-sources",
	Auth:        authAny,
	Description: "A project's architecture sources, one per repository, in the order they were added; [] when it has none. The project's component model is every source's components together.",
	Handler:     (*server).listProjectArchitectureSources,
	Successor:   successorIs(http.MethodGet, "/projects/{key}/architecture-sources"),
	Read:        readEntry{Writes: readWritesNothing},
	Shape:       routeShape{Status: http.StatusOK, Response: []model.ArchitectureSource{}},
})
