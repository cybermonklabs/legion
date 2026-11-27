package api

import (
	"net/http"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

var _ = declareRoute(routeDecl{
	Method:      http.MethodGet,
	Pattern:     "/api/v1/projects/{key}/plans",
	Auth:        authAny,
	Description: "Each finished issue of the project, one whose status is done and whose closed_at is set, newest close first, its close within [since, until) where ?since= and ?until= (RFC 3339) name either bound: {project, issues: [{issue, title, closed_at, plan, ratings, started_at, ended_at, duration_seconds, failed_steps, experiment, variant}]}. plan is the plan of the issue's newest run as the Legion daemon wrote it {id, issue, generation, source, artifact_id, version, tasks: [{id, steps, needs}]}, each task's needs null where the write recorded none, and plan null for an issue with none; ratings is the rating of each of its retros, in the order they ran, {rating, reason, step_id, ended_at}; started_at is the earliest start of its step records, ended_at the latest end and duration_seconds the whole seconds between them, each null where no record holds one, and duration_seconds null too where that end is before that start, a time Dispatch cannot count; failed_steps counts its step records of kind step that failed; experiment and variant are the experiment a draw put the issue in and the variant it drew there, as the step records of its newest run name them, each null where they name none. It writes nothing. 404 PROJECT_NOT_FOUND for a project Dispatch does not hold, 400 INVALID_QUERY for a bound that is no RFC 3339 time or a since not before its until.",
	Handler:     (*server).listProjectPlans,
	Read:        readEntry{Writes: readWritesNothing},
	Shape: routeShape{
		Status: http.StatusOK, Response: model.ProjectPlans{}, Params: projectPlansQuery{},
	},
})
