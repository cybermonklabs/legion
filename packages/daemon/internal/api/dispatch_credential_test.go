package api

import (
	"encoding/json"
	"slices"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// dispatchCredentialServed is the `dispatchCredential` of a state body, as the plugin's reader sees
// it, and whether the key is on the wire at all.
func dispatchCredentialServed(t *testing.T, body []byte) (map[string]any, bool) {
	t.Helper()
	raw, carried := keysOf(t, body)["dispatchCredential"]
	if !carried {
		return nil, false
	}
	var credential map[string]any
	if err := json.Unmarshal(raw, &credential); err != nil {
		t.Fatalf("the state's dispatchCredential %s is not an object: %v", raw, err)
	}
	return credential, true
}

// The state route serves the daemon's Dispatch credential under dispatch_token_broker: the session
// its own calls run under and the two counts a deployment reads before it removes
// dispatch_token_transition_file, the time of the last transition launch null before the first.
func TestTheStateServesTheDaemonsDispatchCredential(t *testing.T) {
	body := stateFrom(t, populatedState(t), "192.0.2.1:41000", podClaims(DaemonAPIVersion()))
	credential, carried := dispatchCredentialServed(t, body)
	want := map[string]any{
		"session": "legion-daemon:LEGION", "transitionLaunches": float64(2),
		"lastTransitionLaunchAt": "2026-09-22T09:17:03Z", "liveLaunchesWithDispatchToken": float64(3),
	}
	if !carried || len(credential) != len(want) {
		t.Fatalf("the state %s carries dispatchCredential %v (carried %v), want %v", body, credential, carried, want)
	}
	for key, value := range want {
		if credential[key] != value {
			t.Errorf("dispatchCredential.%s = %v, want %v", key, credential[key], value)
		}
	}

	state := populatedState(t)
	none := *state.DispatchCredential
	none.TransitionLaunches, none.LastTransitionLaunchAt = 0, nil
	state.DispatchCredential = &none
	credential, _ = dispatchCredentialServed(t, stateFrom(t, state, "192.0.2.1:41000", podClaims(DaemonAPIVersion())))
	if at, written := credential["lastTransitionLaunchAt"]; !written || at != nil {
		t.Errorf("a daemon that made no transition launch writes lastTransitionLaunchAt = %v (written %v), want null", at, written)
	}

	state.DispatchCredential = nil
	if credential, carried := dispatchCredentialServed(t, stateFrom(t, state, "192.0.2.1:41000", podClaims(DaemonAPIVersion()))); carried {
		t.Errorf("a daemon without the setting serves dispatchCredential %v, want no such key", credential)
	}
}

// A plugin below dispatchCredentialContract is served no `dispatchCredential`: one built before
// sjawhar/legion#2788 reads the state with a strict schema and would refuse the whole body for a
// key it does not name, and one built after strips the key, so the strip is what keeps the object
// from a caller that did not ask for it. The route authenticates nobody, so it knows that plugin by
// the contract the request states, which every plugin from shapedAnswersContract on does, or else
// by the pod the request came from, whose claims are read only below shapedAnswersContract
// (podContracts). The daemon's log says what was dropped, named here as a literal rather than read
// from stateNarrowings, so a strip applied beside the table, which writes no line, fails here.
func TestTheDispatchCredentialIsStrippedBelowItsContract(t *testing.T) {
	state := populatedState(t)
	for _, behind := range []struct {
		name   string
		claims []supervise.Claim
		stated int
	}{
		{name: "a caller stating the contract just below", claims: podClaims(memoryContract - 1), stated: dispatchCredentialContract - 1},
		{name: "a pod whose plugin states no contract", claims: podClaims(shapedAnswersContract - 1)},
	} {
		remote := "192.0.2.1:4100"
		if behind.stated == 0 {
			remote = statePodAddr + ":4100"
		}
		body, narrowings := stateNarrowedStating(t, state, remote, behind.claims, behind.stated)
		if credential, carried := dispatchCredentialServed(t, body); carried {
			t.Errorf("%s is served dispatchCredential %v, want no such key: the object is not its contract's",
				behind.name, credential)
		}
		said := false
		for _, line := range narrowings {
			said = said || line.Dropped == "dispatchCredential"
		}
		if !said {
			t.Errorf("%s was served no line saying it dropped dispatchCredential; the daemon wrote %+v",
				behind.name, narrowings)
		}
	}

	at, narrowings := stateNarrowedStating(t, state, "192.0.2.1:4100", podClaims(memoryContract-1), dispatchCredentialContract)
	if _, carried := dispatchCredentialServed(t, at); !carried {
		t.Errorf("a caller stating %d is served no dispatchCredential: %s", dispatchCredentialContract, at)
	}
	// A caller stating dispatchCredentialContract stands below no frozen row and below each
	// entry's band above it, so the lines it is written are those bands' (narrowingsExpected),
	// whatever their number, and none is the Dispatch credential's.
	want := narrowingsExpected(state, dispatchCredentialContract)
	if slices.Contains(want, "dispatchCredential") {
		t.Fatalf("a caller stating %d expects the lines %v, want none of the Dispatch credential's", dispatchCredentialContract, want)
	}
	wantNarrowings(t, narrowings, dispatchCredentialContract, want)

	operator, _ := stateNarrowed(t, state, "192.0.2.1:4100", podClaims(memoryContract-1))
	if _, carried := dispatchCredentialServed(t, operator); !carried {
		t.Errorf("the operator's own answer, in no pod and stating no contract, is served no dispatchCredential: %s", operator)
	}
}

// dispatchCredentialContract is the contract whose entry in version.go added the key, and this
// daemon's own contract is never below it: a caller at this contract is served the object.
func TestTheDispatchCredentialContractIsTheContractThatAddedTheKey(t *testing.T) {
	const addedDispatchCredential = 275
	if dispatchCredentialContract != addedDispatchCredential {
		t.Fatalf("dispatchCredentialContract = %d, want %d, the contract whose entry in version.go added `dispatchCredential`",
			dispatchCredentialContract, addedDispatchCredential)
	}
	if dispatchCredentialContract > DaemonAPIVersion() {
		t.Fatalf("dispatchCredentialContract = %d, above DaemonAPIVersion %d: no caller could be served the key",
			dispatchCredentialContract, DaemonAPIVersion())
	}
}
