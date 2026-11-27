package api

import (
	"net/http"
)

var _ = declareRoute(routeDecl{
	Method:      http.MethodGet,
	Pattern:     "/api/v1/agents/{session_id}/artifacts/{slug}",
	Auth:        authAny,
	Description: "Read an agent's conversation's file or image and its versions by slug.",
	Handler:     (*server).getArtifact,
	Successor:   successorIs(http.MethodGet, "/agents/{session_id}/artifacts?slug={slug}"),
	Read:        readEntry{Writes: readWritesNothing},
	Shape:       routeShape{Status: http.StatusOK, Response: artifactRead{}},
})
