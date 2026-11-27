package api

import (
	"net/http"

	"github.com/sjawhar/envoy/internal/dispatch/api/apicode"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

var _ = declareRow(func(s *server) routeRow {
	return routeRow{
		Method: http.MethodPatch, Pattern: "/me/agent-tokens/{id}", Auth: authAny, Versions: onlyV2,
		Description: "Set the scopes of one of the caller's personal agent tokens {scopes}: the whole set it is to " +
			"hold, with the mint's refusals. A change to or from read only is always 400 TOKEN_SCOPES_INPUT; mint " +
			"a new token instead. A set that names a scope the caller does not hold itself is 403 " +
			"OAUTH_SCOPE_REQUIRED: only a credential that holds p0 names p0 in a set, also for a token that holds it " +
			"already, since the set sent is the whole set. Answers the token, with no plaintext. Needs the person and " +
			"approve scopes. " +
			"404 AGENT_TOKEN_NOT_FOUND for an id that is no token of the caller's, another person's among them.",
		Handler:  s.updateAgentTokenV2,
		Resource: "agent-tokens", Kind: kindItem, Key: "{id}",
		Body:        requestBody{Kind: bodyJSON, Type: updateAgentTokenScopesInput{}},
		Shape:       routeShape{Status: http.StatusOK, Response: model.AgentToken{}},
		OAuthScopes: v2SettlesScope,
		Scope:       scopeSession,
		IDs:         v2TokenIDs(),
		Refusals:    []apicode.Refusal{undeclaredKeyRefusal, agentTokenMissingRefusal, tokenScopesInvalidRefusal},
	}
})
