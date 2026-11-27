package api

import (
	"net/http"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

var _ = declareRoute(routeDecl{
	Method:      http.MethodPut,
	Pattern:     "/api/v1/me/threads/{id}/read",
	Auth:        authAny,
	Description: "Move the caller's read mark on one thread forward to {read_through} (RFC3339; stored as no later than now; more than a minute ahead of the server is 400 INVALID_STATE). It only moves forward; a thread the caller never wrote in writes nothing; 204 either way. A non-uuid id is 400 THREAD_ID_INPUT.",
	Handler:     (*server).putThreadRead,
	Successor:   successorIs(http.MethodPatch, "/me/marks/{kind}/{id}", "kind=thread"),
	WriteClass:  scopeSession,
	ID:          &idRoute{Reason: "a thread id: the root of a conversation, and no read of the message it names"},
	OAuthScopes: routeScopeSet{model.ScopePerson},
	Shape:       routeShape{Status: http.StatusNoContent, Request: threadReadInput{}},
})
