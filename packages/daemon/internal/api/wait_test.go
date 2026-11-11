package api

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// waitState is a state whose `admission.blocked` carries both kinds of row: one issue an
// unfinished Dispatch issue blocks, which every contract since 38 reads, and two the daemon
// itself holds — a member of a tree that closed and owes it a hand-over, and a root a person
// has taken the legion label off — which carry `wait` and no blocker at all.
func waitState() State {
	return State{
		Admission: Admission{Cap: 2, Active: []string{}, Waiting: []string{}, Trees: []string{},
			Blocked: []BlockedIssue{
				{Issue: "LEGION-209", BlockedBy: []string{"LEGION-300"}},
				{Issue: "LEGION-321", Wait: &record.Wait{Kind: record.WaitTreeClosed, Issue: "LEGION-320"}},
				{Issue: "LEGION-350", Wait: &record.Wait{Kind: record.WaitUnlabelled}},
			}},
		Issues: map[string]Issue{
			"LEGION-209": {Key: "LEGION-209", Phase: phase.Admitted, Status: "todo"},
			"LEGION-321": {Key: "LEGION-321", Phase: phase.Admitted, Status: "todo"},
			"LEGION-350": {Key: "LEGION-350", Phase: phase.Admitted, Status: "todo"},
		},
	}
}

// blockedServed is the `admission.blocked` of a state body, each row as it reached the wire.
func blockedServed(t *testing.T, body []byte) []map[string]json.RawMessage {
	t.Helper()
	var state struct {
		Admission struct {
			Blocked []map[string]json.RawMessage `json:"blocked"`
		} `json:"admission"`
	}
	if err := json.Unmarshal(body, &state); err != nil {
		t.Fatalf("decode state %s: %v", body, err)
	}
	return state.Admission.Blocked
}

// A plugin below waitContract is served `admission.blocked` without the rows that carry a hold
// of the daemon's own: it reads a row as one issue and the unfinished issues it waits on, so
// such a row would say the issue waits on nothing, and one built before sjawhar/legion#2788
// reads the state with a strict schema that refuses the whole body for the key. The rows a
// Dispatch link holds reach it unchanged, which is the list its own contract describes. A
// caller at this contract is served all three rows with their holds.
func TestTheHoldsAreStrippedBelowTheirContract(t *testing.T) {
	behind := blockedServed(t, stateFrom(t, waitState(), statePodAddr+":4100", podClaims(waitContract-1)))
	if len(behind) != 1 {
		t.Fatalf("the pod below %d is served %d blocked rows, want the one a Dispatch link holds", waitContract, len(behind))
	}
	if got := string(behind[0]["issue"]); got != `"LEGION-209"` {
		t.Errorf("the pod below %d is served the row of %s, want LEGION-209's", waitContract, got)
	}
	if _, carried := behind[0]["wait"]; carried {
		t.Errorf("the row served below %d carries a wait: %v", waitContract, behind[0])
	}

	at := blockedServed(t, stateFrom(t, waitState(), statePodAddr+":4100", podClaims(waitContract)))
	if len(at) != 3 {
		t.Fatalf("the pod at %d is served %d blocked rows, want all three", waitContract, len(at))
	}
	for _, row := range at[1:] {
		if _, carried := row["wait"]; !carried {
			t.Errorf("the pod at %d is served %v with no wait, want the hold that keeps it there", waitContract, row)
		}
	}
	var served record.Wait
	if err := json.Unmarshal(at[1]["wait"], &served); err != nil {
		t.Fatalf("decode the wait %s: %v", at[1]["wait"], err)
	}
	if want := (record.Wait{Kind: record.WaitTreeClosed, Issue: "LEGION-320"}); !reflect.DeepEqual(served, want) {
		t.Errorf("the wait served = %+v, want %+v", served, want)
	}
}

// The strip leaves the state the source answered whole, so the next caller reads every row.
func TestTheStateServedBelowTheWaitContractLeavesTheNextCallersStateWhole(t *testing.T) {
	handler := stateServer(t, waitState(), podClaims(waitContract-1))
	if behind := blockedServed(t, stateAsked(t, handler, statePodAddr+":41000")); len(behind) != 1 {
		t.Fatalf("the pod below %d is served %d rows, want one", waitContract, len(behind))
	}
	if operator := blockedServed(t, stateAsked(t, handler, "192.0.2.1:41000")); len(operator) != 3 {
		t.Fatalf("the caller in no pod is served %d rows, want all three", len(operator))
	}
}

// waitContract is the contract whose entry in version.go added the holds, and this daemon's own
// contract is never below it: a caller at this contract is served them.
func TestWaitContractIsTheContractThatAddedTheHolds(t *testing.T) {
	const addedWaits = 71
	if waitContract != addedWaits {
		t.Fatalf("waitContract = %d, want %d, the contract whose entry in version.go added the holds of `admission.blocked`",
			waitContract, addedWaits)
	}
	if waitContract > DaemonAPIVersion {
		t.Fatalf("waitContract = %d, above DaemonAPIVersion %d: no caller could be served the holds",
			waitContract, DaemonAPIVersion)
	}
}
