package api

import (
	"net/http"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

var _ = declareRoute(routeDecl{
	Method:      http.MethodPatch,
	Pattern:     "/api/v1/me/agent-tokens/{id}",
	Auth:        authAny,
	Description: "Set the scopes of one of the caller's personal agent tokens {scopes}: the whole set it is to hold, with the mint's refusals, answering the updated token. A set that names a scope the caller does not hold itself is 403 OAUTH_SCOPE_REQUIRED: only a credential that holds p0 names p0 in a set, also for a token that holds it already, since the set sent is the whole set. No token changes to or from read only: a set holding read, and any set for a read-only token, is 400 TOKEN_SCOPES_INPUT, since a person makes a new token for that. A token of another person's, or none, is 404 NOT_FOUND.",
	Handler:     (*server).updateAgentTokenScopes,
	Successor:   successorIs(http.MethodPatch, "/me/agent-tokens/{id}"),
	WriteClass:  scopeSession,
	ID:          &idRoute{Reason: "a personal agent token id"},
	OAuthScopes: routeScopeSet{model.ScopePerson, model.ScopeApprove},
	Shape: routeShape{
		Status: http.StatusOK, Response: model.AgentToken{}, Request: updateAgentTokenScopesInput{},
	},
})
