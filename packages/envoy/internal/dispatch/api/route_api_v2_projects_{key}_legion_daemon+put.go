package api

import (
	"net/http"

	"github.com/sjawhar/envoy/internal/dispatch/legion"
)

var _ = declareRow(func(s *server) routeRow {
	return routeRow{
		Method: http.MethodPut, Pattern: "/projects/{key}/legion/daemon", Auth: authAny,
		Description: "Name the Legion daemon this project's state is read from {state_url}: an absolute http(s) URL with a host and no path, else 400 LEGION_SETTING_INPUT. With NATS, a write can have Dispatch read that daemon whole. The daemon belongs to the deployment, so a project-scoped credential is refused it, the project this path names included. 400 INVALID_PROJECT for a key that is not a project key, 404 PROJECT_NOT_FOUND for a project that does not exist.",
		Handler:     s.putLegionSetting,
		Body:        requestBody{Kind: bodyJSON, Type: putLegionSettingInput{}},
		Shape:       routeShape{Status: http.StatusOK, Response: legion.Settings{}},
		// Which daemon a project's Legion state is read from is addressed under the project it
		// serves and does not belong to it: pointing Dispatch at a host is the deployment's
		// write, so a credential that may write one project's issues is refused it by name, the
		// project its path names included.
		Scope:      scopeDeployment,
		OfINFRA979: true,
	}
})
