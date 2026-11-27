package embedqueue

import (
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// ShortenCoverageBoundForTest points the bound Coverage holds its statement to
// (coverageStatementTimeout) at timeout until restore is called, rounded as a statement_timeout
// holds it (store.StatementTimeout: whole milliseconds, never under one) so that the message
// naming the bound names the one in force. Tests in package api shorten it to see the search
// coverage route answer a statement Postgres ended at its bound, without waiting production's ten
// seconds out; they cannot reach the unexported var. It is set and restored while nothing else
// reads it: the tests that shorten it (TestSearchCoverageRefusesAtItsBound in api,
// TestACoveragePassWhoseReportRunsPastItsBoundFailsNothing here) are serial and restore it in
// t.Cleanup, and Go starts no parallel test of a package until its serial tests have finished, so
// no parallel test that reads the coverage report runs while it is shortened.
// cmd/dispatch calls it too, only under DISPATCH_TEST_HOOKS=1 with
// DISPATCH_TEST_COVERAGE_STATEMENT_TIMEOUT set, before it serves, and never restores it.
func ShortenCoverageBoundForTest(timeout time.Duration) (restore func()) {
	healthy := coverageStatementTimeout
	coverageStatementTimeout = store.StatementTimeout(timeout)
	return func() { coverageStatementTimeout = healthy }
}

// ShortenCoverageFillTimeoutForTest points the bound of the whole fill of the coverage counts
// (coverageFillTimeout) at timeout. cmd/dispatch calls it, and only under DISPATCH_TEST_HOOKS=1
// with DISPATCH_TEST_COVERAGE_FILL_TIMEOUT set, so a test can start a server whose fill reaches
// its timeout without waiting a deployment's out; the server sets it before the counts loop
// starts.
func ShortenCoverageFillTimeoutForTest(timeout time.Duration) {
	coverageFillTimeout = timeout
}

// FailCoverageFillsForTest makes the next failures fills of the coverage counts fail before their
// first statement (errFillMadeToFail), as a fill that fails does. cmd/dispatch calls it, and only
// under DISPATCH_TEST_HOOKS=1 with DISPATCH_TEST_COVERAGE_FILL_FAILURES set, before the counts
// loop starts; a test of this package sets the count itself.
func FailCoverageFillsForTest(failures int) {
	coverageFillFailures.Store(int64(failures))
}

// PaceIntervalForTest is the interval of d's pace (Deps.pace, which Paced sets), and 0 for a d with
// no pace. Tests in package api read it to see the background drain back off from a throttled call
// past the floor it starts at; they cannot reach the unexported field.
func (d Deps) PaceIntervalForTest() time.Duration {
	if d.pace == nil {
		return 0
	}
	return d.pace.Interval()
}
