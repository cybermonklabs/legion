package embedqueue

import "time"

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

// PaceIntervalForTest is the interval of d's pace (Deps.pace, which Paced sets), and 0 for a d with
// no pace. Tests in package api read it to see the background drain back off from a throttled call
// past the floor it starts at; they cannot reach the unexported field.
func (d Deps) PaceIntervalForTest() time.Duration {
	if d.pace == nil {
		return 0
	}
	return d.pace.Interval()
}
