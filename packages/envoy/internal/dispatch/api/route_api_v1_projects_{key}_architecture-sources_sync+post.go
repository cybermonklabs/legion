package api

import (
	"net/http"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

var _ = declareRoute(routeDecl{
	Method:      http.MethodPost,
	Pattern:     "/api/v1/projects/{key}/architecture-sources/sync",
	Auth:        authAny,
	Description: "Import every architecture source of a project now, each whatever another's import did; 200 carries every updated row in the order the sources were added, each with its own last_error (an access failure included).",
	Handler:     (*server).syncArchitectureSources,
	WriteClass:  scopeProject,
	Shape:       routeShape{Status: http.StatusOK, Response: []model.ArchitectureSource{}},
})
