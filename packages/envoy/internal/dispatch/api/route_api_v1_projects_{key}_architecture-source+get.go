package api

import (
	"net/http"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

var _ = declareRoute(routeDecl{
	Method:      http.MethodGet,
	Pattern:     "/api/v1/projects/{key}/architecture-source",
	Auth:        authAny,
	Description: "Kept for dispatch clients older than this release, logged at warn: the project's first architecture source, or null when it has none. GET /api/v1/projects/{key}/architecture-sources replaces it.",
	Handler:     (*server).getArchitectureSource,
	Read:        readEntry{Writes: readWritesNothing, AsOf: asOfNoHistory},
	Shape: routeShape{
		Status: http.StatusOK, Response: model.ArchitectureSource{},
		// A project with no source answers 200 and the JSON literal `null`
		// (architecture_source.go).
		Nullable: true,
	},
})
