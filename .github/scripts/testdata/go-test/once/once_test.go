package once

import (
	"fmt"
	"testing"

	"example.com/gotestfixtures/state"
)

// The failure is printed with fmt rather than t.Fatalf, whose prefix of this file's name and the
// line of the call the captured jsonfile under .github/scripts/testdata/ci-ok-retries would carry,
// which .github/scripts/ci_citations.py reads as a citation of this file by a line number.
func TestFailsOnceThenPasses(t *testing.T) {
	if attempt := state.Attempt(t, "once"); attempt == 1 {
		fmt.Printf("attempt %d fails, as a flake does\n", attempt)
		t.FailNow()
	}
}

func TestPassesEveryTime(t *testing.T) {}
