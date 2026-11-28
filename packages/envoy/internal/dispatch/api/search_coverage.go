package api

import (
	"errors"
	"net/http"

	"github.com/sjawhar/envoy/internal/dispatch/embedqueue"
)

// searchCoverage answers GET /api/v1/search/coverage: how much of what meaning search ranks has a
// current vector from the embedder search embeds its queries with, and how many items each kind
// search covers nothing of holds (embedqueue.Coverage), read in one statement from the
// counts the writes keep, so every number reads one snapshot. The identity is Deps.Embedder's, the
// client cmd/dispatch builds and never wraps for search, so it is the identity search filters
// candidates by; with no embedder configured it is empty, the report's identity, place and model
// are null and nothing counts as current.
//
// The statement runs under a statement_timeout of its own (embedqueue.CoverageStatementTimeout,
// 10 s), which ends it while the caller still waits - the coverage probe waits 30 s - and holds
// where the cancel pgx sends when a caller leaves cannot arrive: that CancelRequest needs a
// connection of its own, which a saturated server may refuse. A statement ended at that bound
// counted nothing, and the route answers it 503 SEARCH_COVERAGE_TIMEOUT. Until the counts loop's
// first fill ends (embedqueue.RunCounts) there are no counts to read, and the route answers 503
// SEARCH_COVERAGE_NOT_FILLED; with more delta rows waiting for the fold than the read reads
// (embedqueue's coverageDeltaBound) it answers 503 SEARCH_COVERAGE_FOLD_BEHIND. Each tells a caller
// to ask again later rather than read a failure; every other error answers as the handler's errors
// do.
func (s *server) searchCoverage(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuthenticated(w, r) {
		return
	}
	identity := ""
	if s.deps.Embedder != nil {
		identity = s.deps.Embedder.Identity()
	}
	report, err := embedqueue.Coverage(r.Context(), s.deps.Store, identity)
	switch {
	case errors.Is(err, embedqueue.ErrCoverageTimedOut):
		s.writeHandlerError(w, errorf(http.StatusServiceUnavailable, "SEARCH_COVERAGE_TIMEOUT",
			"the search coverage statement ran past its %s bound, so nothing was counted; try again later", embedqueue.CoverageStatementTimeout()))
	case errors.Is(err, embedqueue.ErrCoverageNotFilled):
		s.writeHandlerError(w, errorf(http.StatusServiceUnavailable, "SEARCH_COVERAGE_NOT_FILLED",
			"%s; try again later", embedqueue.ErrCoverageNotFilled))
	case errors.Is(err, embedqueue.ErrCoverageFoldBehind):
		s.writeHandlerError(w, errorf(http.StatusServiceUnavailable, "SEARCH_COVERAGE_FOLD_BEHIND",
			"%s; try again later", embedqueue.ErrCoverageFoldBehind))
	case err != nil:
		s.writeHandlerError(w, err)
	default:
		WriteJSON(w, http.StatusOK, report)
	}
}
