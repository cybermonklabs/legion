package api

import (
	"net/http"
)

var _ = declareRoute(routeDecl{
	Method:      http.MethodDelete,
	Pattern:     "/api/v1/projects/{key}/legion/daemon",
	Auth:        authAny,
	Description: "Stop reading this project's Legion daemon; what was already read stays. 404 NOT_FOUND when the project names none, 400 INVALID_PROJECT for a key that is not a project key and 404 PROJECT_NOT_FOUND for a project that does not exist.",
	Handler:     (*server).deleteLegionSetting,
	WriteClass:  scopeDeployment,
	Shape:       routeShape{Status: http.StatusNoContent},
})
