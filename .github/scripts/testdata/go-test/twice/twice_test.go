package twice

import (
	"testing"

	"example.com/gotestfixtures/state"
)

func TestFailsEveryAttempt(t *testing.T) {
	t.Fatalf("attempt %d fails, as a broken test does", state.Attempt(t, "twice"))
}
