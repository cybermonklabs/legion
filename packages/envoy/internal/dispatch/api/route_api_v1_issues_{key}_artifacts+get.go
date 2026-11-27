package api

import (
	"net/http"

	"github.com/sjawhar/envoy/internal/dispatch/admission"
)

var _ = declareRoute(routeDecl{
	Method:      http.MethodGet,
	Pattern:     "/api/v1/issues/{key}/artifacts",
	Auth:        authAny,
	Description: "List an issue's documents and files.",
	Handler:     (*server).listArtifacts,
	Lane:        admission.IssueArtifacts,
	Read:        readEntry{Writes: readWritesNothing},
	Shape:       routeShape{Status: http.StatusOK, Response: []artifactRead{}},
})
