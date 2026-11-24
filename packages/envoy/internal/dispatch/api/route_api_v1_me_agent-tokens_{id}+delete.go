package api

import (
	"net/http"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

var _ = declareRoute(routeDecl{
	Method:      http.MethodDelete,
	Pattern:     "/api/v1/me/agent-tokens/{id}",
	Auth:        authAny,
	Description: "Revoke a personal agent token, read-only or not; a revoked token answers 401 TOKEN_REVOKED.",
	Handler:     (*server).revokeAgentToken,
	WriteClass:  scopeSession,
	ID:          &idRoute{Reason: "a personal agent token id"},
	OAuthScopes: routeScopeSet{model.ScopePerson, model.ScopeApprove},
	Shape:       routeShape{Status: http.StatusNoContent},
})
