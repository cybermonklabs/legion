package api

import (
	"net/http"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

var _ = declareRoute(routeDecl{
	Method:      http.MethodGet,
	Pattern:     "/api/v1/near-copy-checks/{id}",
	Auth:        authAny,
	Description: "One near-copy gate answer by id: the row GET /api/v1/near-copy-checks lists, with spec, the spec as sent, on a refused row alone (absent on every other, whose issue holds it; empty for a refused create that sent none); 404 NEAR_COPY_CHECK_NOT_FOUND when there is none, 400 NEAR_COPY_CHECK_ID_INPUT for an id that is not a uuid.",
	Handler:     (*server).getNearCopyCheck,
	ID:          &idRoute{Reason: "a near-copy check id: one answer of the near-copy gate"},
	Read:        readEntry{Writes: readWritesNothing, AsOf: asOfNoHistory},
	Shape:       routeShape{Status: http.StatusOK, Response: model.NearCopyCheck{}},
})
