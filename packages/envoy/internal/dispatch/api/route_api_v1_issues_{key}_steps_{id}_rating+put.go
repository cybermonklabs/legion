package api

import (
	"fmt"
	"net/http"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

var _ = declareRoute(routeDecl{
	Method:      http.MethodPut,
	Pattern:     "/api/v1/issues/{key}/steps/{id}/rating",
	Auth:        authAny,
	Description: fmt.Sprintf("Write the rating of a retro's run {actor, rating, rating_reason, task?, step?, kind?, role?, type?, agent?, session?, model?, generation?, started_at?, ended_at?, result?, returned_to?, report?}: rating, %d to %d, is how well the plan the issue ran fit its work in the view of the agent that did the retro, and rating_reason its reason, not blank and at most %d characters counted as Unicode code points. The Legion daemon writes it under the id of the retro's run, with the fields PUT /api/v1/issues/{key}/steps/{id} takes, which make the record where the retro's start and end have not landed yet; each field the record does not hold yet is set and every field it holds is left, but returned_to, which only the write that sets the result sets, so the rating is written once and lands before or after the retro's end. The actor must be session legion-daemon:<PROJECT> of the issue's project, or session legion-daemon:<P> of another project's daemon whose broker token names P and proves that session (403 STEP_WRITER otherwise). A bearer caller names its own session, so any caller whose credential proves no session can name the session of the issue's project's daemon. A closed issue takes it, and no write appends an issue event. Answers 200 with the record as it stands after the write and its attempt. A rating or reason left out or outside its form, and a record that is not a retro's, are 400 STEP_INPUT naming the field; an id whose record is a run of another issue is 409 STEP_OTHER_ISSUE, and an issue Dispatch does not know 404 ISSUE_NOT_FOUND.", contracts.PlanRatingLeast, contracts.PlanRatingMost, contracts.PlanReasonMax),
	Handler:     (*server).putIssueStepRating,
	Successor:   successorIs(http.MethodPut, "/issues/{key}/steps/{id}/rating"),
	WriteClass:  scopeProject,
	ID:          &idRoute{Reason: "a step record id: the uuid the Legion daemon derives for the run of a retro"},
	Shape: routeShape{
		Status: http.StatusOK, Response: model.StepRecord{}, Request: stepRatingInput{},
	},
})
