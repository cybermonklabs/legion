package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

const (
	// failureHeldIssue is the held root whose implementer failed and whose tree still runs, and
	// failureLingeringIssue the held root of a tree that lingers, whose implementer an operator
	// retired after it failed. The two part on `holdReason` and agree on `heldFrom`.
	failureHeldIssue      = "LEGION-220"
	failureLingeringIssue = "LEGION-221"
)

// failedWorker drives one phase worker's claim to a failure through the supervision's own
// terminal path — its process is found gone and the runtime refuses every relaunch, until the
// launch budget runs out — and answers the claim the store holds for it. The record is the
// supervision's, not the test's: the cause, the sentence, the moment and the counts are all
// written by internal/supervise.
func failedWorker(t *testing.T, h *harness, issue, session string) supervise.Claim {
	t.Helper()
	token, _ := h.launch(issue, claim.RoleImplementer)
	h.ready(token, session, 1)
	refused := fake.SpawnResult{Err: errors.New("the runtime refused the relaunch")}
	h.runtime.ScriptResume(refused, refused)
	machine, ok := h.supervisor.Machine(token)
	if !ok {
		t.Fatalf("no machine for %s", token)
	}
	gone := runtime.Observation{Locator: *machine.Claim().Locator, Kind: runtime.Gone}
	if err := machine.Handle(h.ctx, supervise.RuntimeObservation{Observation: gone}); err == nil {
		t.Fatalf("the relaunches of %s the runtime refused answered no error", token)
	}
	failed := h.stored(token)
	if failed.State != supervise.StateFailed {
		t.Fatalf("%s is %s, want it failed: the rows below read the record its failure wrote", token, failed.State)
	}
	return failed
}

// retiredAfterFailure is failedWorker's claim stopped by the operator, which retires it. Nothing
// but a launch clears the record (supervise's launchNow), so the retired claim carries the cause
// its failure wrote, and an agent reading it can still say why the worker is gone.
func retiredAfterFailure(t *testing.T, h *harness, issue, session string) supervise.Claim {
	t.Helper()
	failed := failedWorker(t, h, issue, session)
	if recorder := h.operator(http.MethodPost, "/legion/v1/operator/claims/"+string(failed.Token)+"/stop", nil); recorder.Code != http.StatusOK {
		t.Fatalf("stop %s = %d, want 200; body %s", failed.Token, recorder.Code, recorder.Body)
	}
	retired := h.stored(failed.Token)
	if retired.State != supervise.StateRetired {
		t.Fatalf("the stopped %s is %s, want it retired", failed.Token, retired.State)
	}
	return retired
}

// failureRecordSettings is the resolved file of a daemon whose retry limits are the ones the
// harness's supervision failed these claims against, so the counts the claims carry and the
// limits the state serves are read against each other rather than against two unrelated numbers.
func failureRecordSettings(t *testing.T, limits supervise.Limits) Settings {
	t.Helper()
	path := filepath.Join(t.TempDir(), "legion.yaml")
	file := fmt.Sprintf("project: LEGION\nstate_dir: /var/lib/legion\nadmission_cap: 2\n"+
		"postgres_dsn: postgres://legion@127.0.0.1:5432/legion\n"+
		"launch_failure_limit: %d\nprompt_failure_limit: %d\nprompt_retire_limit: %d\n",
		limits.LaunchFailures, limits.PromptFailures, limits.PromptRetires)
	if err := os.WriteFile(path, []byte(file), 0o600); err != nil {
		t.Fatalf("write legion.yaml: %v", err)
	}
	cfg, err := config.Load(path, func(string) string { return "" })
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	return SettingsOf(cfg)
}

// failureRecordState is the state internal/projection builds from those two claims and their two
// held issues: the running tree's root carries both the reason and the phase it is held from, and
// the lingering tree's root carries the phase alone, which is R4's deliberate difference
// (internal/projection's own row holds the producer to it).
func failureRecordState(t *testing.T, failed, retired supervise.Claim, limits supervise.Limits) State {
	t.Helper()
	return State{
		Daemon:    DaemonInfo{Project: "LEGION", SchemaVersion: 3},
		Admission: Admission{Cap: 2, Active: []string{failureHeldIssue}, Waiting: []string{}, Trees: []string{failureHeldIssue}},
		Issues: map[string]Issue{
			failureHeldIssue: {
				Key:        failureHeldIssue,
				Generation: 1,
				Phase:      phase.Held,
				Status:     "in_progress",
				Tree:       failureHeldIssue,
				Repo:       "acme/widgets",
				HoldReason: string(record.HoldEscalated),
				HeldFrom:   string(phase.Implementing),
				Workers: map[claim.Role]PhaseView{
					claim.RoleImplementer: {Claim: ClaimView{
						Session: failed.Session,
						State:   string(failed.State),
						Failure: FailureViewOf(failed),
					}, Rounds: 1},
				},
			},
			failureLingeringIssue: {
				Key:        failureLingeringIssue,
				Generation: 1,
				Phase:      phase.Held,
				Status:     "in_progress",
				Tree:       failureLingeringIssue,
				Repo:       "acme/widgets",
				HeldFrom:   string(phase.Implementing),
				Workers: map[claim.Role]PhaseView{
					claim.RoleImplementer: {Claim: ClaimView{
						Session: retired.Session,
						State:   string(retired.State),
						Failure: FailureViewOf(retired),
					}, Rounds: 1},
				},
			},
		},
		PendingStatusWrites: []PendingStatusWrite{},
		Settings:            failureRecordSettings(t, limits),
	}
}

