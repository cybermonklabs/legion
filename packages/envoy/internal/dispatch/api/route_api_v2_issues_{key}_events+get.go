package api

import (
	"net/http"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

var _ = declareRow(func(s *server) routeRow {
	return routeRow{
		Method: http.MethodGet, Pattern: "/issues/{key}/events", Auth: authAny, Versions: onlyV2,
		Description: "Page an issue's event log. " + ownerLogText + " The answer carries an ETag over the issue's event " +
			"sequence and the query; If-None-Match answers 304. An event's actor carries proven: true when the write's " +
			"credential proved it (a browser's person, a broker-issued token's session), and no proven when the request " +
			"only named it. 404 ISSUE_NOT_FOUND for an issue that does not exist.",
		Handler:  s.listIssueEventsV2,
		Resource: "events", Kind: kindLog, Position: seqPosition,
		Query: ownerEventsV2Query{},
		List:  listBounds{Default: maxEventPage, Max: maxEventPage},
		Shape: routeShape{
			Status: http.StatusOK, Response: page[model.Event]{},
			Also: []routeAnswer{{Status: http.StatusNotModified}},
		},
		Refusals: listRefusals(positionRefusal, issueMissingRefusal),
	}
})
