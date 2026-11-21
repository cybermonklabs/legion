package api

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/supervise"
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

// claimWaitsServed is the `wait` of every claim view of a state body that carries one, raw, by the
// issue and role whose claim it is (claimKeyServed).
func claimWaitsServed(t *testing.T, body []byte) map[string]json.RawMessage {
	t.Helper()
	return claimKeyServed(t, body, "wait")
}

// claimKeyServed is key of every claim view of a state body that carries it, raw, by the issue
// and role whose claim it is. A view carrying no key is in no entry, because whether the key is on
// the wire at all is what the strip is read by.
func claimKeyServed(t *testing.T, body []byte, key string) map[string]json.RawMessage {
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
	for issueKey, issue := range state.Issues {
		if value, carried := issue.Architect[key]; carried {
			served[issueKey+" architect"] = value
		}
		for role, worker := range issue.Workers {
			if value, carried := worker.Claim[key]; carried {
				served[issueKey+" "+role] = value
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
// strips the key, so the strip is what keeps it from a caller that did not ask for it. A caller is
// placed by the contract it states, else by its pod's claims (contractsFor): one that states a
// contract below this one, or whose pod's claims record a contract below shapedAnswersContract,
// is served the wait on no claim, and one that states this contract, whose pod records it, or the
// operator's own `legion state` from no pod is served it on the implementer alone, in the same
// reading of the same state.
func TestTheStateServesAStandingWaitOnlyToACallerAtTheWaitContract(t *testing.T) {
	want := `{"id":"5e2b7c41-9d36-4a8f-b0c2-7e14d9a3f685","waitingOn":"the review of LEGION-207's pull request",` +
		`"wakeAt":"2026-11-14T08:00:00Z","declaredAt":"2026-11-14T06:00:00.123456Z"}`
	for _, tc := range waitBandCallers() {
		t.Run(tc.name, func(t *testing.T) {
			served := claimWaitsServed(t, stateStating(t, stateServer(t, waitState(), tc.claims), tc.remote, tc.stated))
			if !tc.served {
				if len(served) != 0 {
					t.Errorf("the caller is served the waits %v, want none: the key is not its contract's", served)
				}
				return
			}
			if len(served) != 1 {
				t.Fatalf("the state carries the waits %v, want the implementer's alone: the tester declared none", served)
			}
			if got := string(served["LEGION-208 implementer"]); got != want {
				t.Errorf("LEGION-208's implementer is served the wait %s, want %s", got, want)
			}
		})
	}
}

// waitKeys is the two keys waitContract added to each claim view, as the wire names them.
var waitKeys = []string{"wait", "hold"}

// waitBandCaller is one caller of the state that the band of waitContract places: where it asks
// from, the claims of its pod, the contract it states in its header (0 for none) and whether it
// is served the two keys of that band, `wait` and `hold`.
type waitBandCaller struct {
	name   string
	remote string
	claims []supervise.Claim
	stated int
	served bool
}

// waitBandCallers is every caller the two tests of that band read the state as. A pod's claims
// place a caller only below shapedAnswersContract (podContracts), so a caller below waitContract
// is a caller that states it, or a pod placed below that; a caller that states waitContract is
// served the keys whatever its pod records.
func waitBandCallers() []waitBandCaller {
	return []waitBandCaller{
		{name: "a caller that states the contract before this one", remote: "192.0.2.1:4100", claims: podClaims(waitContract), stated: waitContract - 1},
		{name: "a pod whose claims record a contract the pod reading places", remote: statePodAddr + ":4100", claims: podClaims(shapedAnswersContract - 1)},
		{name: "a caller that states this contract from that pod", remote: statePodAddr + ":4100", claims: podClaims(shapedAnswersContract - 1), stated: waitContract, served: true},
		{name: "a pod at the contract", remote: statePodAddr + ":4100", claims: podClaims(waitContract), served: true},
		{name: "the operator's own, from no pod", remote: "192.0.2.1:4100", claims: podClaims(waitContract), served: true},
	}
}

// waitContract is the contract whose entry in version.go added the key, and this daemon's own
// contract is never below it: a caller at this contract is served the wait.
func TestWaitContractIsTheContractThatAddedTheKey(t *testing.T) {
	const addedWait = 207
	if waitContract != addedWait {
		t.Fatalf("waitContract = %d, want %d, the contract whose entry in version.go added each claim view's wait",
			waitContract, addedWait)
	}
	if waitContract > DaemonAPIVersion {
		t.Fatalf("waitContract = %d, above DaemonAPIVersion %d: no caller could be served the key",
			waitContract, DaemonAPIVersion)
	}
}
