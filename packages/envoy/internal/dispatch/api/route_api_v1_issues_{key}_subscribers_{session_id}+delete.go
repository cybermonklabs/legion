package api

import (
	"net/http"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

var _ = declareRoute(routeDecl{
	Method:      http.MethodDelete,
	Pattern:     "/api/v1/issues/{key}/subscribers/{session_id}",
	Auth:        authAny,
	Description: "Unsubscribe a session from an issue.",
	Handler:     (*server).unsubscribeIssueSession,
	WriteClass:  scopeProject,
	OAuthScopes: routeScopeSet{model.ScopePerson},
	Shape:       routeShape{Status: http.StatusNoContent},
})
