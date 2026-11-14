package api

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
)

// waitState is a state whose implementer of LEGION-208 carries the wait it declared, which
// stands, beside its tester, which declared none: the projection writes `wait` on the claim whose
// wait stands and on no other (internal/projection's Project, handed the waits that stand by
// workflow.StandingWaitsNow).
func waitState() State {
	return State{
		Admission: Admission{Cap: 2, Active: []string{"LEGION-208"}, Trees: []string{"LEGION-208"}},
		Issues: map[string]Issue{
			"LEGION-208": {Key: "LEGION-208", Tree: "LEGION-208", Phase: phase.Implementing, Status: "in_progress",
				Workers: map[claim.Role]PhaseView{
					claim.RoleImplementer: {Claim: ClaimView{Session: "ses_implementer_208", State: "ready",
						Wait: &WaitView{
							ID:         "5e2b7c41-9d36-4a8f-b0c2-7e14d9a3f685",
							WaitingOn:  "the review of LEGION-207's pull request",
							WakeAt:     time.Date(2026, 11, 14, 8, 0, 0, 0, time.UTC),
							DeclaredAt: time.Date(2026, 11, 14, 6, 0, 0, 123456000, time.UTC),
						}}},
					claim.RoleTester: {Claim: ClaimView{Session: "ses_tester_208", State: "idle"}},
				}},
		},
	}
}

// waitsServed is the `wait` of every claim view of a state body that carries one, raw, by the
// issue and role whose claim it is. A view carrying no key is in no entry, because whether the key
// is on the wire at all is what the strip is read by.
func waitsServed(t *testing.T, body []byte) map[string]json.RawMessage {
	t.Helper()
	var state struct {
		Issues map[string]struct {
			Architect map[string]json.RawMessage `json:"architect"`
			Workers   map[string]struct {
				Claim map[string]json.RawMessage `json:"claim"`
			} `json:"workers"`
		} `json:"issues"`
	}
	if err := json.Unmarshal(body, &state); err != nil {
		t.Fatalf("decode state %s: %v", body, err)
	}
	served := map[string]json.RawMessage{}
	for key, issue := range state.Issues {
		if wait, carried := issue.Architect["wait"]; carried {
			served[key+" architect"] = wait
		}
		for role, worker := range issue.Workers {
			if wait, carried := worker.Claim["wait"]; carried {
				served[key+" "+role] = wait
			}
		}
	}
	return served
}

// `GET /legion/v1/state` — the body `read_record` reads an issue's record from — shows each wait
// that stands on the claim whose worker declared it: its id, what it waits on, when its time comes
// and when the daemon recorded the declaration, to the microsecond. A plugin below waitContract is
// served no claim view carrying `wait`: one built before sjawhar/legion#2788 reads the state with
// a strict schema and would refuse the whole body for a key it does not name, and one built after
// strips the key, so the strip is what keeps it from a caller that did not ask for it. The route
// authenticates nobody, so it knows that plugin by the pod the request came from: a caller whose
// pod's claims record a contract below this one is served the wait on no claim, and a caller whose
// pod records this contract, or the operator's own `legion state` from no pod, is served it on the
// implementer alone, in the same reading of the same state.
func TestTheStateServesAStandingWaitOnlyToACallerAtTheWaitContract(t *testing.T) {
	state := waitState()

	behind := stateFrom(t, state, statePodAddr+":4100", podClaims(waitContract-1))
	if served := waitsServed(t, behind); len(served) != 0 {
		t.Errorf("the pod below %d is served the waits %v, want none: the key is not its contract's", waitContract, served)
	}

	want := `{"id":"5e2b7c41-9d36-4a8f-b0c2-7e14d9a3f685","waitingOn":"the review of LEGION-207's pull request",` +
		`"wakeAt":"2026-11-14T08:00:00Z","declaredAt":"2026-11-14T06:00:00.123456Z"}`
	for name, remote := range map[string]string{
		"a pod at the contract":           statePodAddr + ":4100",
		"the operator's own, from no pod": "192.0.2.1:4100",
	} {
		t.Run(name, func(t *testing.T) {
			served := waitsServed(t, stateFrom(t, state, remote, podClaims(waitContract)))
			if len(served) != 1 {
				t.Fatalf("the state carries the waits %v, want the implementer's alone: the tester declared none", served)
			}
			if got := string(served["LEGION-208 implementer"]); got != want {
				t.Errorf("LEGION-208's implementer is served the wait %s, want %s", got, want)
			}
		})
	}
}

// waitContract is the contract whose entry in version.go added the key, and this daemon's own
// contract is never below it: a caller at this contract is served the wait.
func TestWaitContractIsTheContractThatAddedTheKey(t *testing.T) {
	const addedWait = 125
	if waitContract != addedWait {
		t.Fatalf("waitContract = %d, want %d, the contract whose entry in version.go added each claim view's wait",
			waitContract, addedWait)
	}
	if waitContract > DaemonAPIVersion {
		t.Fatalf("waitContract = %d, above DaemonAPIVersion %d: no caller could be served the key",
			waitContract, DaemonAPIVersion)
	}
}
