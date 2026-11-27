package api

import (
	"net/http"
)

var _ = declareRow(func(s *server) routeRow {
	return routeRow{
		Method: http.MethodGet, Pattern: "/delivery/pipeline", Auth: authAny,
		Description: "The Pipeline page: over the seven UTC days ending today (now, or ?at= for a fixed day), by the day each run started, the PR-checks workflow's run wall time and success, its five slowest jobs (50 timed results to rank) and the jobs that failed in the last 24 hours, the duration and result of the staging end-to-end, staging deploy and production deploy jobs the settings name in push runs on main, and merge-to-production and PR-open-to-merge per merge day against their targets; not filtered by the timeline's window or facets; 400 INVALID_QUERY for a malformed at, 404 DELIVERY_NOT_CONFIGURED without a delivery_settings row.",
		Handler:     s.getDeliveryPipeline,
		Query:       deliveryPipelineQuery{},
		Shape:       routeShape{Status: http.StatusOK, Response: deliveryPipelineResponse{}},
		OfINFRA979:  true,
	}
})
