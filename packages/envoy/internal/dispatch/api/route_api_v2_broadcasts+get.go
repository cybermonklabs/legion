package api

import (
	"net/http"
)

var _ = declareRow(func(s *server) routeRow {
	return routeRow{
		Method: http.MethodGet, Pattern: "/broadcasts", Auth: authAny, Versions: onlyV2,
		Description: "The broadcasts, which need the person scope, newest first, each {id, author, body, delivery, " +
			"created_at} with recipients, how many sessions it reached, and replies, how many of them answered. limit 1 " +
			"to 200 (default 50) and offset; {items, total, limit, offset}.",
		Handler:  s.listBroadcastsV2,
		Resource: "broadcasts", Kind: kindCollection, Key: "{id}",
		Query:       broadcastsV2Query{},
		Shape:       routeShape{Status: http.StatusOK, Response: page[broadcastSummary]{}},
		OAuthScopes: v2PersonScope,
		Refusals:    listRefusals(),
	}
})
