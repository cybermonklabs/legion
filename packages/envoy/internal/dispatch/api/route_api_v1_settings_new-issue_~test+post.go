package api

import (
	"net/http"
)

var _ = declareRoute(routeDecl{
	Method:      http.MethodPost,
	Pattern:     "/api/v1/settings/new-issue/_test",
	Auth:        authAny,
	Description: "Test hook: {label, session} replaces, on the process the call lands on, the new-issue label and session GET /api/v1/settings/new-issue answers from its environment; an empty value is none.",
	Handler:     (*server).setNewIssueSettings,
	TestHook:    true,
	WriteClass:  scopeDeployment,
	Shape: routeShape{
		Status: http.StatusOK, Response: newIssueSettings{}, Request: newIssueSettingsInput{},
	},
})
