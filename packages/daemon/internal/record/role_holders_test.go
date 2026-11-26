package record

import (
	"slices"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// A role-holder publication sends the sessions of the claims that hold their role, which are the
// claim rows supervise.HoldsRole answers true for. The SQL compares each row's state and
// suspension source as a pair of strings (roleHoldingStates, roleHoldingSources), so nothing but
// this test holds the pairs to the rule: a pair the rule holds and the lists leave out would drop
// a session from the set, and one the lists hold and the rule does not would name a session whose
// role has ended. The test walks supervise.States and supervise.SuspendSources, with the empty
// source of a claim that is not suspended, which supervise's own tests hold to the constants its
// source declares, so a state or a source added there is walked here with nothing added. Each
// listed pair is one of them, so a misspelt one, which the SQL would match on no row, fails too.
func TestTheRoleHoldingPairsAreTheRowsThatHoldTheirRole(t *testing.T) {
	states, sources := supervise.States(), append([]supervise.SuspendSource{""}, supervise.SuspendSources()...)
	listed := func(state supervise.ClaimState, by supervise.SuspendSource) bool {
		for i := range roleHoldingStates {
			if roleHoldingStates[i] == string(state) && roleHoldingSources[i] == string(by) {
				return true
			}
		}
		return false
	}
	for _, state := range states {
		for _, by := range sources {
			if holds, lists := supervise.HoldsRole(state, by), listed(state, by); holds != lists {
				t.Errorf("state %s suspended by %q: HoldsRole says %t, the role-holding pairs say %t", state, by, holds, lists)
			}
		}
	}
	if len(roleHoldingStates) != len(roleHoldingSources) {
		t.Fatalf("the role-holding pairs list %d states and %d sources, want one of each per pair", len(roleHoldingStates), len(roleHoldingSources))
	}
	for i := range roleHoldingStates {
		if !slices.Contains(states, supervise.ClaimState(roleHoldingStates[i])) || !slices.Contains(sources, supervise.SuspendSource(roleHoldingSources[i])) {
			t.Errorf("the role-holding pairs name (%q, %q), which is no state and source supervise declares", roleHoldingStates[i], roleHoldingSources[i])
		}
	}
}
