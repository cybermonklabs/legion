// Package logquote has tests that quote go test's kill line inside a line of their log, as a test
// that logs a child's output does.
package logquote

import "testing"

func TestLogQuotesTheKillLineThenFails(t *testing.T) {
	t.Log("the child said: panic: test timed out after 1s")
	t.Fail()
}

func TestLogQuotesTheKillLineThenPasses(t *testing.T) {
	t.Log("the child said: panic: test timed out after 1s")
}

func TestSubtestLogQuotesTheKillLineThenFails(t *testing.T) {
	t.Run("quotes", func(t *testing.T) {
		t.Log("the child said: panic: test timed out after 1s")
		t.Fail()
	})
}
