package api

import (
	"net/http"
)

var _ = declareRoute(routeDecl{
	Method:      http.MethodPut,
	Pattern:     "/api/v1/asks/{id}/followers/{session_id}",
	Auth:        authAny,
	Description: "Follow an ask (a credential holding the person scope for any session; every other for its own session alone).",
	Handler:     (*server).followAsk,
	WriteClass:  scopeProject,
	ID:          &idRoute{Kind: "ask", NotFound: "NOT_FOUND"},
	Shape:       routeShape{Status: http.StatusNoContent, Request: actorInput{}, OptionalRequest: true},
})
