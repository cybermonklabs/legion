package api

import (
	"net/http"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

var _ = declareRoute(routeDecl{
	Method:      http.MethodDelete,
	Pattern:     "/api/v1/artifacts/{id}/subscribers/{session_id}",
	Auth:        authAny,
	Description: "Unsubscribe a session from a document.",
	Handler:     (*server).unsubscribeArtifactSession,
	WriteClass:  scopeProject,
	ID:          &idRoute{Kind: "artifact", NotFound: "ARTIFACT_NOT_FOUND"},
	OAuthScopes: routeScopeSet{model.ScopePerson},
	Shape:       routeShape{Status: http.StatusNoContent},
})
