package api

import (
	"net/http"
)

var _ = declareRoute(routeDecl{
	Method:      http.MethodGet,
	Pattern:     "/api/v1/projects/{key}/evals",
	Auth:        authAny,
	Description: "The project's evals, one page {items, total, limit, offset} by slug (limit 1-200, default 50), each its current definition (every field of eval_versions, read through evals_current), its owner, each listed series with its statistics over the window [from, to) (RFC3339, a moment of the UTC years 1 to 9999, default the last 7 days, at most 84) and over compare_from..compare_to (both or neither, at most 84 days; the two need not be of one length), the change between them, data_since (the series' first day; a range that starts before it reads every statistic null, \"no data\", never 0), the count of rows that reached the project through a repository, and for a headline series a per-day array; and the verdict of its target (null where the target is off). Every figure is folded from the stored day rows of the UTC days the range covers, which end by 9999-12-31, through the one fold the eval's source names, or its series' agg for a logged eval; a figure past what a float holds is null; window and compare state those days, freshness the oldest row folded and the day-row refresher's age. ?kind= (delivery, legion, ci, experiment) keeps one kind; ?repo=, repeated, folds those repositories' rows in place of the project's: at most 100 repositories, a repeated one counted once, each name of at most 512 bytes. 400 INVALID_QUERY names a malformed key, a repo list or name past its bound, or a key the route does not take (any but these and the global session parameter), 404 PROJECT_NOT_FOUND an unknown project.",
	Handler:     (*server).listProjectEvals,
	Read:        readEntry{Writes: readWritesNothing},
	Shape: routeShape{
		Status: http.StatusOK, Response: evalListResponse{}, Params: evalListQuery{},
	},
})
