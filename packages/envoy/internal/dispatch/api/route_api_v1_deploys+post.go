package api

import (
	"net/http"
)

var _ = declareRoute(routeDecl{
	Method:      http.MethodPost,
	Pattern:     "/api/v1/deploys",
	Auth:        authAny,
	Description: "Record a program's deployed build {repo, program, commit, deployed_at?, url?}; 201 carries the new deploy and its credential-resolved writer, 200 the unchanged current build, 400 DEPLOY_INPUT or DEPLOY_COMMIT_UNKNOWN, 409 DEPLOY_NOT_NEWEST. A project-scoped credential cannot write a deployment-wide fact.",
	Handler:     (*server).createDeploy,
	Successor:   successorIs(http.MethodPost, "/deploys"),
	WriteClass:  scopeDeployment,
	Shape: routeShape{
		Status: http.StatusCreated, Response: deployResponse{}, Request: deployInput{},
		Also: []routeAnswer{{Status: http.StatusOK, Body: deployResponse{}}},
	},
})
