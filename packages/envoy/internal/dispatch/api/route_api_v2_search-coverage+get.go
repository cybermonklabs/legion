package api

import (
	"net/http"

	"github.com/sjawhar/envoy/internal/dispatch/api/apicode"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

var _ = declareRow(func(s *server) routeRow {
	return routeRow{
		Method: http.MethodGet, Pattern: "/search-coverage", Auth: authAny, Versions: onlyV2,
		Description: "Meaning search's coverage {identity, place, model, total, kinds, not_indexed}: the configured " +
			"embedder's identity, its place (bedrock, azure, or dev for a local harness) and model, all three null " +
			"with none configured; then, in total and for each kind (issue, document_block, comment_block, " +
			"message_block, ask_part), the searchable items, how many carry a current vector from that identity, how " +
			"many lack one, how many failed for good, and how many seconds the oldest lacking item has waited (null " +
			"when none lacks). not_indexed names every kind of node neither leg of search holds a row of - component, " +
			"session, document_version, result and handoff - each {kind, items}, items being how many of it the " +
			"database holds. Every number is read in one statement, so they all read one snapshot, under a 10 s " +
			"statement_timeout: a statement that runs past it counts nothing and answers 503 " +
			"SEARCH_COVERAGE_TIMEOUT, to be asked again later.",
		Handler:  s.searchCoverage,
		Resource: "search-coverage", Kind: kindReport,
		Shape:    routeShape{Status: http.StatusOK, Response: model.SearchCoverage{}},
		Refusals: []apicode.Refusal{undeclaredKeyRefusal, searchCoverageTimedOutRefusal},
	}
})
