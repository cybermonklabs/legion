package api

import (
	"net/http"

	"github.com/sjawhar/envoy/internal/dispatch/delivery"
)

var _ = declareRoute(routeDecl{
	Method:      http.MethodGet,
	Pattern:     "/api/v1/delivery/runs/{id}",
	Auth:        authAny,
	Description: "One deploy-repository run with every job it ran, by start, each with its result and timings; 400 RUN_ID_INPUT for an id that is not a positive integer, 404 NOT_FOUND for no such run, 404 DELIVERY_NOT_CONFIGURED without a delivery_settings row.",
	Handler:     (*server).getDeliveryRun,
	ID:          &idRoute{Reason: "a deploy-repository run id, a positive integer and no uuid at all"},
	Read:        readEntry{Writes: readWritesNothing, AsOf: asOfNoHistory},
	Shape:       routeShape{Status: http.StatusOK, Response: delivery.DeliveryRunDetailView{}},
})
