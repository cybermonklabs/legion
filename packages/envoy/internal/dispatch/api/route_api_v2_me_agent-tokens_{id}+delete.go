package api

import (
	"net/http"

	"github.com/sjawhar/envoy/internal/dispatch/api/apicode"
)

var _ = declareRow(func(s *server) routeRow {
	return routeRow{
		Method: http.MethodDelete, Pattern: "/me/agent-tokens/{id}", Auth: authAny, Versions: onlyV2,
		Description: "Revoke one of the caller's personal agent tokens: it is marked revoked, keeping its first " +
			"revoked_at and staying listed, and fails authentication with 401 TOKEN_REVOKED from then on. 204, a " +
			"token revoked already included. Needs the person and approve scopes. 404 AGENT_TOKEN_NOT_FOUND for an " +
			"id that is no token of the caller's, another person's among them.",
		Handler:  s.revokeAgentTokenV2,
		Resource: "agent-tokens", Kind: kindItem, Key: "{id}",
		Shape:       routeShape{Status: http.StatusNoContent},
		OAuthScopes: v2SettlesScope,
		Scope:       scopeSession,
		IDs:         v2TokenIDs(),
		Refusals:    []apicode.Refusal{undeclaredKeyRefusal, agentTokenMissingRefusal},
	}
})
