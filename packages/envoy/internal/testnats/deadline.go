package testnats

import (
	"testing"
	"time"
)

// deadlineMargin is what Remaining keeps back of the test binary's deadline, for the test's
// cleanup and its failure report to run before go test ends the binary.
const deadlineMargin = 10 * time.Second

// noDeadline is Remaining's answer for a binary run with no deadline (go test -timeout 0).
const noDeadline = 5 * time.Minute

// Remaining is how long a test may still wait: what the test binary's deadline (go test's
// -timeout) leaves, less deadlineMargin, or noDeadline when the binary has none. A row whose
// pass or failure is decided by an order - a request that fails at its connection's loss before
// its own deadline fires - waits for that order up to Remaining and no shorter, so a loaded or
// starved box makes it slow, never wrong.
func Remaining(t *testing.T) time.Duration {
	if deadline, ok := t.Deadline(); ok {
		return time.Until(deadline) - deadlineMargin
	}
	return noDeadline
}
