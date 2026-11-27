package api

import (
	"net/http"
)

var _ = declareRoute(routeDecl{
	Method:      http.MethodGet,
	Pattern:     "/api/v1/issues/{key}/artifacts",
	Auth:        authAny,
	Description: "List an issue's documents and files.",
	Handler:     (*server).listArtifacts,
	Successor:   successorIs(http.MethodGet, "/issues/{key}/artifacts"),
	Read:        readEntry{Writes: readWritesNothing},
	Shape:       routeShape{Status: http.StatusOK, Response: []artifactRead{}},
})
