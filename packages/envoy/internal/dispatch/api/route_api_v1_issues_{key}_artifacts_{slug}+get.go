package api

import (
	"net/http"
)

var _ = declareRoute(routeDecl{
	Method:      http.MethodGet,
	Pattern:     "/api/v1/issues/{key}/artifacts/{slug}",
	Auth:        authAny,
	Description: "Read an issue document by slug or unambiguous filename, as GET /api/v1/artifacts/{id} reads it, each version of a document carrying its digest.",
	Handler:     (*server).getArtifact,
	Read:        readEntry{Writes: readWritesNothing},
	Shape:       routeShape{Status: http.StatusOK, Response: artifactRead{}},
})
