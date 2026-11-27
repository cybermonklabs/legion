package api

import (
	"net/http"

	"github.com/sjawhar/envoy/internal/dispatch/api/apicode"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

var _ = declareRoute(routeDecl{
	Method:      http.MethodPost,
	Pattern:     "/api/v1/asks/{id}/decline",
	Auth:        authAny,
	Description: "Decline to answer an ask: the person closes it with no answer, and Dispatch writes beside their note the words that tell the asking agent to decide without one. The note is optional, and at most the event body limit (400 CAP_EXCEEDED); the ask resolves with the kind declined, a block ask's block records it as every other resolution does, and an approval request is 409 ASK_NOT_DECLINABLE, being answered with Approve or Request changes.",
	Handler:     (*server).declineAsk,
	Successor:   successorIs(http.MethodPut, "/asks/{id}/state"),
	WriteClass:  scopeProject,
	ID:          &idRoute{Kind: "ask", NotFound: apicode.NotFound},
	OAuthScopes: routeScopeSet{model.ScopePerson},
	Shape:       routeShape{Status: http.StatusOK, Response: shapedAsk{}, Request: declineAskInput{}},
})
