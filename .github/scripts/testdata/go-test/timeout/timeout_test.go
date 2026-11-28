package timeout

import (
	"testing"
	"time"

	"example.com/gotestfixtures/state"
)

// TestOutlivesItsTimeoutOnce sleeps past any -timeout the harness gives on its first attempt, so
// the binary panics, and returns at once on any later attempt.
func TestOutlivesItsTimeoutOnce(t *testing.T) {
	if state.Attempt(t, "timeout") == 1 {
		time.Sleep(time.Minute)
	}
}
