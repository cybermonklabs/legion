package api

import (
	"net/http"

	"github.com/sjawhar/envoy/internal/dispatch/api/apicode"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

var _ = declareRow(func(s *server) routeRow {
	return routeRow{
		Method: http.MethodGet, Pattern: "/limit-set", Auth: authAny, Versions: onlyV2,
		Description: "The bounds a write is held to, each read from the constant that enforces it: block_units, how " +
			"long one block of a body may be; item_blocks, how many blocks a body may hold; ask_options and " +
			"ask_units, how many options an ask may have and how long its question and options are together; " +
			"event_body_units, how long a body an event carries whole may be; and request_bytes, document_bytes and " +
			"handoff_bytes, the bytes a request, a document and a handoff body may hold.",
		Handler:  s.getLimits,
		Resource: "limit-set", Kind: kindSingleton,
		Shape:    routeShape{Status: http.StatusOK, Response: model.DispatchLimits{}},
		Refusals: []apicode.Refusal{undeclaredKeyRefusal},
	}
})
