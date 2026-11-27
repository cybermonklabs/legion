package api

import (
	"fmt"
	"net/http"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/api/apicode"
)

var _ = declareRow(func(s *server) routeRow {
	return routeRow{
		Method: http.MethodPost, Pattern: "/going-requests", Auth: authAny, Versions: onlyV2,
		Description: fmt.Sprintf("Ask a question of what Dispatch holds {words, project}: records the question and "+
			"answers 201 with its run - {id, author, project, words, created_at, ends_at}, whose ends_at is the %s "+
			"the run has to answer in - and answers it behind that, each frame of the answer stored for GET "+
			"/api/v2/going-requests/{id}/stream to carry. words is required (400 GOING_INPUT when it is blank) and at "+
			"most %d characters (400 CAP_EXCEEDED); project is the project key the question is asked of, empty "+
			"asking of every project (400 INVALID_PROJECT when it is not a key), and a credential scoped to one "+
			"project may ask of any project or of every project. Such a credential gets 404 PROJECT_NOT_FOUND, and "+
			"records nothing, for a project that does not exist and for a private project that it does not name. The run belongs to the credential that asked it, which the stream "+
			"compares its reader with, so a credential that names nobody - the deployment's shared token - is "+
			"refused 403 %s and records nothing. With no model configured the run is recorded all the same and its "+
			"stream carries the error frame %s.", contracts.GoingRunWindow, contracts.GoingWordsMax,
			apicode.GoingAskerNamesNobody.Name(), contracts.GoingErrorNoModelConfigured),
		Handler:  s.createGoingRequestV2,
		Resource: "going-requests", Kind: kindCollection, Key: "{id}",
		// project is optional in the body, so a question may belong to no project: a collection
		// of no one project's, which stands at the top level (R1.2 of dispatch://INFRA-2946/spec).
		Body:  requestBody{Kind: bodyJSON, Type: createGoingRequestInput{}},
		Shape: routeShape{Status: http.StatusCreated, Response: goingRequest{}},
		Scope: scopeProjectInBody,
		Refusals: []apicode.Refusal{undeclaredKeyRefusal, goingAskerNamesNobodyRefusal, goingWordsBlankRefusal,
			valuePastBoundRefusal, bodyProjectMalformedRefusal,
			apicode.Refusal{Condition: apicode.GoingProjectOutOfReach, Code: apicode.ProjectNotFound}},
	}
})
