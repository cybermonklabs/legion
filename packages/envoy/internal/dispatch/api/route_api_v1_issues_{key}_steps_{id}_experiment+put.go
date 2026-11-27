package api

import (
	"net/http"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

var _ = declareRoute(routeDecl{
	Method:      http.MethodPut,
	Pattern:     "/api/v1/issues/{key}/steps/{id}/experiment",
	Auth:        authAny,
	Description: "Write the experiment and the variant of a run's issue {actor, experiment, variant, task?, step?, kind?, role?, type?, agent?, session?, model?, generation?, started_at?, ended_at?, result?, returned_to?, report?}: experiment is the experiment a draw put the issue in and variant the variant it drew there, each a name of 1 to 32 characters of a-z, 0-9 and -. The Legion daemon writes it under the id of the run, after the run's own write, with the fields PUT /api/v1/issues/{key}/steps/{id} takes, which make the record where the run's start and end have not landed yet; each field the record does not hold yet is set and every field it holds is left, but returned_to, which only the write that sets the result sets, so the experiment and the variant are written once. The actor must be session legion-daemon:<PROJECT> of the issue's project, or session legion-daemon:<P> of another project's daemon whose broker token names P and proves that session (403 STEP_WRITER otherwise). A bearer caller names its own session, so any caller whose credential proves no session can name the session of the issue's project's daemon. A closed issue takes it, and no write appends an issue event. Answers 200 with the record as it stands after the write and its attempt. A body naming neither, one naming one alone, and a name outside its form are 400 STEP_INPUT naming the field; an id whose record is a run of another issue is 409 STEP_OTHER_ISSUE, and an issue Dispatch does not know 404 ISSUE_NOT_FOUND.",
	Handler:     (*server).putIssueStepExperiment,
	WriteClass:  scopeProject,
	ID:          &idRoute{Reason: "a step record id: the uuid the Legion daemon derives for one run of one step"},
	Shape: routeShape{
		Status: http.StatusOK, Response: model.StepRecord{}, Request: stepExperimentInput{},
	},
})
