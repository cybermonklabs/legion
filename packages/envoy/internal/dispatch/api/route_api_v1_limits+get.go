package api

import (
	"net/http"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

var _ = declareRoute(routeDecl{
	Method:      http.MethodGet,
	Pattern:     "/api/v1/limits",
	Auth:        authAny,
	Description: "The bounds a write is held to: how long one block of a body may be and how many blocks a body may hold, how many options an ask may have and how long its question and options are together, how long a body an event carries whole may be, and the bytes a request, a document and a handoff body may hold.",
	Handler:     (*server).getLimits,
	Read:        readEntry{Writes: readWritesNothing},
	Shape:       routeShape{Status: http.StatusOK, Response: model.DispatchLimits{}},
})
