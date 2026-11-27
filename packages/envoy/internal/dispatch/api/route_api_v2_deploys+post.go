package api

import (
	"net/http"

	"github.com/sjawhar/envoy/internal/dispatch/api/apicode"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

var _ = declareRow(func(s *server) routeRow {
	return routeRow{
		Method: http.MethodPost, Pattern: "/deploys", Auth: authAny, Versions: onlyV2,
		Description: "Record a program's deployed build {repo, program, commit, deployed_at?, url?}: repo is the " +
			"owner/repo the program is built from, program 1 to 64 lowercase letters, digits or hyphens, commit the " +
			"40 lowercase hexadecimal characters of the build, and deployed_at no more than five minutes ahead (the " +
			"time of the request where the body names none). Answers 201 the new deploy, its writer the credential's " +
			"own identity, and 200 the program's current deploy where the body names that build again. 400 " +
			"DEPLOY_INPUT for a value the record does not take, 400 DEPLOY_COMMIT_UNKNOWN where GitHub proves the " +
			"repository holds no such commit, and 409 DEPLOY_NOT_NEWEST for a deploy older than the program's " +
			"current one. A deploy is a deployment-wide fact, so a credential scoped to one project is refused it.",
		Handler:  s.createDeployV2,
		Resource: "deploys", Kind: kindCollection,
		Body: requestBody{Kind: bodyJSON, Type: deployInput{}},
		Shape: routeShape{
			Status: http.StatusCreated, Response: model.Deploy{},
			Also: []routeAnswer{{Status: http.StatusOK, Body: model.Deploy{}}},
		},
		Scope: scopeDeployment,
		Refusals: []apicode.Refusal{
			undeclaredKeyRefusal,
			{Condition: apicode.DeployInputInvalid, Code: apicode.DeployInput},
			{Condition: apicode.DeployCommitAbsent, Code: apicode.DeployCommitUnknown},
			{Condition: apicode.DeployOlderThanCurrent, Code: apicode.DeployNotNewest},
		},
	}
})
