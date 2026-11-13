package api

import (
	"encoding/json"
	"slices"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// The state the fixture builds (populatedState) holds two issues in the workflow that stand in
// neither `admission.active` nor `admission.waiting`, one of them held by issues it names and
// one held by none, so one state proves the key comes off the wire with both shapes of the row
// on it.

// heldServed is the `admission.held` of a state body — the issues it names, in the order the
// body lists them, and whether the key is on the wire at all, which an empty array is not. The
// two are read apart because the reader before the contract refuses `held` whether it names an
// issue or nothing, so an emptied key is as bad as a filled one.
func heldServed(t *testing.T, body []byte) (rows []HeldIssue, carried bool) {
	t.Helper()
	var state struct {
		Admission map[string]json.RawMessage `json:"admission"`
	}
	if err := json.Unmarshal(body, &state); err != nil {
		t.Fatalf("decode state %s: %v", body, err)
	}
	raw, carried := state.Admission["held"]
	if !carried {
		return nil, false
	}
	if err := json.Unmarshal(raw, &rows); err != nil {
		t.Fatalf("decode admission.held %s: %v", raw, err)
	}
	return rows, true
}

// The route authenticates nobody, so it knows a plugin by the pod the request came from: a pod
// whose claims record a contract below heldContract is served a state with no `admission.held`
// at all, since that pod's plugin reads the state with a strict schema that refuses the whole
// body for a key it does not name, and refuses `held` emptied as surely as filled. A pod at this
// contract, a pod whose claims record no contract at all, and a caller the daemon cannot place
// in a pod — the operator's `legion state` — are served the key with its rows. A pod whose
// claims cannot be read is no case here: the claims are the state's own snapshot (stateFor), so
// a read that fails refuses the whole request before this is asked.
func TestTheHeldListIsStrippedBelowItsContract(t *testing.T) {
	full := stateFrom(t, populatedState(t), "192.0.2.1:41000", podClaims(heldContract-1))
	wantRows, wantCarried := heldServed(t, full)
	if !wantCarried || len(wantRows) == 0 {
		t.Fatalf("the fixture's state %s holds no issue; this test proves nothing", full)
	}
	named, alone := false, false
	for _, row := range wantRows {
		if row.Reason == "" {
			t.Fatalf("the fixture's held row %+v carries no word for why; this test proves nothing", row)
		}
		named, alone = named || len(row.HeldBy) > 0, alone || len(row.HeldBy) == 0
	}
	if !named || !alone {
		t.Fatalf("the fixture's held list %+v carries no row of one of the two shapes; this test proves nothing", wantRows)
	}

	for _, tc := range []struct {
		name   string
		remote string
		claims []supervise.Claim
		held   bool
	}{
		{name: "a pod whose plugin is the contract before this one", remote: statePodAddr + ":41000", claims: podClaims(heldContract - 1)},
		{name: "a pod below PodUpdateContract, below every one of them", remote: statePodAddr + ":41000", claims: podClaims(supervise.PodUpdateContract - 1)},
		{name: "the same pod asking over IPv6", remote: "[::ffff:" + statePodAddr + "]:41000", claims: podClaims(heldContract - 1)},
		{name: "a pod at this contract", remote: statePodAddr + ":41000", claims: podClaims(heldContract), held: true},
		{name: "a pod whose claims record no contract", remote: statePodAddr + ":41000", claims: podClaims(0), held: true},
		{name: "a caller in no pod", remote: "192.0.2.1:41000", claims: podClaims(heldContract - 1), held: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := stateFrom(t, populatedState(t), tc.remote, tc.claims)
			rows, carried := heldServed(t, body)

			if !tc.held {
				if carried {
					t.Errorf("the state %s carries admission.held = %+v; the reader before heldContract refuses the key, empty as much as filled", body, rows)
				}
				return
			}
			if !carried || !slices.EqualFunc(rows, wantRows, func(got, want HeldIssue) bool {
				return got.Issue == want.Issue && got.Reason == want.Reason && slices.Equal(got.HeldBy, want.HeldBy)
			}) {
				t.Errorf("the state %s holds %+v (the key is on the wire = %v), want the fixture's %+v", body, rows, carried, wantRows)
			}
		})
	}
}

// The body a pod before the contract is served is a copy: the next caller of the same daemon, at
// this contract, is served the state as the source answered it, held list and all.
func TestTheStateServedToAPodBeforeTheHeldContractLeavesTheNextCallersStateWhole(t *testing.T) {
	handler := stateServer(t, populatedState(t), podClaims(heldContract-1))

	behind := stateAsked(t, handler, statePodAddr+":41000")
	operator := stateAsked(t, handler, "192.0.2.1:41000")

	if _, carried := heldServed(t, behind); carried {
		t.Fatalf("the pod before the contract was served %s, want no held list", behind)
	}
	if rows, carried := heldServed(t, operator); !carried || len(rows) == 0 {
		t.Fatalf("the operator was served %s, want the issues the record holds", operator)
	}
}

// heldContract is the contract whose entry in version.go added the key, and this daemon's own
// contract is never below it: a caller at this contract is served the list.
func TestTheHeldContractIsTheContractThatAddedTheKey(t *testing.T) {
	const addedHeld = 98
	if heldContract != addedHeld {
		t.Fatalf("heldContract = %d, want %d, the contract whose entry in version.go added `admission.held`",
			heldContract, addedHeld)
	}
	if heldContract > DaemonAPIVersion {
		t.Fatalf("heldContract = %d, above DaemonAPIVersion %d: no caller could be served the key",
			heldContract, DaemonAPIVersion)
	}
}
