package api

import (
	"net/http"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

var _ = declareRoute(routeDecl{
	Method:      http.MethodPost,
	Pattern:     "/api/v1/asks/{id}/answer",
	Auth:        authAny,
	Description: "Answer an ask with selected options and/or text.",
	Handler:     (*server).answerAsk,
	WriteClass:  scopeProject,
	ID:          &idRoute{Kind: "ask", NotFound: "NOT_FOUND"},
	OAuthScopes: routeScopeSet{model.ScopePerson},
	// An approve-only token reaches this route for an approval request alone: answerAsk refuses it
	// the answer of every other ask, 403 APPROVE_ONLY_TOKEN, once the ask's kind is read
	// (ask_transitions.go).
	ApproveOnly: true,
	Shape:       routeShape{Status: http.StatusOK, Response: shapedAsk{}, Request: answerAskInput{}},
})
