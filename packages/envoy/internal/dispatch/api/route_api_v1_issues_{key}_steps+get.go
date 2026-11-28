package api

import (
	"net/http"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

var _ = declareRoute(routeDecl{
	Method:      http.MethodGet,
	Pattern:     "/api/v1/issues/{key}/steps",
	Auth:        authAny,
	Description: "Every run of every step of the issue's workflow, task by task in the order of each task's first run, each task's runs in the order they ran: {issue, tasks: [{task, steps}]}, each step {id, issue, task, step, kind, role, type, agent, session, model, generation, attempt, started_at, ended_at, result, returned_to, report, rating, rating_reason, experiment, variant}, its attempt its place among the issue's runs of the same task, step and kind from 1, rating and rating_reason a retro's alone, and experiment and variant the experiment a draw put the issue in and the variant it drew there, each null where the run holds none. A closed issue's read too, an issue no step ran on reads with tasks [], and a key no issue carries is 404 NOT_FOUND, as the issue's own read answers it.",
	Handler:     (*server).listIssueSteps,
	Read:        readEntry{Writes: readWritesNothing},
	Shape:       routeShape{Status: http.StatusOK, Response: model.IssueSteps{}},
})
