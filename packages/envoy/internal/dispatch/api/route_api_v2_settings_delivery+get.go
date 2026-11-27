package api

import (
	"net/http"

	"github.com/sjawhar/envoy/internal/dispatch/delivery"
)

var _ = declareRow(func(s *server) routeRow {
	return routeRow{
		Method: http.MethodGet, Pattern: "/settings/delivery", Auth: authAny,
		Description: "The delivery timeline's one configuration record (deploy repository, workflow paths, production job names, optional production apply step, population authors, excluded repositories, and the Pipeline page's optional staging end-to-end, staging deploy and PR-checks roll-up job names), or null when it has not been set.",
		Handler:     s.getDeliverySettings,
		Shape: routeShape{
			Status: http.StatusOK, Response: delivery.DeliverySettings{},
			// A deployment that has not set them answers 200 and the JSON literal `null`
			// (settings_delivery.go).
			Nullable: true,
		},
		OfINFRA979: true,
	}
})
