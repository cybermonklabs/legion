package api

import (
	"net/http"

	"github.com/sjawhar/envoy/internal/dispatch/api/apicode"
)

var _ = declareRow(func(s *server) routeRow {
	return routeRow{
		Method: http.MethodGet, Pattern: "/block-schema", Auth: authPublic, Versions: onlyV2,
		Description: "The typed document block schema as JSON, the bytes pre-rendered.",
		Handler:     s.getBlockSchema,
		Resource:    "block-schema", Kind: kindBytes,
		Shape: routeShape{
			Status:             http.StatusOK,
			Opaque:             "the block schema is pre-rendered JSON bytes (pmdoc.SchemaJSON), written as they stand",
			OpaqueContentTypes: []string{"application/json"},
		},
		Refusals: []apicode.Refusal{undeclaredKeyRefusal},
	}
})
