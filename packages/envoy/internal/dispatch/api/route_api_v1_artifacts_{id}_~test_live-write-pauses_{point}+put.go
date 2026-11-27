package api

import (
	"net/http"
)

var _ = declareRoute(routeDecl{
	Method:      http.MethodPut,
	Pattern:     "/api/v1/artifacts/{id}/_test/live-write-pauses/{point}",
	Auth:        authAny,
	Description: "Test hook: pause the document's next live write at a point (after-first-append, after-commit).",
	Handler:     (*server).armLiveWritePause,
	TestHook:    true,
	WriteClass:  scopeProject,
	ID:          &idRoute{Reason: "a test hook's document id, naming a live room rather than a stored document"},
	Shape:       routeShape{Status: http.StatusNoContent},
})
