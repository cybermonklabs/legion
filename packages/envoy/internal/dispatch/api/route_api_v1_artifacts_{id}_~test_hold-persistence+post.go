package api

import (
	"net/http"
)

var _ = declareRoute(routeDecl{
	Method:      http.MethodPost,
	Pattern:     "/api/v1/artifacts/{id}/_test/hold-persistence",
	Auth:        authAny,
	Description: "Test hook: hold this process's appends of a document's browser updates for a duration.",
	Handler:     (*server).holdRoomPersistence,
	TestHook:    true,
	WriteClass:  scopeProject,
	// The hook addresses the live room this process holds under the document's own id and reads
	// no row of the artifacts table, so it answers no 404 of a document that does not exist and
	// resolves no prefix: a short id written at it reaches the hook as the caller wrote it, as
	// every other value does.
	ID:    &idRoute{Reason: "a test hook's document id, naming a live room rather than a stored document"},
	Shape: routeShape{Status: http.StatusNoContent, Request: holdPersistenceInput{}},
})
