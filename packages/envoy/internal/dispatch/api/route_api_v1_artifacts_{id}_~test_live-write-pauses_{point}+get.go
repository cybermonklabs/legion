package api

import (
	"net/http"
)

var _ = declareRoute(routeDecl{
	Method:      http.MethodGet,
	Pattern:     "/api/v1/artifacts/{id}/_test/live-write-pauses/{point}",
	Auth:        authAny,
	Description: "Test hook: wait until a live write pauses at the armed pause.",
	Handler:     (*server).awaitLiveWritePaused,
	TestHook:    true,
	ID:          &idRoute{Reason: "a test hook's document id, naming a live room rather than a stored document"},
	Read:        readEntry{Writes: readWritesNothing},
	Shape: routeShape{
		Status: http.StatusOK, Response: liveWritePauseState{}, Params: liveWritePauseQuery{},
	},
})
