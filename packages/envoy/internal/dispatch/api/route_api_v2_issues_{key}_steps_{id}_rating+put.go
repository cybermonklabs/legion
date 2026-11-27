package api

import (
	"fmt"
	"net/http"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

var _ = declareRow(func(s *server) routeRow {
	return routeRow{
		Method: http.MethodPut, Pattern: "/issues/{key}/steps/{id}/rating", Auth: authAny, Versions: onlyV2,
		Description: fmt.Sprintf("Write the rating of a retro's run {rating, rating_reason, task?, step?, kind?, "+
			"role?, type?, agent?, session?, model?, generation?, started_at?, ended_at?, result?, returned_to?, "+
			"report?}, a bearer naming its session in actor: rating, %d to %d, is how well the plan the issue ran "+
			"fit its work in the view of the agent that did the retro, and rating_reason its reason, not blank and "+
			"at most %d characters counted as Unicode code points. The Legion daemon writes it under the id of the "+
			"retro's run, with the fields PUT /api/v2/issues/{key}/steps/{id} takes, which make the record where "+
			"the retro's start and end have not landed yet; each field the record does not hold yet is set and "+
			"every field it holds is left, but returned_to, which only the write that sets the result sets, so the "+
			"rating is written once and lands before or after the retro's end. The actor must be session "+
			"legion-daemon:<PROJECT> of the issue's project, or session legion-daemon:<P> of another project's daemon "+
			"whose broker token names P and proves that session (403 STEP_WRITER otherwise). Before the airlock a "+
			"bearer caller names its own session, so any caller whose credential proves no session can name the "+
			"session of the issue's project's daemon. A closed issue takes it too, and no write appends an issue event. Answers 200 the record as it stands "+
			"after the write, "+stepRecordText+"; a body every field of which the record holds as sent answers the "+
			"record and writes nothing. A rating or reason outside its form, and a record that is not a retro's, "+
			"are 400 STEP_INPUT naming the field, an id whose record is a run of another issue 409 "+
			"STEP_OTHER_ISSUE, and a write that names no identity to an id no record has 404 STEP_NOT_FOUND; 404 "+
			"ISSUE_NOT_FOUND for an issue that does not exist.",
			contracts.PlanRatingLeast, contracts.PlanRatingMost, contracts.PlanReasonMax),
		Handler:  s.putIssueStepRatingV2,
		Resource: "rating", Kind: kindSingleton,
		Body:  requestBody{Kind: bodyJSON, Type: stepRatingInput{}},
		Shape: routeShape{Status: http.StatusOK, Response: model.StepRecord{}},
		Scope: scopeProject, ProjectOf: projectPathIssue,
		IDs:      &idRoute{Reason: "a step record id: the uuid the Legion daemon derives for the run of a retro"},
		Refusals: stepWriteRefusals(),
	}
})
