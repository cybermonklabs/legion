package api

import (
	"net/http"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

var _ = declareRow(func(s *server) routeRow {
	return routeRow{
		Method: http.MethodGet, Pattern: "/projects/{key}/plans", Auth: authAny, Versions: onlyV2,
		Description: "Each finished issue of the project, one whose status is done and whose closed_at is set, " +
			"newest close first, its close within from and to on closed_at, RFC3339, from inclusive and to exclusive " +
			"(400 INVALID_QUERY when from is not before to): each {issue, title, closed_at, plan, ratings, " +
			"started_at, ended_at, duration_seconds, failed_steps, experiment, variant}. plan is the plan of the " +
			"issue's newest run as the Legion daemon wrote it {id, issue, generation, source, artifact_id, version, " +
			"tasks: [{id, steps}]}, null for an issue with none; ratings is the rating of each of its retros, in the " +
			"order they ran, {rating, reason, step_id, ended_at}; started_at is the earliest start of its step " +
			"records, ended_at the latest end and duration_seconds the whole seconds between them, each null where " +
			"no record holds one, and duration_seconds null too where that end is before that start, a time " +
			"Dispatch cannot count; failed_steps counts its step records of kind step that failed; experiment and " +
			"variant are the experiment a draw put the issue in and the variant it drew there, as the step records " +
			"of its newest run name them, each null where they name none. It writes nothing, and a read-only token " +
			"may call it. limit 1 to 200 (default 50) and offset; {items, total, limit, offset}. 404 " +
			"PROJECT_NOT_FOUND for a project that does not exist.",
		Handler:  s.listProjectPlansV2,
		Resource: "plans", Kind: kindCollection, TimeFields: []string{"closed_at"},
		Query:    projectPlansV2Query{},
		Shape:    routeShape{Status: http.StatusOK, Response: page[model.FinishedIssue]{}},
		Refusals: listRefusals(projectMissingRefusal),
	}
})
