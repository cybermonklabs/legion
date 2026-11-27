package api

import (
	"net/http"

	"github.com/sjawhar/envoy/internal/dispatch/api/apicode"
)

var _ = declareRow(func(s *server) routeRow {
	return routeRow{
		Method: http.MethodGet, Pattern: "/related-check-stats", Auth: authAny, Versions: onlyV2,
		Description: "Count the related-item checks recorded when an issue, an ask or a new decision block was " +
			"written: those within from and to on created_at, RFC3339, from inclusive and to exclusive (400 INVALID_QUERY " +
			"when from is not before to); project=<key>, repeatable, keeps those projects' checks, with no default " +
			"project (400 INVALID_PROJECT for a value that is not a key); source_kind=issue, ask or decision_block keeps " +
			"that kind's. {project, source_kind, from, to} echo the filters (project [] and the rest null when absent), " +
			"total, by_source_kind, by_outcome (found, none, not_made), by_search (meaning_and_keywords, keywords_only, " +
			"not_made), by_reason (each reason a check was not made or searched by keywords only), completed (found or " +
			"none), skipped_for_time (reason timeout), other_project_offers, and keyword_ms and total_ms each {count, " +
			"p50, p95, max, over_300ms}: the checks holding that time, its discrete percentiles and largest value (null " +
			"with no check), and how many took over 300 ms. other_project_offers is how many ask creates of the window " +
			"offered at least one answered question of another project, a count of writes and not of items, read from " +
			"write_suggestions over the same window and filters, and zero for a source kind no such row carries; the two " +
			"records are written at different moments and only the check's own row is retried, so a count at a window's " +
			"edge can name one fewer write than the window's total.",
		Handler:  s.getRelatedCheckStatsV2,
		Resource: "related-check-stats", Kind: kindReport,
		TimeFields: []string{"created_at"}, CrossProject: true,
		Query:    relatedCheckStatsV2Query{},
		Shape:    routeShape{Status: http.StatusOK, Response: relatedCheckStatsV2{}},
		Refusals: []apicode.Refusal{undeclaredKeyRefusal, queryProjectMalformedRefusal},
	}
})
