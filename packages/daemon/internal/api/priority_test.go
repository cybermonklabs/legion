package api

import (
	"testing"

	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// Each issue's own Dispatch priority is served as `priority`, its number from 0 for P0 to 3 for
// P3 or null for none, and each member of the waiting line carries the priority that placed it
// as its waitingLine's `priority` (dispatch://INFRA-3576 R1). A caller below the contract that
// added both is served neither key; a caller at this daemon's contract is served both, null
// included. The fixture's LEGION-209 is set to P0 and every other issue to none.
func TestTheStateServedBelowThePriorityContractCarriesNoPriority(t *testing.T) {
	state := populatedState(t)
	p0, none := PriorityView(record.P0), PriorityView(record.NoPriority)
	lined := ""
	for key, view := range state.Issues {
		view.Priority = &none
		if key == "LEGION-209" {
			view.Priority = &p0
		}
		if view.WaitingLine != nil {
			line := *view.WaitingLine
			line.Priority = &p0
			view.WaitingLine, lined = &line, key
		}
		state.Issues[key] = view
	}
	if lined == "" {
		t.Fatal("the populated state holds no member of the waiting line: this test proves nothing of waitingLine")
	}
	for _, tc := range []struct {
		name     string
		claims   []supervise.Claim
		stated   int
		priority bool
	}{
		{name: "a caller stating the contract below the priority", claims: podClaims(0), stated: priorityContract - 1},
		{name: "a caller stating this contract", claims: podClaims(0), stated: DaemonAPIVersion, priority: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			views := issueViews(t, stateStating(t, stateServer(t, state, tc.claims), statePodAddr+":41000", tc.stated))
			for key, view := range views {
				value, carried := view["priority"]
				if carried != tc.priority {
					t.Errorf("%s carries priority %v (%v), want it carried = %v", key, value, carried, tc.priority)
					continue
				}
				if !carried {
					continue
				}
				if want := map[bool]any{true: float64(0), false: nil}[key == "LEGION-209"]; value != want {
					t.Errorf("%s carries priority %v, want %v", key, value, want)
				}
			}
			line, _ := views[lined]["waitingLine"].(map[string]any)
			if value, carried := line["priority"]; carried != tc.priority || (carried && value != float64(0)) {
				t.Errorf("%s's waitingLine carries priority %v (%v), want it carried = %v, as 0", lined, value, carried, tc.priority)
			}
		})
	}
}

// priorityContract is the contract whose entry in version.go added the two keys, and this
// daemon's own contract is never below it.
func TestThePriorityContractIsTheContractThatAddedTheKeys(t *testing.T) {
	const addedPriority = 221
	if priorityContract != addedPriority {
		t.Fatalf("priorityContract = %d, want %d, the contract whose entry in version.go added each issue's priority", priorityContract, addedPriority)
	}
	if priorityContract > DaemonAPIVersion {
		t.Fatalf("priorityContract = %d, above DaemonAPIVersion %d: no caller would ever be served the keys", priorityContract, DaemonAPIVersion)
	}
}
