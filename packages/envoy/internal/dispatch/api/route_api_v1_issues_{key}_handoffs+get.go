package api

import (
	"net/http"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

var _ = declareRoute(routeDecl{
	Method:      http.MethodGet,
	Pattern:     "/api/v1/issues/{key}/handoffs",
	Auth:        authAny,
	Description: "List an issue's handoffs, newest first, each {id, issue_key, phase, version, author, created_at} without its body; a closed issue's read too.",
	Handler:     (*server).listHandoffs,
	Read:        readEntry{Writes: readWritesNothing, AsOf: asOfNoHistory},
	Shape:       routeShape{Status: http.StatusOK, Response: []model.HandoffSummary{}},
})
