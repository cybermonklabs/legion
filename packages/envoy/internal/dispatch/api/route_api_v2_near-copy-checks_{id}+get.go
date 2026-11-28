package api

import (
	"net/http"

	"github.com/sjawhar/envoy/internal/dispatch/api/apicode"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

var _ = declareRow(func(s *server) routeRow {
	return routeRow{
		Method: http.MethodGet, Pattern: "/near-copy-checks/{id}", Auth: authAny, Versions: onlyV2,
		Description: "One near-copy gate answer by id: the row GET /api/v2/projects/{key}/near-copy-checks lists, with " +
			"spec, the spec as sent, on a refused row alone (absent on every other, whose issue holds it; empty for a " +
			"refused create that sent none). 404 NEAR_COPY_CHECK_NOT_FOUND for an id no answer has, a value that is no " +
			"uuid among them.",
		Handler:  s.getNearCopyCheckV2,
		Resource: "near-copy-checks", Kind: kindItem, Key: "{id}",
		Shape:    routeShape{Status: http.StatusOK, Response: model.NearCopyCheck{}},
		IDs:      &idRoute{Reason: "a near-copy check id: one answer of the near-copy gate"},
		Refusals: []apicode.Refusal{undeclaredKeyRefusal, nearCopyCheckMissingRefusal},
	}
})
