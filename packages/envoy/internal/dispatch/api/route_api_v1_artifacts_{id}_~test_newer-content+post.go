package api

import (
	"net/http"

	"github.com/sjawhar/envoy/internal/dispatch/api/apicode"
)

var _ = declareRoute(routeDecl{
	Method:      http.MethodPost,
	Pattern:     "/api/v1/artifacts/{id}/_test/newer-content",
	Auth:        authAny,
	Description: "Test hook: append an element no build knows, so the document holds newer content.",
	Handler:     (*server).injectArtifactNewerContent,
	TestHook:    true,
	WriteClass:  scopeProject,
	ID:          &idRoute{Kind: "artifact", NotFound: apicode.ArtifactNotFound},
	Shape:       routeShape{Status: http.StatusNoContent},
})
