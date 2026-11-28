package api

import (
	"net/http"

	"github.com/sjawhar/envoy/internal/dispatch/api/apicode"
)

var _ = declareRow(func(s *server) routeRow {
	return routeRow{
		Method: http.MethodGet, Pattern: "/agents/{session_id}/artifacts", Auth: authAny, Versions: onlyV2,
		Description: "List the files and images of an agent's conversation, oldest first. " + artifactRowText +
			" slug=<slug> keeps the one at ref_key agent/<session_id>/<slug>, as dispatch://agent/<session_id>/artifact/<slug> " +
			"names it; a conversation holds no document, so no file name is matched. limit 1 to 200 (default 50) and offset; " +
			"{items, total, limit, offset}. A session Dispatch holds no file of answers the empty list; a session_id that " +
			"is empty or holds /, ?, #, whitespace, a control character or <>\"'`[]| is 400 INVALID_SESSION_ID. The list " +
			"that names no slug is part of the conversation, so it needs the person scope (403 OAUTH_SCOPE_REQUIRED) and " +
			"admits exactly the callers GET /api/v2/agents/{session_id}/messages admits, as GET " +
			"/api/v1/agents/{session_id}/artifacts does; the read of one file by its slug needs no more than GET " +
			"/api/v1/agents/{session_id}/artifacts/{slug} does.",
		Handler:  s.listAgentArtifactsV2,
		Resource: "artifacts", Kind: kindCollection, Key: "{id}",
		Query:       agentArtifactsV2Query{},
		Shape:       routeShape{Status: http.StatusOK, Response: page[artifactV2]{}},
		Authorities: []authorityRule{agentArtifactListAuthority},
		Refusals: listRefusals(sessionIDMalformedRefusal, actingSessionMismatchRefusal,
			apicode.Refusal{Condition: apicode.AgentArtifactListWithoutPersonScope, Code: apicode.OAuthScopeRequired}),
	}
})
