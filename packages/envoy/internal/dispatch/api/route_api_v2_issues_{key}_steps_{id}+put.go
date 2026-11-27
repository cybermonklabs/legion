package api

import (
	"net/http"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

var _ = declareRow(func(s *server) routeRow {
	return routeRow{
		Method: http.MethodPut, Pattern: "/issues/{key}/steps/{id}", Auth: authAny, Versions: onlyV2,
		Description: "Write one run of one step of the issue's workflow {task?, step?, kind?, role?, type?, agent?, " +
			"session?, model?, generation?, started_at?, ended_at?, result?, returned_to?, report?}, a bearer naming " +
			"its session in actor: the Legion daemon writes it at the run's start and at its end under one id, the " +
			"uuid it derives for the run. Each field the record does not hold yet is set and every field it holds is " +
			"left, but returned_to, which only the write that sets the result sets, so a start and an end land in " +
			"either order; a field left out or null states nothing, and so does an empty text, while returned_to 0 " +
			"names place 0, the retro's. The first write of a run names its task, step, kind, role and generation. " +
			"The actor must be session legion-daemon:<PROJECT> of the issue's project, or session legion-daemon:<P> of " +
			"another project's daemon whose broker token names P and proves that session (403 STEP_WRITER otherwise). " +
			"Before the airlock a bearer caller names its own session, so any caller whose credential proves no session " +
			"can name the session of the issue's project's daemon. A closed issue takes a write too, and no write appends an issue event. Answers 200 the " +
			"record as it stands after the write, " + stepRecordText + "; a body every field of which the record " +
			"holds as sent answers the record and writes nothing. A value outside its form is 400 STEP_INPUT naming " +
			"the field, an id whose record is a run of another issue 409 STEP_OTHER_ISSUE, and a write that names no " +
			"identity to an id no record has 404 STEP_NOT_FOUND; 404 ISSUE_NOT_FOUND for an issue that does not exist.",
		Handler:  s.putIssueStepV2,
		Resource: "steps", Kind: kindItem, Key: "{id}",
		Body:  requestBody{Kind: bodyJSON, Type: stepRecordInput{}},
		Shape: routeShape{Status: http.StatusOK, Response: model.StepRecord{}},
		Scope: scopeProject, ProjectOf: projectPathIssue,
		IDs:      &idRoute{Reason: "a step record id: the uuid the Legion daemon derives for one run of one step"},
		Refusals: stepWriteRefusals(),
	}
})
