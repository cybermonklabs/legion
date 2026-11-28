package api

import (
	"net/http"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

var _ = declareRow(func(s *server) routeRow {
	return routeRow{
		Method: http.MethodGet, Pattern: "/me/agent-tokens", Auth: authAny, Versions: onlyV2,
		Description: "The caller's personal agent tokens, newest first, each {id, name, prefix, scopes, created_at, " +
			"last_used_at, revoked_at}: a revoked token stays listed with its revoked_at; a read-only token lists read. Needs the person scope. " +
			"limit 1 to 200 (default 50) and offset; {items, total, limit, offset}.",
		Handler:  s.listAgentTokensV2,
		Resource: "agent-tokens", Kind: kindCollection, Key: "{id}",
		Query:       agentTokensV2Query{},
		Shape:       routeShape{Status: http.StatusOK, Response: page[model.AgentToken]{}},
		OAuthScopes: v2PersonScope,
		Refusals:    listRefusals(),
	}
})
