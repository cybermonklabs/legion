package api

import (
	"net/http"

	"github.com/sjawhar/envoy/internal/dispatch/delivery"
)

var _ = declareRoute(routeDecl{
	Method:      http.MethodGet,
	Pattern:     "/api/v1/delivery/runs/{id}",
	Auth:        authAny,
	Description: "One run of a deploy source's repository of a project with delivery on, with every job it ran, by start, each with its result and timings; 400 RUN_ID_INPUT for an id that is not a positive integer, 404 NOT_FOUND for no such run, 404 DELIVERY_NOT_CONFIGURED when no project has delivery on.",
	Handler:     (*server).getDeliveryRun,
	ID:          &idRoute{Reason: "a deploy-repository run id, a positive integer and no uuid at all"},
	Read:        readEntry{Writes: readWritesNothing},
	Shape:       routeShape{Status: http.StatusOK, Response: delivery.DeliveryRunDetailView{}},
})
