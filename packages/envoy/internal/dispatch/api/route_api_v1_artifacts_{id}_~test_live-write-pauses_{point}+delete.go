package api

import (
	"net/http"
)

var _ = declareRoute(routeDecl{
	Method:      http.MethodDelete,
	Pattern:     "/api/v1/artifacts/{id}/_test/live-write-pauses/{point}",
	Auth:        authAny,
	Description: "Test hook: release the live write paused at the pause and disarm it.",
	Handler:     (*server).releaseLiveWritePause,
	TestHook:    true,
	WriteClass:  scopeProject,
	ID:          &idRoute{Reason: "a test hook's document id, naming a live room rather than a stored document"},
	Shape:       routeShape{Status: http.StatusOK, Response: liveWritePauseState{}},
})
