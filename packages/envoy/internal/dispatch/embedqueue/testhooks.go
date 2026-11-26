package embedqueue

// ShortenCoverageBoundForTest points the bound Coverage holds its statement to
// (coverageStatementTimeout) at timeout, a statement_timeout value as Postgres reads it ("50ms"),
// until restore is called. Tests in package api shorten it to see the search coverage route
// answer a statement Postgres ended at its bound, without waiting production's ten seconds out;
// they cannot reach the unexported var. It is set and restored while nothing else reads it: the
// tests that shorten it (TestSearchCoverageRefusesAtItsBound in api,
// TestACoveragePassWhoseReportRunsPastItsBoundFailsNothing here) are serial and restore it in
// t.Cleanup, and Go starts no parallel test of a package until its serial tests have finished, so
// no parallel test that reads the coverage report runs while it is shortened.
func ShortenCoverageBoundForTest(timeout string) (restore func()) {
	healthy := coverageStatementTimeout
	coverageStatementTimeout = timeout
	return func() { coverageStatementTimeout = healthy }
}
