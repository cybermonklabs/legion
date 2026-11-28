package exitonce

import (
	"fmt"
	"os"
	"testing"

	"example.com/gotestfixtures/state"
)

// TestExitsOnce exits the test binary on its first attempt, as a log.Fatal or an os.Exit in a
// helper does: go test prints no result for it and the binary runs no test after it.
func TestExitsOnce(t *testing.T) {
	if state.Attempt(t, "exitonce") == 1 {
		fmt.Println("attempt 1 exits the test binary")
		os.Exit(1)
	}
}

// TestZFailsEveryAttempt is a real defect that the first attempt never reaches.
func TestZFailsEveryAttempt(t *testing.T) {
	state.Attempt(t, "after")
	fmt.Println("a real defect")
	t.FailNow()
}
