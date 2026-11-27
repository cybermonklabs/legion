package api

import (
	"net/http"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

var _ = declareRoute(routeDecl{
	Method:      http.MethodPost,
	Pattern:     "/api/v1/me/agent-tokens",
	Auth:        authAny,
	Description: "Mint a personal agent token {name, scopes?}: scopes is the set the new token holds, defaulting to every scope the caller's own credential holds but p0, which a token holds only where the body names it. A set holding read mints a read-only token, which Dispatch answers on the GET routes it classes as reads and refuses every other route, 403 READ_ONLY_TOKEN: read alone, or read beside person. A scope this deployment does not know, approve without person, or read beside approve or p0, is 400 TOKEN_SCOPES_INPUT; a scope the caller does not hold itself, p0 among them, is 403 OAUTH_SCOPE_REQUIRED, and read needs none. The answer is the one time the token's plaintext is shown.",
	Handler:     (*server).createAgentToken,
	Successor:   successorIs(http.MethodPost, "/me/agent-tokens"),
	WriteClass:  scopeSession,
	OAuthScopes: routeScopeSet{model.ScopePerson, model.ScopeApprove},
	Shape: routeShape{
		Status: http.StatusCreated, Response: createAgentTokenResponse{}, Request: createAgentTokenInput{},
	},
})
