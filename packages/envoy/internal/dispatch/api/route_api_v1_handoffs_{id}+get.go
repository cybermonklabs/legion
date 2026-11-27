package api

import (
	"net/http"
)

var _ = declareRoute(routeDecl{
	Method:      http.MethodGet,
	Pattern:     "/api/v1/handoffs/{id}",
	Auth:        authAny,
	Description: "One handoff by id with its body, the exact text written; 404 HANDOFF_NOT_FOUND when there is none, 400 HANDOFF_ID_INPUT for an id that is not a uuid.",
	Handler:     (*server).getHandoff,
	Successor:   successorIs(http.MethodGet, "/handoffs/{id}"),
	ID:          &idRoute{Reason: "a handoff id: one phase's record on an issue"},
	Read:        readEntry{Writes: readWritesNothing},
	Shape:       routeShape{Status: http.StatusOK, Response: handoffRead{}},
})
