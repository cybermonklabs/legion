package api

import (
	"net/http"
)

var _ = declareRoute(routeDecl{
	Method:      http.MethodGet,
	Pattern:     "/api/v1/schema/blocks",
	Auth:        authPublic,
	Description: "The typed document block schema as JSON.",
	Handler:     (*server).getBlockSchema,
	Read:        readEntry{Writes: readWritesNothing, AsOf: asOfNoHistory},
	Shape: routeShape{
		Status:             http.StatusOK,
		Opaque:             "the block schema is pre-rendered JSON bytes (pmdoc.SchemaJSON), written as they stand",
		OpaqueContentTypes: []string{"application/json"},
	},
})
