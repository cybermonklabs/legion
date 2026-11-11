package api

import (
	"encoding/json"
	"testing"
)

// claimsToRefreshState is a state of a wave in flight: the lane has claims left to relaunch and
// a wake of its own already waiting for one of them. The count and the wakes are the pair a
// reader of a running wave needs, so the fixture of this band carries both.
func claimsToRefreshState() State {
	state := State{ClaimsToRefresh: 7, WakesWaiting: []WakeWaiting{}}
	return state
}

// The state route serves the count of claims the stale-plugin lane still has to relaunch onto
// this daemon's plugin, and serves it as a number on every body: a lane with nothing left is 0,
// never a missing key, since a reader that found the key absent would have been served the
// stripped body of a caller below the contract and could tell neither case from the other.
func TestTheCountOfClaimsLeftToRefreshIsServedAsANumber(t *testing.T) {
	body := stateFrom(t, claimsToRefreshState(), "10.1.24.9:4100", podClaims(DaemonAPIVersion))
	count, carried := claimsToRefreshServed(t, body)
	if !carried || count != 7 {
		t.Fatalf("the state %s is served %s = %d (carried %v), want the lane's 7", body, ClaimsToRefreshField, count, carried)
	}

	drained := stateFrom(t, State{}, "10.1.24.9:4100", podClaims(DaemonAPIVersion))
	count, carried = claimsToRefreshServed(t, drained)
	if !carried || count != 0 {
		t.Fatalf("the drained state %s is served %s = %d (carried %v), want the key naming 0", drained, ClaimsToRefreshField, count, carried)
	}
	// Read on the wire rather than through the helper: 0 is the value, and `null` or an absent
	// key is what a strict reader of this contract refuses.
	var wire map[string]json.RawMessage
	if err := json.Unmarshal(drained, &wire); err != nil {
		t.Fatalf("decode the state %s: %v", drained, err)
	}
	if raw := string(wire[ClaimsToRefreshField]); raw != "0" {
		t.Fatalf("the drained state writes %s as %q, want the number 0", ClaimsToRefreshField, raw)
	}
}

// A plugin below claimsToRefreshContract is served no `claimsToRefresh` at all: one built
// before sjawhar/legion#2788 reads the state with a strict schema and would refuse the whole
// body for a key it does not name, and one built after strips the key, so the strip is what
// keeps the count from a caller that did not ask for it. The route authenticates nobody, so it
// knows that plugin by the pod the request came from. The key is left off rather than zeroed,
// because that reader refuses it when it is 0 as surely as when it names a number.
func TestTheCountOfClaimsLeftToRefreshIsStrippedBelowItsContract(t *testing.T) {
	state := claimsToRefreshState()
	behind := stateFrom(t, state, statePodAddr+":4100", podClaims(claimsToRefreshContract-1))
	if count, carried := claimsToRefreshServed(t, behind); carried {
		t.Errorf("the pod below %d is served %s = %d, want no such key: the count is not its contract's",
			claimsToRefreshContract, ClaimsToRefreshField, count)
	}

	drained := stateFrom(t, State{}, statePodAddr+":4100", podClaims(claimsToRefreshContract-1))
	if count, carried := claimsToRefreshServed(t, drained); carried {
		t.Errorf("the pod below %d is served %s = %d on a drained lane, want no such key: 0 is refused as surely as a number",
			claimsToRefreshContract, ClaimsToRefreshField, count)
	}

	at := stateFrom(t, state, statePodAddr+":4100", podClaims(claimsToRefreshContract))
	count, carried := claimsToRefreshServed(t, at)
	if !carried || count != state.ClaimsToRefresh {
		t.Errorf("the pod at %d is served %s = %d (carried %v), want the lane's %d: %s",
			claimsToRefreshContract, ClaimsToRefreshField, count, carried, state.ClaimsToRefresh, at)
	}
}

// The two strips compose on one body. A caller below wakesWaitingContract is below this contract
// as well, and the body it is served carries neither `wakesWaiting` nor `claimsToRefresh`: each
// key is written on every other body (State.MarshalJSON), and the reader before either contract
// refuses its key empty or zero as surely as filled, so a body that kept one of them would be
// refused whole. The band between them is read here too, because a caller served exactly one of
// the two is what a shape carrying both shadows at once would get wrong.
func TestABodyWithBothStripsCarriesNeitherKey(t *testing.T) {
	state := claimsToRefreshState()
	state.WakesWaiting = []WakeWaiting{{Claim: "legion-legion-legion-208-planner", Issue: "LEGION-208", Role: "planner", Kind: "plugin-refresh"}}

	both := stateFrom(t, state, statePodAddr+":4100", podClaims(wakesWaitingContract-1))
	if _, carried := wakesServed(t, both); carried {
		t.Errorf("the pod below %d is served the `wakesWaiting` key: %s", wakesWaitingContract, both)
	}
	if _, carried := claimsToRefreshServed(t, both); carried {
		t.Errorf("the pod below %d is served the `%s` key, and it is below %d too: %s",
			wakesWaitingContract, ClaimsToRefreshField, claimsToRefreshContract, both)
	}

	// Between the two contracts: the wakes are served and the count is not.
	between := stateFrom(t, state, statePodAddr+":4100", podClaims(claimsToRefreshContract-1))
	wakes, carried := wakesServed(t, between)
	if !carried || len(wakes) != 1 {
		t.Errorf("the pod at %d is served %d wakes (carried %v), want the one the lane holds: %s",
			claimsToRefreshContract-1, len(wakes), carried, between)
	}
	if _, carried := claimsToRefreshServed(t, between); carried {
		t.Errorf("the pod at %d is served the `%s` key, which contract %d adds: %s",
			claimsToRefreshContract-1, ClaimsToRefreshField, claimsToRefreshContract, between)
	}

	// And a caller at this contract is served both, which is what makes the two refusals above
	// the strips' doing rather than a body that never carried either key.
	at := stateFrom(t, state, statePodAddr+":4100", podClaims(DaemonAPIVersion))
	wakes, carried = wakesServed(t, at)
	count, counted := claimsToRefreshServed(t, at)
	if !carried || len(wakes) != 1 || !counted || count != state.ClaimsToRefresh {
		t.Errorf("the pod at %d is served %d wakes (carried %v) and %s = %d (carried %v), want the one wake and the lane's %d: %s",
			DaemonAPIVersion, len(wakes), carried, ClaimsToRefreshField, count, counted, state.ClaimsToRefresh, at)
	}
}

// claimsToRefreshContract is the contract whose entry in version.go added the count, and this
// daemon's own contract is never below it: a caller at this contract is served the number.
func TestClaimsToRefreshContractIsTheContractThatAddedTheCount(t *testing.T) {
	const addedClaimsToRefresh = 73
	if claimsToRefreshContract != addedClaimsToRefresh {
		t.Fatalf("claimsToRefreshContract = %d, want %d, the contract whose entry in version.go added the count of claims the stale-plugin lane has left",
			claimsToRefreshContract, addedClaimsToRefresh)
	}
	if claimsToRefreshContract > DaemonAPIVersion {
		t.Fatalf("claimsToRefreshContract = %d, above DaemonAPIVersion %d: no caller could be served the count",
			claimsToRefreshContract, DaemonAPIVersion)
	}
}
