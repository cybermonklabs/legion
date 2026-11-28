package api

import (
	"net/http"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

var _ = declareRow(func(s *server) routeRow {
	return routeRow{
		Method: http.MethodGet, Pattern: "/issues/{key}/steps", Auth: authAny, Versions: onlyV2,
		Description: "Every run of every step of the issue's workflow, as the Legion daemon wrote them, task by task - " +
			"the tasks of the issue's newest plan first, in the plan's order, then each other task with a run, in the " +
			"order of its first run - and each task's runs in the order they ran: each " + stepRecordText +
			". GET /api/v2/issues/{key}/step-tasks reads each task's needs and planned steps. A closed issue's read " +
			"too; an issue no step ran on reads []. limit 1 to 200 (default 50) and offset; " +
			"{items, total, limit, offset}. 404 ISSUE_NOT_FOUND for an issue that does not exist.",
		Handler:  s.listIssueStepsV2,
		Resource: "steps", Kind: kindCollection, Key: "{id}",
		Query:    issueStepsV2Query{},
		Shape:    routeShape{Status: http.StatusOK, Response: page[model.StepRecord]{}},
		Refusals: listRefusals(issueMissingRefusal),
	}
})
