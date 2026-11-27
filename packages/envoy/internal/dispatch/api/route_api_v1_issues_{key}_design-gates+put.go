package api

import (
	"net/http"
)

var _ = declareRoute(routeDecl{
	Method:      http.MethodPut,
	Pattern:     "/api/v1/issues/{key}/design-gates",
	Auth:        authAny,
	Description: "Publish the documents this issue's design-gate steps record {gates}: each entry {step, artifact_id, holds}, step one of criteria and spec, artifact_id the document that step's gate recorded, and holds whether the deployment's gate holds the issue's tree on that step, or [] for an issue whose gates record none. An entry without holds, or with holds null, is stored as null, which the superseded_approval advice answers by the project's gated_steps. The deployment's daemon writes it and replaces the issue's whole set, appending no issue event and notifying nobody whether the set changed or not. Answers 200 {issue, gates} with the stored set in step order. An entry of the retired step decisions, which a daemon of the release before still publishes, is taken and dropped, so the issue's stored row of that step goes with the set it replaces. A body without gates, a step that is none of these, a step named twice, and an artifact_id that is empty are each 400 INVALID_ISSUE; an issue Dispatch does not know is 404 ISSUE_NOT_FOUND.",
	Handler:     (*server).putIssueDesignGates,
	WriteClass:  scopeProject,
	Shape: routeShape{
		Status: http.StatusOK, Response: issueDesignGates{}, Request: issueDesignGatesInput{},
	},
})
