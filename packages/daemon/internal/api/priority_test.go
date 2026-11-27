package api

import (
	"testing"

	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// Each issue's own Dispatch priority is served as `priority`, its number from 0 for P0 to 3 for
// P3 or null for none, and each member of the waiting line carries the priority that placed it
// as its waitingLine's `priority`. A caller below the contract that added both is served neither
// key; a caller at this daemon's contract is served both, null included. The state is the golden
// one as populatedState writes it: LEGION-212 is P0 and LEGION-211 P1, the line's two members,
// each placed at its own; every other issue the workflow records has none; and an issue it does
// not record carries no priority at any contract.
func TestTheStateServedBelowThePriorityContractCarriesNoPriority(t *testing.T) {
	state := populatedState(t)
	own := map[string]any{"LEGION-212": float64(0), "LEGION-211": float64(1)}
	for _, tc := range []struct {
		name     string
		claims   []supervise.Claim
		stated   int
		priority bool
	}{
		{name: "a caller stating the contract below the priority", claims: podClaims(0), stated: priorityContract - 1},
		{name: "a caller stating this contract", claims: podClaims(0), stated: DaemonAPIVersion(), priority: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			views := issueViews(t, stateStating(t, stateServer(t, state, tc.claims), statePodAddr+":41000", tc.stated))
			for key, view := range views {
				recorded := state.Issues[key].Phase != Unrecorded
				value, carried := view["priority"]
				if carried != (tc.priority && recorded) {
					t.Errorf("%s carries priority %v (%v), want it carried = %v", key, value, carried, tc.priority && recorded)
					continue
				}
				if carried && value != own[key] {
					t.Errorf("%s carries priority %v, want %v", key, value, own[key])
				}
				line, inLine := view["waitingLine"].(map[string]any)
				if !inLine {
					continue
				}
				if placed, carried := line["priority"]; carried != tc.priority || (carried && placed != own[key]) {
					t.Errorf("%s's waitingLine carries priority %v (%v), want it carried = %v, as %v", key, placed, carried, tc.priority, own[key])
				}
			}
			for _, member := range []string{"LEGION-212", "LEGION-211"} {
				if _, inLine := views[member]["waitingLine"].(map[string]any); !inLine {
					t.Fatalf("%s carries no waitingLine: this test proves nothing of the line's priority", member)
				}
			}
		})
	}
}

// priorityContract is the contract whose entry in version.go added the two keys, and this
// daemon's own contract is never below it.
func TestThePriorityContractIsTheContractThatAddedTheKeys(t *testing.T) {
	const addedPriority = 250
	if priorityContract != addedPriority {
		t.Fatalf("priorityContract = %d, want %d, the contract whose entry in version.go added each issue's priority", priorityContract, addedPriority)
	}
	if priorityContract > DaemonAPIVersion() {
		t.Fatalf("priorityContract = %d, above DaemonAPIVersion %d: no caller would ever be served the keys", priorityContract, DaemonAPIVersion())
	}
}
