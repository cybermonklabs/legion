package api

import (
	"net/http"
)

var _ = declareRoute(routeDecl{
	Method:      http.MethodGet,
	Pattern:     "/api/v1/projects/{key}/evals",
	Auth:        authAny,
	Description: "The project's evals, one page {items, total, limit, offset} by slug (limit 1-200, default 50), each its current definition (every field of eval_versions but charts, which the eval's own read answers, read through evals_current; answer, the short text that answers the eval's question, among them), its owner, each listed series with its statistics over the window [from, to) (RFC3339, a moment of the UTC years 1 to 9999, default the last 7 days, at most 84) and over compare_from..compare_to (both or neither, at most 84 days; the two need not be of one length), the change between them, data_since (the series' first day; a range that starts before it reads every statistic null, \"no data\", never 0), the count of rows that reached the project through a repository, and for a headline series a per-day array; and the verdict of its target (null where the target is off). Every figure is folded from the stored day rows of the UTC days the range covers, which end by 9999-12-31, through the one fold the eval's source names, or its series' agg for a logged eval; a figure past what a float holds is null; window and compare state those days, freshness the oldest row folded and the day-row refresher's age. ?kind= (delivery, legion, ci, experiment) keeps one kind; ?repo=, repeated, folds those repositories' rows in place of the project's: at most 100 repositories, a repeated one counted once, each name of at most 512 bytes. With kind=delivery, delivery_freshness is the freshness of the project's deploy sources {last_event_at, last_reconcile_at, last_error, unfetchable_count}, the delivery timeline's freshness folded over the project's sources in their order (the newest event, the oldest reconcile, null while any source never reconciled, each failing source's error prefixed with its repository) with the unfetchable pull requests that count for the project; null for any other kind or none and for a project with no deploy source. 400 INVALID_QUERY names a malformed key, a repo list or name past its bound, or a key the route does not take (any but these and the global session parameter), 404 PROJECT_NOT_FOUND an unknown project.",
	Handler:     (*server).listProjectEvals,
	Read:        readEntry{Writes: readWritesNothing},
	Shape: routeShape{
		Status: http.StatusOK, Response: evalListResponse{}, Params: evalListQuery{},
	},
})
