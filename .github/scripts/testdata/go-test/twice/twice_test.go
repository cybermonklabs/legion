package twice

import (
	"fmt"
	"testing"

	"example.com/gotestfixtures/state"
)

// The failure is printed with fmt rather than t.Fatalf, for the reason once_test.go gives.
func TestFailsEveryAttempt(t *testing.T) {
	fmt.Printf("attempt %d fails, as a broken test does\n", state.Attempt(t, "twice"))
	t.FailNow()
}
