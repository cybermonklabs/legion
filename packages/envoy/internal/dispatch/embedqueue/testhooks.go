package embedqueue

// ShortenCoverageBoundForTest points the bound Coverage holds its statement to
// (coverageStatementTimeout) at timeout, a statement_timeout value as Postgres reads it ("50ms"),
// until restore is called. Tests in package api shorten it to see the search coverage route
// answer a statement Postgres ended at its bound, without waiting production's ten seconds out;
// they cannot reach the unexported var. It is set and restored while nothing else reads it: no
// test of either package that reads the coverage report runs in parallel.
func ShortenCoverageBoundForTest(timeout string) (restore func()) {
	healthy := coverageStatementTimeout
	coverageStatementTimeout = timeout
	return func() { coverageStatementTimeout = healthy }
}
