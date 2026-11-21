package agentwire

import (
	"testing"

	"go.uber.org/goleak"
)

// TestMain runs the package's tests and fails the run where a goroutine a test started outlives
// it (goleak): a connection of agent/v1 that was not closed, or a call still waiting on one.
func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}
