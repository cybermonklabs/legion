package api

import (
	"net/http"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

var _ = declareRoute(routeDecl{
	Method:      http.MethodGet,
	Pattern:     "/api/v1/issues/{key}/handoffs/{phase}/latest",
	Auth:        authAny,
	Description: "The newest handoff of one phase of an issue with its body, the exact text written; 404 HANDOFF_NOT_FOUND when the phase has none. A closed issue's read too.",
	Handler:     (*server).getLatestHandoff,
	Read:        readEntry{Writes: readWritesNothing},
	Shape:       routeShape{Status: http.StatusOK, Response: model.Handoff{}},
})
