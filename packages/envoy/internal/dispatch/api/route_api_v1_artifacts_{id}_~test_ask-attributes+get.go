package api

import (
	"net/http"
)

var _ = declareRoute(routeDecl{
	Method:      http.MethodGet,
	Pattern:     "/api/v1/artifacts/{id}/_test/ask-attributes",
	Auth:        authAny,
	Description: "Test hook: read the attributes each ask block of a document holds in this process's live tree.",
	Handler:     (*server).getLiveAskAttributes,
	TestHook:    true,
	ID:          &idRoute{Reason: "a test hook's document id, naming a live room rather than a stored document"},
	Read:        readEntry{Writes: readWritesNothing},
	Shape:       routeShape{Status: http.StatusOK, Response: liveAskAttributes{}},
})
