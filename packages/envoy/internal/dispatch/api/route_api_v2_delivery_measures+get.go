package api

import (
	"net/http"
)

var _ = declareRow(func(s *server) routeRow {
	return routeRow{
		Method: http.MethodGet, Pattern: "/delivery/measures", Auth: authAny,
		Description: "The four delivery measures and Sami's KPI targets for [from, to) (default the last 7 days) and the same facets the timeline takes: deploys a day, lead time (merge, first commit and open to production; open to merge), change failure rate per PR and per deploy (flags_source says whether stored flags fed it), time to restore, rework share, deploy-run success, a per-UTC-day series, the open P0 issues with no owner, and whether each target is met; 404 DELIVERY_NOT_CONFIGURED without a delivery_settings row.",
		Handler:     s.getDeliveryMeasures,
		Query:       deliveryWindowQuery{},
		Shape:       routeShape{Status: http.StatusOK, Response: deliveryMeasuresResponse{}},
		OfINFRA979:  true,
	}
})
