package api

import (
	"net/http"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

var _ = declareRoute(routeDecl{
	Method:      http.MethodGet,
	Pattern:     "/api/v1/issues/{key}/steps",
	Auth:        authAny,
	Description: "The tasks of the issue's workflow and every run of their steps: {issue, tasks: [{task, needs, planned?, steps}]}. The tasks of the issue's newest plan come first, in the plan's order, whether a step of them ran or not: needs is the ids of the tasks of that plan the task needs, as the plan's node records them ([] for a task that needs none), and null where the node recorded none: a daemon of the release before wrote that plan, or a Dispatch of the release before took the write and dropped its needs, which no later write at the same version puts back; planned is each step the plan gives it, {step, name, state}: its place from 1, its role or role:key, and where it stands among the records of the plan's own run of the issue (its generation) - not started where none of them is a run of it, running while its newest run there has no end, and that run's result once it has one. Then each other task with a run, in the order of its first run, with null needs and no planned. The read takes the plan node and the runs alone, and reads no document. Each task's runs come in the order they ran, each step {id, issue, task, step, kind, role, type, agent, session, model, generation, attempt, started_at, ended_at, result, returned_to, report, rating, rating_reason, experiment, variant}, its attempt its place among the issue's runs of the same task, step and kind from 1, rating and rating_reason a retro's alone, and experiment and variant the experiment a draw put the issue in and the variant it drew there, each null where the run holds none. A closed issue's read too, an issue with no plan and no run reads with tasks [], and a key no issue carries is 404 NOT_FOUND, as the issue's own read answers it.",
	Handler:     (*server).listIssueSteps,
	Read:        readEntry{Writes: readWritesNothing, AsOf: asOfNoHistory},
	Shape:       routeShape{Status: http.StatusOK, Response: model.IssueSteps{}},
})