// servedFailureState is a state body as a plugin at the contract reads it: the two keys
// failureRecordContract added, the hold reason beside one of them, and the retry limits the
// counts are read against.
type servedFailureState struct {
	Issues map[string]struct {
		HoldReason string `json:"holdReason"`
		HeldFrom   string `json:"heldFrom"`
		Workers    map[string]struct {
			Claim struct {
				State   string            `json:"state"`
				Failure *ClaimFailureView `json:"failure"`
			} `json:"claim"`
		} `json:"workers"`
	} `json:"issues"`
	Settings struct {
		LaunchFailureLimit int `json:"launch_failure_limit"`
		PromptFailureLimit int `json:"prompt_failure_limit"`
		PromptRetireLimit  int `json:"prompt_retire_limit"`
	} `json:"settings"`
}

func decodeFailureState(t *testing.T, body []byte) servedFailureState {
	t.Helper()
	var served servedFailureState
	if err := json.Unmarshal(body, &served); err != nil {
		t.Fatalf("decode the state %s: %v", body, err)
	}
	return served
}

// A claim the daemon holds a cause for says why beside the sentence it already kept, on every
// surface an agent reads, and a held issue says which phase its hold took it out of
// (dispatch://INFRA-3083). This is the one test of that record over the routes: the rows below
// read `GET /legion/v1/state` and the operator's claim list, and the routes the rest of the
// issue adds — the architect's retry and the status route — each add a row of their own, so a
// build that serves the record on one surface and not another fails here rather than passing
// on the surface its author happened to read.
//
// The record is driven through the supervision's own terminal path rather than written by hand:
// a view built from a claim no failure wrote would pass whatever the daemon records.
func TestTheFailureRecordOverTheRoutes(t *testing.T) {
	h := newHarness(t)
	limits := h.supervisor.deps.Limits
	failed := failedWorker(t, h, failureHeldIssue, "ses_implementer_220")
	retired := retiredAfterFailure(t, h, failureLingeringIssue, "ses_implementer_221")
	state := failureRecordState(t, failed, retired, limits)

	t.Run("GET /legion/v1/state", func(t *testing.T) {
		body := stateFrom(t, state, statePodAddr+":41000", podClaims(failureRecordContract))
		served := decodeFailureState(t, body)

		held := served.Issues[failureHeldIssue]
		if held.HeldFrom != string(phase.Implementing) || held.HoldReason != string(record.HoldEscalated) {
			t.Errorf("%s is held from %q for %q, want held from %s for %s",
				failureHeldIssue, held.HeldFrom, held.HoldReason, phase.Implementing, record.HoldEscalated)
		}
		failure := held.Workers[string(claim.RoleImplementer)].Claim.Failure
		if failure == nil {
			t.Fatalf("%s's failed implementer carries no failure record: %s", failureHeldIssue, body)
		}
		if failure.Cause != supervise.CauseDied {
			t.Errorf("the cause served is %q, want %q: the process died and the relaunches the runtime refused spent the budget",
				failure.Cause, supervise.CauseDied)
		}
		if failure.Detail != failed.Failure || failure.Detail == "" {
			t.Errorf("the sentence served is %q, want the one the supervision wrote, %q", failure.Detail, failed.Failure)
		}
		if failure.At == nil || !failure.At.Equal(failed.FailedAt) || failed.FailedAt.IsZero() {
			t.Errorf("the moment served is %v, want the one the claim carries, %s", failure.At, failed.FailedAt)
		}
		wantAttempts := BudgetsView{
			LaunchFailures: failed.Budgets.LaunchFailures,
			Deaths:         failed.Budgets.Deaths,
			PromptFailures: failed.Budgets.PromptFailures,
			PromptRetires:  failed.Budgets.PromptRetires,
		}
		if failure.Attempts != wantAttempts {
			t.Errorf("the counts served are %+v, want the four the claim failed with, %+v", failure.Attempts, wantAttempts)
		}
		if failure.Attempts.LaunchFailures != limits.LaunchFailures {
			t.Errorf("the claim failed on %d launch failures, want the limit %d: the counts are read against the limits in `settings`",
				failure.Attempts.LaunchFailures, limits.LaunchFailures)
		}
		if served.Settings.LaunchFailureLimit != limits.LaunchFailures ||
			served.Settings.PromptFailureLimit != limits.PromptFailures ||
			served.Settings.PromptRetireLimit != limits.PromptRetires {
			t.Errorf("the limits served are %+v, want the ones the supervision failed the claim against, %+v",
				served.Settings, limits)
		}

		// The lingering tree's root: the phase it is held from is served, the reason is not, and
		// its implementer carries the record still although an operator retired it.
		lingering := served.Issues[failureLingeringIssue]
		if lingering.HeldFrom != string(phase.Implementing) || lingering.HoldReason != "" {
			t.Errorf("the root of the lingering tree is held from %q for %q, want held from %s for no reason served",
				lingering.HeldFrom, lingering.HoldReason, phase.Implementing)
		}
		retiredView := lingering.Workers[string(claim.RoleImplementer)].Claim
		if retiredView.State != string(supervise.StateRetired) {
			t.Fatalf("the lingering tree's implementer is %q, want it retired", retiredView.State)
		}
		// The record the retirement left untouched, read field by field: the moment crosses the
		// wire as a timestamp, which is equal to the claim's rather than identical to it.
		kept := retiredView.Failure
		if want := FailureViewOf(retired); kept == nil || kept.Cause != want.Cause || kept.Detail != want.Detail ||
			kept.Attempts != want.Attempts || kept.At == nil || !kept.At.Equal(*want.At) {
			t.Errorf("the retired claim carries %+v, want the record its failure wrote, %+v", kept, want)
		}
	})

	// The negative control, and the only proof the narrowing exists: a caller whose pod's claims
	// record one contract below the declared number reads a body with neither key, because its
	// plugin's schema names neither and a strict reader before the contract refuses the whole
	// body for one key it does not know. The caller's own recorded claim contract is what decides
	// this (podContract), never the declared constant, which is why this row stands before
	// DaemonAPIVersion reaches the number.
	t.Run("GET /legion/v1/state, a pod one contract behind", func(t *testing.T) {
		body := stateFrom(t, state, statePodAddr+":41000", podClaims(failureRecordContract-1))
		if bytes.Contains(body, []byte(`"failure"`)) || bytes.Contains(body, []byte(`"heldFrom"`)) {
			t.Fatalf("the body served to a pod one contract behind carries a key its plugin does not name: %s", body)
		}
		served := decodeFailureState(t, body)
		for key, issue := range served.Issues {
			if issue.HeldFrom != "" {
				t.Errorf("%s is served held from %q to a pod one contract behind", key, issue.HeldFrom)
			}
			for role, worker := range issue.Workers {
				if worker.Claim.Failure != nil {
					t.Errorf("%s's %s is served the failure record %+v to a pod one contract behind",
						key, role, worker.Claim.Failure)
				}
			}
		}
		// What that pod is still served: the hold's reason, which is no key of this contract, and
		// the claim states the narrowing leaves alone.
		if got := served.Issues[failureHeldIssue].HoldReason; got != string(record.HoldEscalated) {
			t.Errorf("the hold reason served to a pod one contract behind is %q, want %s: the strip takes the two keys of this contract and nothing else",
				got, record.HoldEscalated)
		}
	})

	// The operator's own list, the surface `legion claims list` reads: the record stands on
	// every claim of it, beside the budgets an operator already reads there, so the seat that
	// runs the command is told the cause the daemon chose rather than left to read it out of
	// the sentence. This route narrows by no contract — it answers an operator's token, not a
	// pod's plugin — so both claims carry the record whole, the retired one included.
	t.Run("GET /legion/v1/operator/claims", func(t *testing.T) {
		recorder := h.operator(http.MethodGet, "/legion/v1/operator/claims", nil)
		if recorder.Code != http.StatusOK {
			t.Fatalf("list the claims = %d, want 200; body %s", recorder.Code, recorder.Body)
		}
		var list OperatorClaims
		if err := json.Unmarshal(recorder.Body.Bytes(), &list); err != nil {
			t.Fatalf("decode the listing %s: %v", recorder.Body, err)
		}
		listed := make(map[claim.Token]OperatorClaim, len(list.Claims))
		for _, c := range list.Claims {
			listed[c.Token] = c
		}
		for _, held := range []supervise.Claim{failed, retired} {
			got, ok := listed[held.Token]
			if !ok {
				t.Fatalf("the listing carries no %s: %s", held.Token, recorder.Body)
			}
			// Field by field, as the state's row above: the moment crosses the wire as a
			// timestamp, which is equal to the claim's rather than identical to it.
			want := FailureViewOf(held)
			if got.Failure == nil || got.Failure.Cause != want.Cause || got.Failure.Detail != want.Detail ||
				got.Failure.Attempts != want.Attempts || got.Failure.At == nil || !got.Failure.At.Equal(*want.At) {
				t.Errorf("%s is listed with %+v, want the record its failure wrote, %+v", held.Token, got.Failure, want)
			}
			if got.Failure != nil && got.Failure.Attempts != got.Budgets {
				t.Errorf("%s is listed with the counts %+v beside the budgets %+v, want the record's counts to be the budgets it failed with",
					held.Token, got.Failure.Attempts, got.Budgets)
			}
		}
	})
}
