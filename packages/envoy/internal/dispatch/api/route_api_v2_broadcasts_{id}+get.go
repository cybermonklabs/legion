package api

import (
	"net/http"

	"github.com/sjawhar/envoy/internal/dispatch/api/apicode"
)

var _ = declareRow(func(s *server) routeRow {
	return routeRow{
		Method: http.MethodGet, Pattern: "/broadcasts/{id}", Auth: authAny, Versions: onlyV2,
		Description: "One broadcast, which needs the person scope, with every recipient {session_id, message, replies}: " +
			"the copy it was sent with its delivery attempts, and the replies threaded under it, recipients in the order " +
			"the send named them (a broadcast from before that order was stored falls back to created_at, id). 404 " +
			"BROADCAST_NOT_FOUND for an id no broadcast has.",
		Handler:  s.getBroadcast,
		Resource: "broadcasts", Kind: kindItem, Key: "{id}",
		Shape:       routeShape{Status: http.StatusOK, Response: broadcastRead{}},
		OAuthScopes: v2PersonScope,
		IDs:         &idRoute{Reason: "a broadcast id: one send to many sessions"},
		Refusals:    []apicode.Refusal{undeclaredKeyRefusal, broadcastMissingRefusal},
	}
})
