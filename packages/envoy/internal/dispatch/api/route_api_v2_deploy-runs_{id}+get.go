package api

import (
	"net/http"

	"github.com/sjawhar/envoy/internal/dispatch/api/apicode"
	"github.com/sjawhar/envoy/internal/dispatch/delivery"
)

var _ = declareRow(func(s *server) routeRow {
	return routeRow{
		Method: http.MethodGet, Pattern: "/deploy-runs/{id}", Auth: authAny, Versions: onlyV2,
		Description: "One run of the deployment's deploy repository, by its GitHub run id, with every job it ran, by " +
			"start, each with its result and timings: {id, url, jobs}. The repository is the deployment's own " +
			"(GET /api/v2/settings/delivery), so the path names no project. 400 RUN_ID_INPUT for an id that is not " +
			"a positive integer, 404 NOT_FOUND for no such run of that repository, 404 DELIVERY_NOT_CONFIGURED " +
			"without the delivery settings.",
		Handler:  s.getDeliveryRun,
		Resource: "deploy-runs", Kind: kindItem, Key: "{id}",
		Shape:    routeShape{Status: http.StatusOK, Response: delivery.DeliveryRunDetailView{}},
		IDs:      &idRoute{Reason: "a deploy-repository run id, a positive integer and no uuid at all"},
		Refusals: []apicode.Refusal{undeclaredKeyRefusal, runIDMalformedRefusal, deployRunMissingRefusal, deliveryUnconfiguredRefusal},
	}
})
