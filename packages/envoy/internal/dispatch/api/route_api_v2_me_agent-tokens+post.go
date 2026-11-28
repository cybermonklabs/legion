package api

import (
	"net/http"

	"github.com/sjawhar/envoy/internal/dispatch/api/apicode"
)

var _ = declareRow(func(s *server) routeRow {
	return routeRow{
		Method: http.MethodPost, Pattern: "/me/agent-tokens", Auth: authAny, Versions: onlyV2,
		Description: "Mint a personal agent token {name, scopes?}: scopes is the set the new token holds, defaulting " +
			"to every scope the caller's own credential holds but p0, which a token holds only where the body names " +
			"it, and [] is a token that holds none. A blank name is 400 " +
			"AGENT_TOKEN_INPUT. Read alone or with person mints a read-only token. An unknown scope, approve without " +
			"person, or read with approve or p0 is 400 TOKEN_SCOPES_INPUT. Except for read, a scope the caller does not " +
			"hold itself, p0 among them, is 403 OAUTH_SCOPE_REQUIRED. Answers 201 the " +
			"token with its plaintext in token, the one time it is shown, so an Idempotency-Key never keeps the " +
			"answer: a repeat under the key is 409 KEY_COMMITTED and mints no second token. Needs the person and " +
			"approve scopes.",
		Handler:  s.createAgentTokenV2,
		Resource: "agent-tokens", Kind: kindCollection, Key: "{id}",
		Body:              requestBody{Kind: bodyJSON, Type: createAgentTokenInput{}},
		Shape:             routeShape{Status: http.StatusCreated, Response: createAgentTokenResponse{}},
		OAuthScopes:       v2SettlesScope,
		Scope:             scopeSession,
		AnswerHoldsSecret: true,
		Refusals: []apicode.Refusal{
			undeclaredKeyRefusal, tokenScopesInvalidRefusal,
			{Condition: apicode.AgentTokenNameBlank, Code: apicode.AgentTokenInput},
		},
	}
})
