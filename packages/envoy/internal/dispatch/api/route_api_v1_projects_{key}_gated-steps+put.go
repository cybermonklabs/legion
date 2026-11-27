package api

import (
	"net/http"
)

var _ = declareRoute(routeDecl{
	Method:      http.MethodPut,
	Pattern:     "/api/v1/projects/{key}/gated-steps",
	Auth:        authAny,
	Description: "Publish the design-gate steps the Legion deployment that serves this project gates {steps}: a list of criteria and spec, in the order it gates them, or [] for a deployment that gates none. The deployment's daemon writes it at every boot; a publish of the list the row already holds changes nothing and appends no event. The retired step decisions, which a page of the release before still sends, is taken and stored nowhere: the list is stored without it, and a list of decisions alone is stored as [spec]. A body without steps, or one naming anything else, is 400 INVALID_PROJECT, and an unknown project 404 PROJECT_NOT_FOUND. It declares no personal scope: the credential the deployment configured writes it, as it does the issue PATCH.",
	Handler:     (*server).putProjectGatedSteps,
	WriteClass:  scopeProject,
	Shape: routeShape{
		Status: http.StatusOK, Response: projectGatedSteps{}, Request: projectGatedStepsInput{},
	},
})
