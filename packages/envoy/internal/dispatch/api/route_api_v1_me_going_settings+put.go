package api

import (
	"net/http"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

var _ = declareRoute(routeDecl{
	Method:      http.MethodPut,
	Pattern:     "/api/v1/me/going/settings",
	Auth:        authAny,
	Description: "Set the caller's own command-bar setting {mode} (required, ask or auto_review; 400 INVALID_STATE for anything else), answered as it now stands. A run takes the mode when it starts.",
	Handler:     (*server).putGoingSettings,
	WriteClass:  scopeSession,
	OAuthScopes: routeScopeSet{model.ScopePerson},
	Shape: routeShape{
		Status: http.StatusOK, Response: goingSettings{}, Request: goingSettingsInput{},
	},
})
