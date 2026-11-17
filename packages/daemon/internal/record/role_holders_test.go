package record

import (
	"slices"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// A role-holder publication sends the sessions of the claims that hold their role, which are the
// claims supervise.HoldsCapability answers true for. The SQL compares the states as strings
// (liveClaimStates), so nothing but this test holds the two together: a state the machine starts
// to count as live and the list leaves out would drop a running session from the set. The test
// walks supervise.States, the package's one list of every state, which supervise's own test holds
// to the states its source declares, so a state added there is a state this test walks with
// nothing added here. Each state of liveClaimStates is one of them, so a misspelt state, which the
// SQL would match on no row, fails here too.
func TestTheLiveClaimStatesAreTheStatesThatHoldTheCapability(t *testing.T) {
	states := supervise.States()
	for _, state := range states {
		if live, listed := supervise.HoldsCapability(state), slices.Contains(liveClaimStates, string(state)); live != listed {
			t.Errorf("state %s: HoldsCapability says %t, liveClaimStates says %t", state, live, listed)
		}
	}
	for _, listed := range liveClaimStates {
		if !slices.Contains(states, supervise.ClaimState(listed)) {
			t.Errorf("liveClaimStates names %q, which is none of the states supervise declares (%v)", listed, states)
		}
	}
}
