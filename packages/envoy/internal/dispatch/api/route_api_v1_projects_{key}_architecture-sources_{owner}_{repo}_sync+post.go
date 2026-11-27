package api

import (
	"net/http"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

var _ = declareRoute(routeDecl{
	Method:      http.MethodPost,
	Pattern:     "/api/v1/projects/{key}/architecture-sources/{owner}/{repo}/sync",
	Auth:        authAny,
	Description: "Import one architecture source's model now; 200 carries its updated row (last_error set when the import failed), 409 SOURCE_ACCESS a credential or branch problem.",
	Handler:     (*server).syncArchitectureSource,
	WriteClass:  scopeProject,
	Shape:       routeShape{Status: http.StatusOK, Response: model.ArchitectureSource{}},
})
