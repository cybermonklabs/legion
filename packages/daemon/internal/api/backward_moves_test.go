package api

import (
	"encoding/json"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
)

// backwardMovesState is a state of two issues whose work has run rounds: LEGION-20's implementer
// has been sent back twice, by two kinds of move, and LEGION-30's has run a round no move back
// made — a conflict round, or a round from before the counting landed — so the daemon holds no
// count for it. Both carry a live claim, which is what a worker view is.
func backwardMovesState() State {
	worker := func(session string, rounds int, moves map[string]int) PhaseView {
		return PhaseView{
			Claim:         ClaimView{Session: session, State: "working"},
			Rounds:        rounds,
			BackwardMoves: moves,
		}
	}
	return State{
		Issues: map[string]Issue{
			"LEGION-20": {Key: "LEGION-20", Phase: phase.Implementing, Status: "in_progress",
				Workers: map[claim.Role]PhaseView{
					claim.RoleImplementer: worker("ses_implementer_20", 3, map[string]int{
						"reviewing-implementing": 2,
						"testing-implementing":   1,
					}),
					claim.RoleReviewer: worker("ses_reviewer_20", 0, nil),
				}},
			"LEGION-30": {Key: "LEGION-30", Phase: phase.Implementing, Status: "in_progress",
				Workers: map[claim.Role]PhaseView{
					claim.RoleImplementer: worker("ses_implementer_30", 1, nil),
				}},
		},
	}
}

// backwardMovesField is the name `backwardMoves` carries on the wire. The tests of the strip
// read the key by this name rather than repeating the string, as the state's own fields are
// written out in memory_test.go's list.
const backwardMovesField = "backwardMoves"

// backwardMovesServed is, for every worker of a state body, whether it carries `backwardMoves`
// and the counts it carries, keyed "<issue>/<role>". The key is read by name
// (backwardMovesField) and its presence is kept apart from its value, because what the route is
// held to here is whether the key is on the wire at all: an issue that has had no move back
// carries no counts, and `{}` would say the daemon holds counts that are all zero.
func backwardMovesServed(t *testing.T, body []byte) map[string]struct {
	carried bool
	counts  map[string]int
} {
	t.Helper()
	var state struct {
		Issues map[string]struct {
			Workers map[string]map[string]json.RawMessage `json:"workers"`
		} `json:"issues"`
	}
	if err := json.Unmarshal(body, &state); err != nil {
		t.Fatalf("decode state %s: %v", body, err)
	}
	served := map[string]struct {
		carried bool
		counts  map[string]int
	}{}
	for key, issue := range state.Issues {
		for role, worker := range issue.Workers {
			raw, carried := worker[backwardMovesField]
			var counts map[string]int
			if carried {
				if err := json.Unmarshal(raw, &counts); err != nil {
					t.Fatalf("decode the %s of %s's %s, %s: %v", backwardMovesField, key, role, raw, err)
				}
			}
			served[key+"/"+role] = struct {
				carried bool
				counts  map[string]int
			}{carried, counts}
		}
	}
	return served
}

// The state route serves each worker the counts of the moves back its issue has made, one count
// for each `<from>-<to>` pair the daemon counts, and serves no such key at all for a worker whose
// issue has had none: the key's absence is how a reader tells an issue that carries no counts
// from one whose counts the daemon holds, which an empty object would not say.
func TestTheStateRouteServesTheBackwardMoveCounts(t *testing.T) {
	body := stateFrom(t, backwardMovesState(), "10.1.24.9:4100", podClaims(DaemonAPIVersion))
	served := backwardMovesServed(t, body)

	counted := served["LEGION-20/implementer"]
	want := map[string]int{"reviewing-implementing": 2, "testing-implementing": 1}
	if !counted.carried || len(counted.counts) != len(want) {
		t.Fatalf("LEGION-20's implementer is served %+v, want the two pairs %v: %s", counted, want, body)
	}
	for pair, count := range want {
		if _, _, ok := phase.ParsePair(pair); !ok {
			t.Fatalf("%q is no move back the daemon counts; this test proves nothing", pair)
		}
		if got := counted.counts[pair]; got != count {
			t.Errorf("LEGION-20's implementer is served %d of %s, want %d: %s", got, pair, count, body)
		}
	}

	// The two workers the daemon holds no count for: the one whose issue has run a round no move
	// back made, and the one whose phase has run none at all. Neither is served the key, empty or
	// otherwise, so a build that writes `{}` for them fails here.
	for _, key := range []string{"LEGION-30/implementer", "LEGION-20/reviewer"} {
		worker, shown := served[key]
		if !shown {
			t.Fatalf("%s is in no worker of %s; this test proves nothing", key, body)
		}
		if worker.carried {
			t.Errorf("%s is served %s %v, want no such key: the daemon holds no count for it",
				key, backwardMovesField, worker.counts)
		}
	}
}

// A plugin below backwardMovesContract is served no worker carrying `backwardMoves`: one built
// before sjawhar/legion#2788 reads the state with a strict schema and would refuse the whole body
// for a key it does not name, and one built after strips the key, so the strip is what keeps the
// counts from a caller that did not ask for them. The route authenticates nobody, so it knows
// that plugin by the pod the request came from: a caller whose pod's claims record a contract
// below this one is served no counts, and a caller whose pod records this contract is served
// them.
func TestTheBackwardMoveCountsAreStrippedBelowTheirContract(t *testing.T) {
	state := backwardMovesState()
	behind := stateFrom(t, state, statePodAddr+":4100", podClaims(backwardMovesContract-1))
	for key, worker := range backwardMovesServed(t, behind) {
		if worker.carried {
			t.Errorf("the pod below %d is served %s %v for %s, want none: the field is not its contract's",
				backwardMovesContract, backwardMovesField, worker.counts, key)
		}
	}

	at := stateFrom(t, state, statePodAddr+":4100", podClaims(backwardMovesContract))
	if counted := backwardMovesServed(t, at)["LEGION-20/implementer"]; !counted.carried {
		t.Errorf("the pod at %d is served no %s for LEGION-20's implementer, want its counts: %s",
			backwardMovesContract, backwardMovesField, at)
	}
}

// backwardMovesContract is the contract whose entry in version.go added the field, and this
// daemon's own contract is never below it: a caller at this contract is served the counts.
func TestBackwardMovesContractIsTheContractThatAddedTheCounts(t *testing.T) {
	const addedBackwardMoves = 62
	if backwardMovesContract != addedBackwardMoves {
		t.Fatalf("backwardMovesContract = %d, want %d, the contract whose entry in version.go added the workers' counts of the moves back",
			backwardMovesContract, addedBackwardMoves)
	}
	if backwardMovesContract > DaemonAPIVersion {
		t.Fatalf("backwardMovesContract = %d, above DaemonAPIVersion %d: no caller could be served the counts",
			backwardMovesContract, DaemonAPIVersion)
	}
}
