package api

import (
	"net/http"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

var _ = declareRoute(routeDecl{
	Method:      http.MethodGet,
	Pattern:     "/api/v1/priorities/{id}/history",
	Auth:        authAny,
	Description: "One priority's own history, newest first: every act stated about it as {field, value, issue, asserted, actor, at}. field is what was stated — title, description, owner and rank, the four a person writes, state (live when it was made, gone when it was deleted) and visibility; advances is the relation an issue states on it. value is what an attribute write wrote and is empty on an advances act; issue is the issue at the other end of an advances act and is empty on an attribute write; asserted is false on the act that retracted an advances and true on every other. A priority belongs to no project, so any authenticated caller reads this, the shared token every pod carries included. A deleted priority still answers its history, the delete its newest act; an id no priority carries is 404 PRIORITY_NOT_FOUND.",
	Handler:     (*server).priorityHistory,
	ID:          &idRoute{Reason: "a priority id, a slug of its own and no uuid at all"},
	Read:        readEntry{Writes: readWritesNothing},
	// The history is a bare list, newest act first: the acts are the whole answer, and the
	// priority they belong to is the id the path already names (priorities.go).
	Shape: routeShape{Status: http.StatusOK, Response: []model.PriorityAct{}},
})
