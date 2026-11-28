package api

import (
	"net/http"

	"github.com/sjawhar/envoy/internal/dispatch/api/apicode"
)

var _ = declareRoute(routeDecl{
	Method:      http.MethodPost,
	Pattern:     "/api/v1/artifacts/{id}/_test/outside-schema",
	Auth:        authAny,
	Description: "Test hook: write a crafted tree outside the Proof schema.",
	Handler:     (*server).injectArtifactSchemaFailure,
	TestHook:    true,
	WriteClass:  scopeProject,
	ID:          &idRoute{Kind: "artifact", NotFound: apicode.ArtifactNotFound},
	Shape:       routeShape{Status: http.StatusNoContent},
})
