package api

import (
	"net/http"

	"github.com/sjawhar/envoy/internal/dispatch/api/apicode"
)

var _ = declareRoute(routeDecl{
	Method:      http.MethodPut,
	Pattern:     "/api/v1/asks/{id}/followers/{session_id}",
	Auth:        authAny,
	Description: "Follow an ask (a credential holding the person scope for any session; every other for its own session alone).",
	Handler:     (*server).followAsk,
	Successor:   successorIs(http.MethodPut, "/asks/{id}/followers/{session_id}"),
	WriteClass:  scopeProject,
	ID:          &idRoute{Kind: "ask", NotFound: apicode.NotFound},
	Shape:       routeShape{Status: http.StatusNoContent, Request: actorInput{}, OptionalRequest: true},
})
