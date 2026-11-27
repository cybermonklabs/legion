package api

import (
	"net/http"
)

var _ = declareRoute(routeDecl{
	Method:      http.MethodPost,
	Pattern:     "/api/v1/artifacts/{id}/_test/refuse-next-update",
	Auth:        authAny,
	Description: "Test hook: make the document's room refuse the next update a read-write browser sends it, as it refuses one holding content the document does not take.",
	Handler:     (*server).refuseNextDocumentUpdate,
	// It acts on the process its call lands on, alone.
	TestHook:   true,
	WriteClass: scopeProject,
	ID:         &idRoute{Kind: "artifact", NotFound: "ARTIFACT_NOT_FOUND"},
	Shape:      routeShape{Status: http.StatusNoContent},
})
