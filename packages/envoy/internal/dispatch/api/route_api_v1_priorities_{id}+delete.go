package api

import (
	"net/http"
)

var _ = declareRoute(routeDecl{
	Method:      http.MethodDelete,
	Pattern:     "/api/v1/priorities/{id}",
	Auth:        authAny,
	Description: "Delete a priority: its state is written gone and no row is removed, so the listing answers without it while its history still names who ended it and when. An id no priority carries is 404 PRIORITY_NOT_FOUND; 204 otherwise.",
	Handler:     (*server).deletePriority,
	WriteClass:  scopeDeployment,
	ID:          &idRoute{Reason: "a priority id, a slug of its own and no uuid at all"},
	// A delete answers no body: the state it wrote is `gone` and the row is still there, which
	// the listing and the priority's own read already say (priorities.go).
	Shape: routeShape{Status: http.StatusNoContent, Request: actorInput{}, OptionalRequest: true},
})
