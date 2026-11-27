package api

import (
	"net/http"
)

var _ = declareRoute(routeDecl{
	Method:      http.MethodPost,
	Pattern:     "/api/v1/artifacts/{id}/_test/tangled",
	Auth:        authAny,
	Description: "Test hook: close the document's room and append a tangle no one apply of a browser's may scan through.",
	Handler:     (*server).tangleBeyondOneApply,
	// It writes the stored history, which every process reads.
	TestHook:   true,
	WriteClass: scopeProject,
	ID:         &idRoute{Kind: "artifact", NotFound: "ARTIFACT_NOT_FOUND"},
	Shape:      routeShape{Status: http.StatusNoContent},
})
