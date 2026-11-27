package api

import (
	"net/http"
)

var _ = declareRow(func(s *server) routeRow {
	return routeRow{
		Method: http.MethodGet, Pattern: "/agents", Auth: authAny, Versions: onlyV2,
		Description: "The sessions of Dispatch's copy of the Envoy listener's session list, at most 5 s old: each " +
			"session as the listener describes it (its state, live or unresponsive, roles, capabilities, host and " +
			"last heartbeat), with open_asks, the open asks it asked on open issues and on documents, and " +
			"last_activity, the time of its newest event, null for none. The newest heartbeat first, then by session " +
			"id. limit 1 to 200 (default 50) and offset; {items, total, limit, offset}. 503 ENVOY_UNAVAILABLE where " +
			"Dispatch has no listener configured, 503 ENVOY_REFILLING with a Retry-After while the listener refills " +
			"its list, and 503 for as long as the listener has not answered.",
		Handler:  s.listAgentsV2,
		Resource: "agents", Kind: kindCollection, Key: "{session_id}",
		Query:    agentsV2Query{},
		Shape:    routeShape{Status: http.StatusOK, Response: page[agentResponse]{}},
		Refusals: listRefusals(envoyListenerRefusals()...),
	}
})
