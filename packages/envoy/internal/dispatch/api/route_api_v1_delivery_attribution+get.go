package api

import (
	"net/http"

	"github.com/sjawhar/envoy/internal/dispatch/delivery"
)

var _ = declareRoute(routeDecl{
	Method:      http.MethodGet,
	Pattern:     "/api/v1/delivery/attribution",
	Auth:        authAny,
	Description: "What the attribution of the stored pull requests still waits on, counted at the request, with no delivery settings and no App key: {facts_left, reads_left, facts_refused, reading, keys_on_another_reading, moves_queued, links_in_two_cases, owed_failures, owed_failures_oldest_at, owed_refusals, owed_refusals_since}. reads_left is the pull requests a reconcile pass still reads the facts of: an open or merged results row whose texts are not current or whose commit messages are not read, and a timeline row with no results row; a pull request GitHub proved absent is in no count. facts_left is the part of them with no facts, whose issue is the stored one or not known; facts_refused the part whose last read GitHub refused. reading is the identity of the reading that runs now, and keys_on_another_reading the stored texts whose keys another reading made. moves_queued is the changes of a pull request's issue that wait for their events. links_in_two_cases is the URLs, folded to lower case and a pull request's /issues/N form to its /pull/N form, that more than one issue links. owed_failures and owed_refusals count the pull requests whose head is not read while a read of GitHub is owed: GitHub did not answer the read, or GitHub refuses it; a link recorded as unreadable is owed no read and is in neither count. owed_failures_oldest_at is the oldest time a failure is owed since. owed_refusals_since is the oldest time a refusal is refused since, or owed since where an older release owed it. Each time is absent where its count is 0.",
	Handler:     (*server).getDeliveryAttribution,
	Read:        readEntry{Writes: readWritesNothing},
	Shape:       routeShape{Status: http.StatusOK, Response: delivery.AttributionCounts{}},
})
