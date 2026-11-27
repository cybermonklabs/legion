package api

import (
	"net/http"
)

var _ = declareRoute(routeDecl{
	Method:      http.MethodGet,
	Pattern:     "/api/v1/issues/{key}/artifacts/{slug}",
	Auth:        authAny,
	Description: "Read an issue document by slug or unambiguous filename.",
	Handler:     (*server).getArtifact,
	Successor:   deletedFor(http.MethodGet, "/issues/{key}/artifacts?slug={slug}"),
	Read:        readEntry{Writes: readWritesNothing},
	Shape:       routeShape{Status: http.StatusOK, Response: artifactRead{}},
})
