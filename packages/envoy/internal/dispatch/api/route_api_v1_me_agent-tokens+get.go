package api

import (
	"net/http"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

var _ = declareRoute(routeDecl{
	Method:      http.MethodGet,
	Pattern:     "/api/v1/me/agent-tokens",
	Auth:        authAny,
	Description: "List the caller's personal agent tokens, newest first, each with the scopes it holds: a read-only token's scopes hold read.",
	Handler:     (*server).listAgentTokens,
	OAuthScopes: routeScopeSet{model.ScopePerson},
	Read:        readEntry{Writes: readWritesNothing},
	Shape:       routeShape{Status: http.StatusOK, Response: []model.AgentToken{}},
})
