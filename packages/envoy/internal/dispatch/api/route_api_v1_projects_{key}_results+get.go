package api

import (
	"net/http"
)

var _ = declareRoute(routeDecl{
	Method:      http.MethodGet,
	Pattern:     "/api/v1/projects/{key}/results",
	Auth:        authAny,
	Description: "Every result CI jobs wrote for checks of the spec files at ?commit= (40 lowercase hex, else 400 INVALID_QUERY), of ?check= alone when given: {results}, each {id, seq, kind, attestation}, newest first by seq, Dispatch's record order. A results token is refused it.",
	Handler:     (*server).listSpecCheckResults,
	// No Successor: a merge on main added this route after /api/v2's freeze, and it awaits its
	// /api/v2 route (testdata/v1-awaiting-v2-routes.txt), so its answers link GET /api/v2.

	Read: readEntry{Writes: readWritesNothing, AsOf: asOfNoHistory},
	Shape: routeShape{
		Status: http.StatusOK, Response: specCheckResults{}, Params: specCheckResultsQuery{},
	},
})
