package api

import (
	"bytes"
	"encoding/json"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// neverReconnectedIssue is an issue whose two claims carry the failure cause the task-take
// entry (`contract_20261125171504_task-take.go`) added: its architect failed under it, the
// relaunches after a daemon start whose shim never said hello having run out, and its implementer
// is relaunching after one such process (pendingCause).
func neverReconnectedIssue() Issue {
	failedAt := time.Date(2026, 11, 17, 9, 14, 3, 0, time.UTC)
	return Issue{Key: "LEGION-214", Phase: phase.Implementing, Status: "in_progress",
		Architect: &ClaimView{Session: "ses_architect", State: string(supervise.StateFailed), PluginContract: TaskTakeContract(),
			Failure: &ClaimFailureView{Cause: supervise.CauseNeverReconnected, Detail: "launch failures ran out", At: &failedAt,
				Attempts: BudgetsView{LaunchFailures: 3}}},
		Workers: map[claim.Role]PhaseView{claim.RoleImplementer: {Claim: ClaimView{Session: "ses_implementer",
			State: string(supervise.StateQueued), PluginContract: TaskTakeContract(),
			PendingCause: supervise.CauseNeverReconnected}, Rounds: 1}}}
}

// neverReconnectedCauses is the two causes a state body serves for LEGION-214: its architect's
// `failure.cause` and its implementer's `pendingCause`.
func neverReconnectedCauses(t *testing.T, body []byte) (failure, pending string) {
	t.Helper()
	var served struct {
		Issues map[string]struct {
			Architect struct {
				Failure struct {
					Cause string `json:"cause"`
				} `json:"failure"`
			} `json:"architect"`
			Workers map[string]struct {
				Claim struct {
					PendingCause string `json:"pendingCause"`
				} `json:"claim"`
			} `json:"workers"`
		} `json:"issues"`
	}
	if err := json.Unmarshal(body, &served); err != nil {
		t.Fatalf("decode state %s: %v", body, err)
	}
	issue := served.Issues["LEGION-214"]
	return issue.Architect.Failure.Cause, issue.Workers[string(claim.RoleImplementer)].Claim.PendingCause
}

// A plugin below TaskTakeContract reads `failure.cause` and `pendingCause` through a
// strict enum of the causes it was built with, so never_reconnected, the word that entry added,
// would refuse the whole state body at it, every claim of the project included. Such a caller is
// served never_registered in its place in both keys — the nearest cause it knows, a process that
// never said hello — by the value strip of the failure cause (shaping.go's withoutNeverReconnected),
// and the daemon logs the strip; a caller at the contract is served the word, and the state the
// source answered keeps it for the next caller. The caller below is read both ways a caller is
// placed: a pod whose claims record the newest contract a pod's reading counts (podContracts
// counts none from shapedAnswersContract up), and a caller that states the contract right below
// in its header.
//
// The negative control is valueStrips without the strip's row: the caller below the contract is
// then served never_reconnected.
func TestNeverReconnectedIsServedAsNeverRegisteredBelowItsContract(t *testing.T) {
	state := narrowingState(t)
	const podBelow = shapedAnswersContract - 1

	below, narrowings := stateNarrowed(t, state, statePodAddr+":41000", podClaims(podBelow))
	stated := stateStating(t, shapedServer(t, state, podClaims(podBelow), &bytes.Buffer{}, true), "192.0.2.1:41000", TaskTakeContract()-1)
	for _, served := range []struct {
		caller string
		body   []byte
	}{{"a pod at " + strconv.Itoa(podBelow), below}, {"a caller stating " + strconv.Itoa(TaskTakeContract()-1), stated}} {
		if failure, pending := neverReconnectedCauses(t, served.body); failure != string(supervise.CauseNeverRegistered) ||
			pending != string(supervise.CauseNeverRegistered) {
			t.Errorf("%s is served failure.cause %q and pendingCause %q, want %q in both: its plugin's enum names no %q",
				served.caller, failure, pending, supervise.CauseNeverRegistered, supervise.CauseNeverReconnected)
		}
		if bytes.Contains(served.body, []byte(supervise.CauseNeverReconnected)) {
			t.Errorf("the body served to %s names %q: %s", served.caller, supervise.CauseNeverReconnected, served.body)
		}
	}
	if !slices.ContainsFunc(narrowings, func(l narrowingLine) bool {
		return l.Dropped == droppedFailureCause && l.Follows == followsRestore
	}) {
		t.Errorf("the daemon wrote %+v for a pod at %d, want a value line saying it dropped %q", narrowings, podBelow, droppedFailureCause)
	}

	at := stateStating(t, shapedServer(t, state, podClaims(podBelow), &bytes.Buffer{}, true), "192.0.2.1:41000", TaskTakeContract())
	if failure, pending := neverReconnectedCauses(t, at); failure != string(supervise.CauseNeverReconnected) ||
		pending != string(supervise.CauseNeverReconnected) {
		t.Errorf("a caller at %d is served failure.cause %q and pendingCause %q, want %q in both", TaskTakeContract(),
			failure, pending, supervise.CauseNeverReconnected)
	}
	source := state.Issues["LEGION-214"]
	if source.Architect.Failure.Cause != supervise.CauseNeverReconnected ||
		source.Workers[claim.RoleImplementer].Claim.PendingCause != supervise.CauseNeverReconnected {
		t.Errorf("the strip wrote through to the state the source answered: %+v", source)
	}
}

// The one failure-cause strip (shaping.go's withoutNeverReconnected) serves each word by its own
// arrival: a caller stating a contract at or above TaskTakeContract and below the cyber-policy
// entry admits `never_reconnected` and does not admit `cyber_policy`, so it is served
// `never_reconnected` as it stands and `cyber_policy` as `provider_error`, while a caller below
// both is served `never_registered` and `provider_error`, and a caller at the cyber-policy entry
// is served both words.
func TestFailureCauseStripServesEachWordByItsOwnContract(t *testing.T) {
	state := stateCarryingEveryDeclaredValue(t)
	issues := make(map[string]Issue, len(state.Issues)+1)
	for k, v := range state.Issues {
		issues[k] = v
	}
	issues["LEGION-214"] = neverReconnectedIssue()
	state.Issues = issues

	cyberAt := entryNumber(cyberPolicyContract)
	takeAt := TaskTakeContract()
	if !(takeAt < cyberAt) {
		t.Fatalf("task-take (%d) must sit below cyber-policy (%d) for this test to prove the step between them", takeAt, cyberAt)
	}
	srv := shapedServer(t, state, nil, &bytes.Buffer{}, true)
	whole := failureCausesServed(t, stateStating(t, srv, "192.0.2.1:41000", cyberAt))
	between := failureCausesServed(t, stateStating(t, srv, "192.0.2.1:41000", takeAt))
	below := failureCausesServed(t, stateStating(t, srv, "192.0.2.1:41000", takeAt-1))

	var sawCyber, sawNever bool
	for at, cause := range whole {
		switch cause {
		case string(supervise.CauseCyberPolicy):
			sawCyber = true
			if between[at] != string(supervise.CauseProviderError) {
				t.Errorf("caller at %d (between task-take and cyber-policy): %s = %q, want %q", takeAt, at, between[at], supervise.CauseProviderError)
			}
			if below[at] != string(supervise.CauseProviderError) {
				t.Errorf("caller at %d (below both): %s = %q, want %q", takeAt-1, at, below[at], supervise.CauseProviderError)
			}
		case string(supervise.CauseNeverReconnected):
			sawNever = true
			if between[at] != string(supervise.CauseNeverReconnected) {
				t.Errorf("caller at %d (between task-take and cyber-policy): %s = %q, want %q", takeAt, at, between[at], supervise.CauseNeverReconnected)
			}
			if below[at] != string(supervise.CauseNeverRegistered) {
				t.Errorf("caller at %d (below both): %s = %q, want %q", takeAt-1, at, below[at], supervise.CauseNeverRegistered)
			}
		default:
			if between[at] != cause || below[at] != cause {
				t.Errorf("%s = %q at %d / %q at %d, want %q unchanged", at, between[at], takeAt, below[at], takeAt-1, cause)
			}
		}
	}
	if !sawCyber || !sawNever {
		t.Fatalf("the state carried cyber_policy=%v and never_reconnected=%v, want both", sawCyber, sawNever)
	}
}
