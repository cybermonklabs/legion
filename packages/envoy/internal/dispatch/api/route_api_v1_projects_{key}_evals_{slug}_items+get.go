package api

import (
	"net/http"
)

var _ = declareRoute(routeDecl{
	Method:      http.MethodGet,
	Pattern:     "/api/v1/projects/{key}/evals/{slug}/items",
	Auth:        authAny,
	Description: "The rows behind an eval's numbers over the window and the compare range, one page {items, total, limit, offset}, every row {row_key, row_kind, cols, measured_at, source, href, links, range} whatever its kind, range compare for a row of the compare range: the eval's readings (a reading logged under a computed eval's slug among them), its stored day rows (day), and for a computed eval the rows its numbers count, as its project's day rows counted them: a Legion eval's issues closed in the range and legion-splitting's filings (issue), a delivery eval's pull requests and deploy runs (pull_request, deploy), a CI eval's runs (run) and pr-ci's jobs (job), unowned-p0's open P0 issues with no owner (issue). measured_at is when a row's value was measured, the moment its eval dates it by: a reading's own, a pull request's merge, a deploy run's and a CI run's start, a Legion issue's close, a filing's filing, a day row's day, a job's newest day, a failing job's latest failure, and the read itself for an open P0 issue with no owner. ?series= narrows to a series (a Legion series' :roots or :children cut, a filed:<class> series, a job:<name> series), ?row_kind= and ?row= to a row kind and a row key, ?repo=, repeated, to those repositories (at most 100, a repeated one counted once, each name of at most 512 bytes); ?sort=median answers pr-ci's jobs with their series' min_n timed results, slowest first, and ?sort=failures the jobs that failed in the 24 hours before the range's end. 400 INVALID_QUERY names a malformed key, a repo list or name past its bound, or a key the route does not take (any but these and the global session parameter), 404 PROJECT_NOT_FOUND and EVAL_NOT_FOUND as the eval's read answers them.",
	Handler:     (*server).listProjectEvalItems,
	Read:        readEntry{Writes: readWritesNothing},
	Shape: routeShape{
		Status: http.StatusOK, Response: evalItemsResponse{}, Params: evalItemsQuery{},
	},
})
