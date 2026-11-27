package api

import (
	"net/http"
)

var _ = declareRoute(routeDecl{
	Method:      http.MethodGet,
	Pattern:     "/api/v1/projects/{key}/architecture",
	Auth:        authAny,
	Description: "A project's component tree with the work attached: its sources' sync bookkeeping, each component with its source repository, per-component counts and issue rows, plus unassigned, not-architectural, and retired-link issues; 404 SOURCE_NOT_FOUND without a source.",
	Handler:     (*server).getArchitectureTree,
	Read:        readEntry{Writes: readWritesNothing, AsOf: asOfNoHistory},
	// The architecture read answers architectureTreeRead, whose components carry the prose's
	// tree and whose Not architectural rows carry their reasons', so the row names that read: the
	// document describes the body the handler sends, and a client generated from it holds the
	// field a reader draws.
	Shape: routeShape{Status: http.StatusOK, Response: architectureTreeRead{}},
})
