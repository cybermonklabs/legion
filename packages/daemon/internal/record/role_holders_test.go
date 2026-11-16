package record

import (
	"slices"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// A role-holder publication sends the sessions of the claims that hold their role, which are the
// claims supervise.HoldsCapability answers true for. The SQL compares the states as strings
// (liveClaimStates), so nothing but this test holds the two together: a state the machine starts
// to count as live and the list leaves out would drop a running session from the set.
func TestTheLiveClaimStatesAreTheStatesThatHoldTheCapability(t *testing.T) {
	for _, state := range []supervise.ClaimState{
		supervise.StateQueued, supervise.StateLaunching, supervise.StateShimConnected, supervise.StateRegistered,
		supervise.StateReady, supervise.StateWorking, supervise.StateIdle, supervise.StateSuspended,
		supervise.StateFailed, supervise.StateRetired,
	} {
		if live, listed := supervise.HoldsCapability(state), slices.Contains(liveClaimStates, string(state)); live != listed {
			t.Errorf("state %s: HoldsCapability says %t, liveClaimStates says %t", state, live, listed)
		}
	}
}
