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
	Read:        readEntry{Writes: readWritesNothing, AsOf: asOfAnswers, AtACut: "At a cut the route reads the conversation's file or image as it stood at the cut: its name and the versions made by then. One made after the cut, or removed after it, is 404 ARTIFACT_NOT_FOUND: a removal keeps no row of it."},
	Shape:       routeShape{Status: http.StatusOK, Response: artifactRead{}, Params: asOfQuery{}},
})
