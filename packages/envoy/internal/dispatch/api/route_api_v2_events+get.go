package api

import (
	"net/http"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

var _ = declareRow(func(s *server) routeRow {
	return routeRow{
		Method: http.MethodGet, Pattern: "/events", Auth: authAny, Versions: onlyV2,
		Description: "Page the whole event log, whatever owns each event (an issue, a document, a project or none), read up to the " +
			"log's safe horizon, so paging to the end of an answer skips no event committed before it. Position: id, each " +
			"event's id. after=<id> keeps the events above it and before=<id> those below it; sort=id (the default, oldest " +
			"first) or -id, and with no sort a before alone reads down from it, newest first. limit 1 to 1000 (default 1000) " +
			"and offset; {items, total, limit, offset}, and total is null on this route and on the caller's dismissed threads " +
			"(GET /api/v2/me/threads?state=dismissed), and on no other list: counting the log costs about ten " +
			"times its page for a reader near the oldest event (measured 2026-11-16), so a reader pages by after until a page " +
			"comes back short. An after above the highest event id written, or an after or a before that is no integer of 0 " +
			"or more, is 400 POSITION_INVALID naming the parameter and id. An event's actor carries proven: true when the " +
			"write's credential proved it (a browser's person, a broker-issued token's session), and no proven when the " +
			"request only named it.",
		Handler:  s.listEventLogV2,
		Resource: "events", Kind: kindLog, Position: eventIDPosition,
		Query:    eventLogV2Query{},
		List:     listBounds{Default: maxEventReplay, Max: maxEventReplay},
		Shape:    routeShape{Status: http.StatusOK, Response: logPage[model.Event]{}},
		Refusals: listRefusals(positionRefusal),
	}
})
