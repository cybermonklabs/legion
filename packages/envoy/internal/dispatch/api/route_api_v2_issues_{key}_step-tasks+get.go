package api

import (
	"net/http"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

var _ = declareRow(func(s *server) routeRow {
	return routeRow{
		Method: http.MethodGet, Pattern: "/issues/{key}/step-tasks", Auth: authAny, Versions: onlyV2,
		Description: "The tasks of the issue's workflow and every run of their steps, one task an item: each " +
			"{task, needs, planned?, steps}. The tasks of the issue's newest plan come first, in the plan's order, " +
			"whether a step of them ran or not, with needs and planned as GET /api/v1/issues/{key}/steps describes " +
			"them: needs the ids of the tasks of that plan the task needs ([] for a task that needs none, and null " +
			"where the plan's node recorded none), and planned each step the plan gives it, {step, name, state}: its " +
			"place from 1, its role or role:key, and where it stands among the records of the plan's own run of the " +
			"issue (its generation) - not started where none of them is a run of it, running while its newest run " +
			"there has no end, and that run's result once it has one. Then each other task with a run, in the order " +
			"of its first run, with null needs and no planned. Each task's runs come in the order they ran, each " +
			stepRecordText + ". A closed issue's read too; an issue with no plan and no run reads []. limit 1 to 200 " +
			"(default 50) and offset; {items, total, limit, offset}. 404 ISSUE_NOT_FOUND for an issue that does not exist.",
		Handler:  s.listIssueStepTasksV2,
		Resource: "step-tasks", Kind: kindCollection, Key: "{task}",
		Query:    issueStepTasksV2Query{},
		Shape:    routeShape{Status: http.StatusOK, Response: page[model.StepTask]{}},
		Refusals: listRefusals(issueMissingRefusal),
	}
})
