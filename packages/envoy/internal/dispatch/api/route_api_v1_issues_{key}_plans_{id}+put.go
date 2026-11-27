package api

import (
	"net/http"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

var _ = declareRoute(routeDecl{
	Method:      http.MethodPut,
	Pattern:     "/api/v1/issues/{key}/plans/{id}",
	Auth:        authAny,
	Description: "Write the plan one run of the issue ran from {actor, generation, source, artifact_id?, version?, tasks: [{id, steps, needs?}]}: the Legion daemon writes it under the uuid it derives for the run's plan, when the run's work starts and again at each later version of the owner's plan that Legion takes. source is owner, with the document artifact_id and the version of it Legion took, or default, the default steps of an issue whose owner gave no plan or whose experiment's group takes none from its owner, with neither; each task's id is unique in the plan, its steps are each a role or role:key, the role one a step record's role takes, since each run of the step is a record of that role, and its needs are the ids of the tasks of the plan it needs, [] for none; a task written with no needs, as a daemon of the release before writes it, records them as not recorded, and the answer and the steps read give them as null. A Dispatch of the release before ignores needs, so a plan written to it also records them as not recorded. source, artifact_id, version and tasks are one fact: a write whose version is above the one the plan holds replaces all four, and every other write leaves them, a default plan counting as version 0, so a write that arrives late, twice or out of order changes nothing. The generation is the first write's. The actor must be session legion-daemon:<PROJECT> of the issue's project, or session legion-daemon:<P> of another project's daemon whose broker token names P and proves that session (403 STEP_WRITER otherwise). A bearer caller names its own session, so any caller whose credential proves no session can name the session of the issue's project's daemon. A closed issue takes it, and no write appends an issue event. Answers 200 with the plan as it stands after the write {id, issue, generation, source, artifact_id, version, tasks}. A value outside its form, a need that names no task of the plan among them, is 400 PLAN_INPUT naming the field, a step whose role a step record refuses naming the step and its role, an id whose plan is another issue's 409 PLAN_OTHER_ISSUE, and an issue Dispatch does not know 404 ISSUE_NOT_FOUND.",
	Handler:     (*server).putIssuePlan,
	Successor:   successorIs(http.MethodPut, "/issues/{key}/plans/{id}"),
	WriteClass:  scopeProject,
	ID:          &idRoute{Reason: "a plan id: the uuid the Legion daemon derives for the plan of one run of an issue"},
	Shape: routeShape{
		Status: http.StatusOK, Response: model.PlanRecord{}, Request: planRecordInput{},
	},
})
