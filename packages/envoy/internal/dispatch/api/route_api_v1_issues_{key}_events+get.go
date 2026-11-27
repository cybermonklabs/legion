package api

import (
	"net/http"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

var _ = declareRoute(routeDecl{
	Method:      http.MethodGet,
	Pattern:     "/api/v1/issues/{key}/events",
	Auth:        authAny,
	Description: "Page an issue's event log (after, before, ids, order, limit, exclude_types); after and before are positions in its event sequence from 0 to 2147483647 (the largest the store holds), and any other is 400 INVALID_QUERY. exclude_types is at most 50 comma-separated event types the page leaves out before limit counts it; an unknown type or an empty value is 400 INVALID_QUERY. The answer carries an ETag over the issue's event sequence and the paging query; If-None-Match answers 304. An event's actor carries proven: true when the write's credential proved it (a browser's person, a broker-issued token's session), and no proven when the request only named it.",
	Handler:     (*server).listIssueEvents,
	Read:        readEntry{Writes: readWritesNothing},
	Shape:       routeShape{Status: http.StatusOK, Response: []model.Event{}, Params: eventListQuery{}},
})
