package api

import (
	"net/http"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

var _ = declareRoute(routeDecl{
	Method:      http.MethodDelete,
	Pattern:     "/api/v1/me/asks/{id}/snooze",
	Auth:        authAny,
	Description: "Un-snooze an inbox row for the caller.",
	Handler:     (*server).deleteAskSnooze,
	WriteClass:  scopeSession,
	ID:          &idRoute{Kind: "ask", NotFound: notFoundNone},
	OAuthScopes: routeScopeSet{model.ScopePerson},
	Shape:       routeShape{Status: http.StatusNoContent},
})
