package api

import (
	"encoding/json"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
)

// phaseEntriesState is a state of three issues the two entry numbers answer differently:
// LEGION-20's implementer holds the work its phases row names, so the numbers are equal;
// LEGION-30's is behind the entry its row names, which is the role the daemon has still to send
// that entry's work to; and LEGION-40 is held, a phase no phase worker role works, so the
// projection writes it neither number.
func phaseEntriesState() State {
	worker := func(session string) PhaseView {
		return PhaseView{Claim: ClaimView{Session: session, State: "working"}}
	}
	return State{
		Issues: map[string]Issue{
			"LEGION-20": {Key: "LEGION-20", Phase: phase.Implementing, Status: "in_progress",
				PhaseEntry: 4, GivenEntry: 4,
				Workers: map[claim.Role]PhaseView{claim.RoleImplementer: worker("ses_implementer_20")}},
			"LEGION-30": {Key: "LEGION-30", Phase: phase.Testing, Status: "testing",
				PhaseEntry: 2, GivenEntry: 1,
				Workers: map[claim.Role]PhaseView{claim.RoleTester: worker("ses_tester_30")}},
			"LEGION-40": {Key: "LEGION-40", Phase: phase.Held, Status: "in_progress",
				Workers: map[claim.Role]PhaseView{}},
		},
	}
}

// phaseEntryField and givenEntryField are the names the two numbers carry on the wire. The tests
// of the strip read the keys by these names rather than repeating the strings, as the sibling
// contract's test reads `backwardMoves` by backwardMovesField.
const (
	phaseEntryField = "phaseEntry"
	givenEntryField = "givenEntry"
)

// phaseEntriesServed is, for every issue of a state body, whether it carries each of the two
// numbers and what each is. Presence is kept apart from value because what the route is held to
// here is whether the key is on the wire at all: an issue whose phase names no phase worker role
// carries neither key, and a zero would say the daemon holds an entry of 0 for it.
func phaseEntriesServed(t *testing.T, body []byte) map[string]struct {
	carried bool
	entry   int64
	given   int64
} {
	t.Helper()
	var state struct {
		Issues map[string]map[string]json.RawMessage `json:"issues"`
	}
	if err := json.Unmarshal(body, &state); err != nil {
		t.Fatalf("decode state %s: %v", body, err)
	}
	served := map[string]struct {
		carried bool
		entry   int64
		given   int64
	}{}
	for key, issue := range state.Issues {
		number := func(field string) int64 {
			raw, carried := issue[field]
			if !carried {
				return 0
			}
			var value int64
			if err := json.Unmarshal(raw, &value); err != nil {
				t.Fatalf("decode the %s of %s, %s: %v", field, key, raw, err)
			}
			return value
		}
		_, entryCarried := issue[phaseEntryField]
		_, givenCarried := issue[givenEntryField]
		served[key] = struct {
			carried bool
			entry   int64
			given   int64
		}{entryCarried || givenCarried, number(phaseEntryField), number(givenEntryField)}
	}
	return served
}

// The state route serves each issue the entry the role of its current phase stands on and the
// entry that role was given, and serves neither key for an issue whose phase names no phase
// worker role: the absence is how a reader tells an issue the daemon reaches from one no sweep
// of the entries passes over.
func TestTheStateRouteServesThePhaseEntryNumbers(t *testing.T) {
	body := stateFrom(t, phaseEntriesState(), "10.1.24.9:4100", podClaims(DaemonAPIVersion))
	served := phaseEntriesServed(t, body)

	for key, want := range map[string]struct{ entry, given int64 }{
		"LEGION-20": {entry: 4, given: 4},
		"LEGION-30": {entry: 2, given: 1},
	} {
		got, shown := served[key]
		if !shown {
			t.Fatalf("%s is in no issue of %s; this test proves nothing", key, body)
		}
		if !got.carried || got.entry != want.entry || got.given != want.given {
			t.Errorf("%s is served %+v, want entry %d given %d: %s", key, got, want.entry, want.given, body)
		}
	}

	held, shown := served["LEGION-40"]
	if !shown {
		t.Fatalf("LEGION-40 is in no issue of %s; this test proves nothing", body)
	}
	if held.carried {
		t.Errorf("the held issue is served %+v, want neither key: no phase worker role works held", held)
	}
}

// A plugin below phaseEntriesContract is served no issue carrying either number: one built
// before sjawhar/legion#2788 reads the state with a strict schema and would refuse the whole body
// for a key it does not name, and one built after strips the keys, so the strip is what keeps the
// numbers from a caller that did not ask for them. The route authenticates nobody, so it knows
// that plugin by the pod the request came from: a caller whose pod's claims record a contract
// below this one is served neither number, and a caller whose pod records this contract is served
// both.
func TestThePhaseEntryNumbersAreStrippedBelowTheirContract(t *testing.T) {
	state := phaseEntriesState()
	behind := stateFrom(t, state, statePodAddr+":4100", podClaims(phaseEntriesContract-1))
	for key, issue := range phaseEntriesServed(t, behind) {
		if issue.carried {
			t.Errorf("the pod below %d is served %+v for %s, want neither key: they are not its contract's",
				phaseEntriesContract, issue, key)
		}
	}

	at := stateFrom(t, state, statePodAddr+":4100", podClaims(phaseEntriesContract))
	behindRole := phaseEntriesServed(t, at)["LEGION-30"]
	if !behindRole.carried || behindRole.entry != 2 || behindRole.given != 1 {
		t.Errorf("the pod at %d is served %+v for LEGION-30, want entry 2 given 1: %s",
			phaseEntriesContract, behindRole, at)
	}
}

// phaseEntriesContract is the contract whose entry in version.go added the two numbers, and this
// daemon's own contract is never below it: a caller at this contract is served them.
func TestPhaseEntryContractIsTheContractThatAddedTheNumbers(t *testing.T) {
	const addedPhaseEntries = 96
	if phaseEntriesContract != addedPhaseEntries {
		t.Fatalf("phaseEntriesContract = %d, want %d, the contract whose entry in version.go added the issues' phase entry and given entry",
			phaseEntriesContract, addedPhaseEntries)
	}
	if phaseEntriesContract > DaemonAPIVersion {
		t.Fatalf("phaseEntriesContract = %d, above DaemonAPIVersion %d: no caller could be served the two numbers",
			phaseEntriesContract, DaemonAPIVersion)
	}
}
