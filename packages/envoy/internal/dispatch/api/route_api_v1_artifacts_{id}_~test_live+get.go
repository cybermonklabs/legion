package api

import (
	"net/http"
)

var _ = declareRoute(routeDecl{
	Method:      http.MethodGet,
	Pattern:     "/api/v1/artifacts/{id}/_test/live",
	Auth:        authAny,
	Description: "Test hook: whether the document's room is resident in this server, {live}; loads and rebuilds nothing.",
	Handler:     (*server).artifactRoomLive,
	// It reads the process its call lands on, alone.
	TestHook: true,
	ID:       &idRoute{Kind: "artifact", NotFound: "ARTIFACT_NOT_FOUND"},
	// The test hooks' GET rows, which read this process's memory.
	Read:  readEntry{Writes: readWritesNothing},
	Shape: routeShape{Status: http.StatusOK, Response: roomLiveState{}},
})
