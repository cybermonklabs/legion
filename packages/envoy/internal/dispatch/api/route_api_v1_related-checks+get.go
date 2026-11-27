package api

import (
	"net/http"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

var _ = declareRoute(routeDecl{
	Method:      http.MethodGet,
	Pattern:     "/api/v1/related-checks",
	Auth:        authAny,
	Description: "Count the related-item checks recorded when an issue, an ask or a new ask block was written, within [from, to) (RFC3339, each optional) and of ?project= and ?source_kind= (issue, ask or decision_block; any other is 400 INVALID_QUERY) when given: {project, source_kind, from, to} echo the filters (null when absent), total, by_source_kind, by_outcome (found, none, not_made), by_search (meaning_and_keywords, keywords_only, not_made), by_reason (each reason a check was not made or searched by keywords only), completed (found or none), skipped_for_time (reason timeout), other_project_offers, and keyword_ms and total_ms each {count, p50, p95, max, over_300ms}: the checks holding that time, its discrete percentiles and largest value (null with no check), and how many took over 300 ms. other_project_offers is how many ask creates of the window offered at least one answered question of another project (other_projects), a count of writes and not of items, read from write_suggestions over the same window and under the same filters, and zero for a source kind no such row carries; the two records are written at different moments and only the check's own row is retried, so a count at a window's edge can name one fewer write than the window's total. The meaning_ms recorded for such a check holds the whole meaning stage, the wait for the other projects' answers with it, which is at most 300 ms after the asking project's own query answered.",
	Handler:     (*server).getRelatedChecks,
	Read:        readEntry{Writes: readWritesNothing},
	Shape: routeShape{
		Status: http.StatusOK, Response: model.RelatedChecksReport{}, Params: relatedChecksQuery{},
	},
})
