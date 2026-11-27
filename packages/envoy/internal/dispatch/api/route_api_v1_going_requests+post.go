package api

import (
	"fmt"
	"net/http"

	"github.com/sjawhar/envoy/internal/contracts"
)

var _ = declareRoute(routeDecl{
	Method:      http.MethodPost,
	Pattern:     "/api/v1/going/requests",
	Auth:        authAny,
	Description: fmt.Sprintf("Ask a question of what Dispatch holds {words, project, context}: records the question and answers 201 with its run - {id, author, project, words, created_at, ends_at}, whose ends_at is the %s the run has to answer in - and answers it behind that, each frame of the answer stored for GET /api/v1/going/requests/{id}/stream to carry. words is required (400 GOING_INPUT when it is blank) and at most %d characters (400 CAP_EXCEEDED); project is the project key the question is asked of, empty asking of every project (400 INVALID_PROJECT when it is not a key), and a credential scoped to one project may ask of any project or of every project. Such a credential gets 404 PROJECT_NOT_FOUND, and records nothing, for a project that does not exist and for a private project that it does not name. context is the item the bar's context chip names where the words were typed, {path, project, issue, block}, each empty where the page names none (400 INVALID_PROJECT for a project that is not a key, 400 GOING_INPUT for an issue that is not a key or a path or block past its bound); with a measured decision model configured the run first decides which of the actions the caller's credential may run the words ask for, which names the line on top of the bar, and whether the words are about the page. The answer only reads: it changes nothing, and a change is made only by a run of the bar's agent through a plan its person approves. A caller presenting a broker-issued access token under DPoP is offered no action, since the bar acts with a person's own credential and a broker-issued token is none. The run belongs to the credential that asked it, which the stream compares its reader with, so a credential that names nobody - the deployment's shared token - is refused 403 %s and records nothing: no reader of that run could ever be served it. This run is no run of the bar's agent: GET /api/v1/me/going-runs does not list it, and the record keeps it and its frames for %s after it was asked. With no model configured the run is recorded all the same and its stream carries the error frame %s, so a reader learns that from the stream, where every other failure of a run is reported, and not from a status of this route's own.", contracts.GoingRunWindow, contracts.GoingWordsMax, codeGoingAskerNamesNobody, contracts.GoingRequestTTL, contracts.GoingErrorNoModelConfigured),
	Handler:     (*server).createGoingRequest,
	// The question an answer run is recorded for belongs to the project its body names, as an
	// issue create's does. A credential scoped to one project may ask of any project but a private
	// project that it does not name, and of all projects at once (createGoingRequest makes the
	// check itself).
	WriteClass: scopeProjectInBody,
	Shape: routeShape{
		Status: http.StatusCreated, Response: goingRequest{}, Request: createGoingRequestInput{},
	},
})
