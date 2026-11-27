package api

import (
	"net/http"
)

var _ = declareRoute(routeDecl{
	Method:      http.MethodPost,
	Pattern:     "/api/v1/artifacts/{id}/_test/hold-admission",
	Auth:        authAny,
	Description: "Test hook: make the document's next socket wait ms before its admission, as a browser in a burst waits for its room (0 takes the hold back).",
	Handler:     (*server).holdArtifactAdmission,
	// It acts on the process its call lands on, alone.
	TestHook:   true,
	WriteClass: scopeProject,
	ID:         &idRoute{Kind: "artifact", NotFound: "ARTIFACT_NOT_FOUND"},
	Shape: routeShape{
		Status: http.StatusNoContent, Request: holdAdmissionInput{},
	},
})
