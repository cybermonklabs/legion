package api

import (
	"net/http"

	"github.com/sjawhar/envoy/internal/dispatch/api/apicode"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

var _ = declareRow(func(s *server) routeRow {
	return routeRow{
		Method: http.MethodPut, Pattern: "/issues/{key}/plans/{id}", Auth: authAny, Versions: onlyV2,
		Description: "Write the plan one run of the issue ran from {generation, source, artifact_id?, version?, " +
			"tasks: [{id, steps}]}, a bearer naming its session in actor: the Legion daemon writes it under the " +
			"uuid it derives for the run's plan, when the run's work starts and again at each later version of " +
			"the owner's plan that Legion takes. source is owner, with the document artifact_id and the version " +
			"of it Legion took, or default, the default steps of an issue whose owner gave no plan or whose " +
			"experiment's group takes none from its owner, with neither; each task's id is unique in the plan and " +
			"its steps are each a role or role:key, the role one a step record's role takes, since each run of the " +
			"step is a record of that role. source, artifact_id, version and tasks are one fact: a write whose " +
			"version is above the one the plan holds replaces all four, and every other write leaves them, a " +
			"default plan counting as version 0, so a write that arrives late, twice or out of order changes " +
			"nothing; the generation is the first write's. A body every member of which the plan holds as sent " +
			"answers the plan and writes nothing. The write makes the plan whatever id it names, so no plan is " +
			"missing to it. The actor " +
			"must be session legion-daemon:<PROJECT> of the issue's project, or session legion-daemon:<P> of another " +
			"project's daemon whose broker token names P and proves that session (403 STEP_WRITER otherwise). Before " +
			"the airlock a bearer caller names its own session, so any caller whose credential proves no session can " +
			"name the session of the issue's project's daemon. A closed issue takes it too, and no write appends an issue event. Answers 200 the plan " +
			"as it stands after the write {id, issue, generation, source, artifact_id, version, tasks}, artifact_id " +
			"and version null for a default plan. A value outside its form is 400 PLAN_INPUT naming the field, a " +
			"step whose role a step record refuses naming the step and its role, and an id whose plan is another " +
			"issue's 409 PLAN_OTHER_ISSUE; 404 ISSUE_NOT_FOUND for an issue that does not exist.",
		Handler:  s.putIssuePlanV2,
		Resource: "plans", Kind: kindItem, Key: "{id}",
		Body:  requestBody{Kind: bodyJSON, Type: planRecordInput{}},
		Shape: routeShape{Status: http.StatusOK, Response: model.PlanRecord{}},
		Scope: scopeProject, ProjectOf: projectPathIssue,
		IDs: &idRoute{Reason: "a plan id: the uuid the Legion daemon derives for the plan of one run of an issue"},
		Refusals: []apicode.Refusal{
			undeclaredKeyRefusal, issueMissingRefusal, stepWriterRefusal,
			{Condition: apicode.PlanInputInvalid, Code: apicode.PlanInput},
			{Condition: apicode.PlanOfAnotherIssue, Code: apicode.PlanOtherIssue},
		},
	}
})
