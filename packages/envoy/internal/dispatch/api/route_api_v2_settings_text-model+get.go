package api

import (
	"net/http"

	"github.com/sjawhar/envoy/internal/dispatch/api/apicode"
)

var _ = declareRow(func(s *server) routeRow {
	return routeRow{
		Method: http.MethodGet, Pattern: "/settings/text-model", Auth: authAny, Versions: onlyV2,
		Description: "Whether this deployment has a text model configured: {model_configured} and nothing else, so no " +
			"endpoint, deployment name or credential can appear in it. Any caller Dispatch admits reads it, a pod's own " +
			"agent token included.",
		Handler:  s.getGoingCapability,
		Resource: "text-model", Kind: kindSingleton,
		Shape:    routeShape{Status: http.StatusOK, Response: goingCapability{}},
		Refusals: []apicode.Refusal{undeclaredKeyRefusal},
	}
})
