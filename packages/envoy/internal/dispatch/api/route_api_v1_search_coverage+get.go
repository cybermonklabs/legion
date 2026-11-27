package api

import (
	"net/http"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

var _ = declareRoute(routeDecl{
	Method:      http.MethodGet,
	Pattern:     "/api/v1/search/coverage",
	Auth:        authAny,
	Description: "Meaning search's coverage {identity, place, model, total, kinds, not_indexed}: the configured embedder's identity, its place (bedrock, azure, or dev for a local harness) and model, all three null with none configured; then, in total and for each kind (issue, document_block, comment_block, message_block, ask_part), the searchable items, how many carry a current vector from that identity, how many lack one, how many failed for good, and how many seconds the oldest lacking item has waited (null when none lacks). not_indexed names every kind of node neither leg of search holds a row of - component, session, document_version, result and handoff - each {kind, items}, items being how many of it the database holds, counted from the table that holds it; such a kind is named rather than left out, so a reader never reads it as a covered kind with nothing lacking, and it is in no total. Every number is counted in one statement under a 10 s statement_timeout, and a statement that runs past it counts nothing and answers 503 SEARCH_COVERAGE_TIMEOUT, to be asked again later.",
	Handler:     (*server).searchCoverage,
	Read:        readEntry{Writes: readWritesNothing, AsOf: asOfNoHistory},
	Shape:       routeShape{Status: http.StatusOK, Response: model.SearchCoverage{}},
})
