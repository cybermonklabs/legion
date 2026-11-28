package api

import (
	"net/http"
)

var _ = declareRoute(routeDecl{
	Method:      http.MethodGet,
	Pattern:     "/api/v1/issues/{key}/handoffs/{phase}/latest",
	Auth:        authAny,
	Description: "The newest handoff of one phase of an issue with its body, the exact text written; 404 HANDOFF_NOT_FOUND when the phase has none. A closed issue's read too.",
	Handler:     (*server).getLatestHandoff,
	Read:        readEntry{Writes: readWritesNothing, AsOf: asOfNoHistory},
	// The two reads of one handoff with its body answer handoffRead, whose field_trees carries
	// the tree of each text the body's card draws.
	Shape: routeShape{Status: http.StatusOK, Response: handoffRead{}},
})
