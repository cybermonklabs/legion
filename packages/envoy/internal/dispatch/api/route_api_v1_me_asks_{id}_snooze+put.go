package api

import (
	"net/http"

	"github.com/sjawhar/envoy/internal/dispatch/api/apicode"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

var _ = declareRoute(routeDecl{
	Method:      http.MethodPut,
	Pattern:     "/api/v1/me/asks/{id}/snooze",
	Auth:        authAny,
	Description: "Snooze an inbox row for the caller until snoozed_until.",
	Handler:     (*server).putAskSnooze,
	Successor:   successorIs(http.MethodPatch, "/me/marks/{kind}/{id}", "kind=ask"),
	WriteClass:  scopeSession,
	ID:          &idRoute{Kind: "ask", NotFound: apicode.NotFound},
	OAuthScopes: routeScopeSet{model.ScopePerson},
	Shape: routeShape{
		Status: http.StatusOK, Response: userAskSnooze{}, Request: askSnoozeInput{},
	},
})
