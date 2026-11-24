package api

import (
	"net/http"
)

var _ = declareRoute(routeDecl{
	Method:      http.MethodGet,
	Pattern:     "/api/v1/projects/{key}/artifacts",
	Auth:        authAny,
	Description: "List a project's documents.",
	Handler:     (*server).listProjectArtifacts,
	Read:        readEntry{Writes: readWritesNothing},
	Shape: routeShape{
		Status: http.StatusOK, Response: []artifactRead{}, Params: projectArtifactsQuery{},
	},
})
