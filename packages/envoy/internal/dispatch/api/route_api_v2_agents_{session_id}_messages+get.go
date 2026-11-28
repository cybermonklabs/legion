package api

import (
	"net/http"

	"github.com/sjawhar/envoy/internal/dispatch/api/apicode"
)

var _ = declareRow(func(s *server) routeRow {
	return routeRow{
		Method: http.MethodGet, Pattern: "/agents/{session_id}/messages", Auth: authAny, Versions: onlyV2,
		Description: "A session's conversations, which need the person scope, the one that moved last first: every root " +
			"message targeted at the session or one of whose attempts was delivered to it, issue messages, role messages " +
			"it held and broadcast copies among them. " + messageThreadText + " unread is whether the thread holds a " +
			"reply the caller has not read, and unread=true keeps only those. limit 1 to 200 (default 50) and offset; " +
			"{items, total, limit, offset}. A session Dispatch holds nothing for answers an empty page.",
		Handler:  s.listAgentMessagesV2,
		Resource: "messages", Kind: kindCollection, Key: "{id}",
		Query:       agentMessagesV2Query{},
		Shape:       routeShape{Status: http.StatusOK, Response: page[agentMessageV2]{}},
		OAuthScopes: v2PersonScope,
		Refusals:    listRefusals(apicode.Refusal{Condition: apicode.SessionIDBlank, Code: apicode.MessageInput}),
	}
})
