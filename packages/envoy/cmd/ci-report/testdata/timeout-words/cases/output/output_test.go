package output

// The tests of this package quote the words that go test writes when it kills a test at its time
// limit, "panic: test timed out after", in their own output. Each test's own pass or fail is its
// outcome: the reader of the report must not read the quoted words as a timeout.

import "testing"

// TestSaysTimedOut passes after a log line that quotes the words of a timeout.
func TestSaysTimedOut(t *testing.T) {
	t.Log("a log line that quotes: panic: test timed out after 10m0s")
}

// TestFailsSayingTimedOut fails after a log line that quotes the words of a timeout.
func TestFailsSayingTimedOut(t *testing.T) {
	t.Log("want the runner to print: panic: test timed out after 1s")
	t.Error("the runner printed nothing: a real failure")
}
