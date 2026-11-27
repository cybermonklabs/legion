package api

import (
	"net/http"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

var _ = declareRoute(routeDecl{
	Method:      http.MethodGet,
	Pattern:     "/api/v1/me/going/settings",
	Auth:        authAny,
	Description: "The caller's own command-bar setting {mode}: ask, where every change of a run's plan waits for their approval, or auto_review, where a review of the run lets through the changes of fields, links and claims, and the creates with no text but a title, that their words ask for. A person who never set it reads ask.",
	Handler:     (*server).getGoingSettings,
	OAuthScopes: routeScopeSet{model.ScopePerson},
	Read:        readEntry{Writes: readWritesNothing},
	Shape:       routeShape{Status: http.StatusOK, Response: goingSettings{}},
})
