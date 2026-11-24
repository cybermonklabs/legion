package api

import (
	"net/http"
)

var _ = declareRoute(routeDecl{
	Method:      http.MethodGet,
	Pattern:     "/api/v1/asks/{id}",
	Auth:        authAny,
	Description: "Read one ask with its replies, edit history, answers and followers, plus claimant (the session holding its issue's claim) and route (its issue's route), whom a person's answer and replies also reach, and route_holder (the live session that route reaches now: the session itself or the role's holder); each is null when absent, route_holder also when nobody holds the route or the Envoy listener did not answer, and all are null on a project document's ask.",
	Handler:     (*server).getAsk,
	// The read goes through loadAsk or loadAskOwner, whose missing row is writeHandlerError's
	// NOT_FOUND.
	ID:    &idRoute{Kind: "ask", NotFound: "NOT_FOUND"},
	Read:  readEntry{Writes: readWritesNothing, WriteNote: "The ask's anchor block is read through the document service. " + docsCompactionNote},
	Shape: routeShape{Status: http.StatusOK, Response: askRead{}},
})
