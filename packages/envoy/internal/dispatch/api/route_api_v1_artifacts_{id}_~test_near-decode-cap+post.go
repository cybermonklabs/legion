package api

import (
	"net/http"
)

var _ = declareRoute(routeDecl{
	Method:      http.MethodPost,
	Pattern:     "/api/v1/artifacts/{id}/_test/near-decode-cap",
	Auth:        authAny,
	Description: "Test hook: close the document's room and append deleted characters that leave its history near the decode cap, so the rebuild takes it and the page's read says so.",
	Handler:     (*server).nearDecodeCap,
	// It writes the stored history, which every process reads.
	TestHook:   true,
	WriteClass: scopeProject,
	ID:         &idRoute{Kind: "artifact", NotFound: "ARTIFACT_NOT_FOUND"},
	Shape:      routeShape{Status: http.StatusNoContent},
})
