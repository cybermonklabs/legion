package agent

import (
	"testing"

	"go.uber.org/goleak"
)

// TestMain runs the package's tests and fails the run where a goroutine a test started outlives
// it (goleak): a run's goroutine, a renew or a claim that did not end with its context.
func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}
