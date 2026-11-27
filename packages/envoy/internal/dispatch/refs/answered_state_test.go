package refs

import (
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/asks"
)

// TestAnsweredStateIsTheAsksAnsweredState holds block_type.go's answeredState to asks.StateAnswered.
// refs cannot import asks outside its tests (a cycle through pglisten's tests), so the SQL that
// reads an ask node's answer_state names the value from its own constant, and a change of the
// asks' value that missed it would read every answered ask as open.
func TestAnsweredStateIsTheAsksAnsweredState(t *testing.T) {
	if answeredState != asks.StateAnswered {
		t.Fatalf("refs reads an answered ask's answer_state as %q, and asks writes %q", answeredState, asks.StateAnswered)
	}
}
