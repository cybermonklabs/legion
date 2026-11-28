package api

import (
	"net/http"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

var _ = declareRoute(routeDecl{
	Method:      http.MethodPost,
	Pattern:     "/api/v1/projects/{key}/architecture-source/sync",
	Auth:        authAny,
	Description: "Kept for dispatch clients older than this release, logged at warn: import every architecture source of a project now and answer one row, the first in id order whose last_error is set or else the first; 404 SOURCE_NOT_FOUND without a source. POST /api/v1/projects/{key}/architecture-sources/sync replaces it.",
	Handler:     (*server).syncSingularArchitectureSource,
	Successor:   deletedFor(http.MethodPost, "/projects/{key}/architecture-syncs"),
	WriteClass:  scopeProject,
	Shape:       routeShape{Status: http.StatusOK, Response: model.ArchitectureSource{}},
})
