package api

import (
	"net/http"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

var _ = declareRoute(routeDecl{
	Method:      http.MethodPut,
	Pattern:     "/api/v1/issues/{key}/steps/{id}",
	Auth:        authAny,
	Description: "Write one run of one step of the issue's workflow {actor, task?, step?, kind?, role?, type?, agent?, session?, model?, generation?, started_at?, ended_at?, result?, returned_to?, report?}: the Legion daemon writes it at the run's start and at its end under one id, the uuid it derives for the run. Each field the record does not hold yet is set and every field it holds is left, but returned_to, which only the write that sets the result sets, so a start and an end land in either order and a write sent again changes nothing; a field left out or null states nothing, and so does an empty text, while returned_to 0 names place 0, the retro's. The first write of a run names its task, step, kind, role and generation. The actor must be session legion-daemon:<PROJECT> of the issue's project, or session legion-daemon:<P> of another project's daemon whose broker token names P and proves that session (403 STEP_WRITER otherwise). A bearer caller names its own session, so any caller whose credential proves no session can name the session of the issue's project's daemon. A closed issue takes a write too, and no write appends an issue event. Answers 200 with the record as it stands after the write and its attempt. A value outside its form is 400 STEP_INPUT naming the field, an id whose record is a run of another issue 409 STEP_OTHER_ISSUE, and an issue Dispatch does not know 404 ISSUE_NOT_FOUND.",
	Handler:     (*server).putIssueStep,
	WriteClass:  scopeProject,
	ID:          &idRoute{Reason: "a step record id: the uuid the Legion daemon derives for one run of one step"},
	Shape: routeShape{
		Status: http.StatusOK, Response: model.StepRecord{}, Request: stepRecordInput{},
	},
})
