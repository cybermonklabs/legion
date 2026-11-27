package api

import (
	"net/http"

	"github.com/sjawhar/envoy/internal/dispatch/legion"
)

var _ = declareRoute(routeDecl{
	Method:      http.MethodPut,
	Pattern:     "/api/v1/projects/{key}/legion/daemon",
	Auth:        authAny,
	Description: "Name the Legion daemon this project's state is read from {state_url}: an absolute http(s) URL with a host and no path, else 400 LEGION_SETTING_INPUT. With NATS, a write can have Dispatch read that daemon whole. The daemon belongs to the deployment, so a project-scoped credential is refused it, the project this path names included. 400 INVALID_PROJECT for a key that is not a project key, 404 PROJECT_NOT_FOUND for a project that does not exist.",
	Handler:     (*server).putLegionSetting,
	// Which daemon a project's Legion state is read from: addressed under the project it serves,
	// and not belonging to it. Pointing Dispatch at a host is the deployment's write, so a
	// credential that names one project is refused it by name, the project its path names
	// included.
	WriteClass: scopeDeployment,
	Shape: routeShape{
		Status: http.StatusOK, Response: legion.Settings{}, Request: putLegionSettingInput{},
	},
})
