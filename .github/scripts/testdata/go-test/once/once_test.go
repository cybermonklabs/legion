package once

import (
	"testing"

	"example.com/gotestfixtures/state"
)

func TestFailsOnceThenPasses(t *testing.T) {
	if attempt := state.Attempt(t, "once"); attempt == 1 {
		t.Fatalf("attempt %d fails, as a flake does", attempt)
	}
}

func TestPassesEveryTime(t *testing.T) {}
