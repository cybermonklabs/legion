package api

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// A production check's look retries its failed checking role with one attempt left of each budget
// its failure spent (the daemon's outbox, supervise.RequestRetry.OneAttemptLeft): the half-open
// trial of a role that keeps failing. The resets a person or the project's controller makes come
// through other doors, and each gives the role its whole budget: the operator's `legion claims
// retry`, the operator's retry in a fresh session, and the controller's fresh_session each relaunch
// the tester of a closed issue, failed with its launch budget spent, at fresh budgets. A check look's
// one attempt applied to these doors would leave the role a single launch after its reset.
func TestAnOperatorsRetryAndAFreshSessionGiveAFailedCheckRoleItsWholeBudget(t *testing.T) {
	doors := append([]freshSessionDoor{{"the operator's retry", supervise.SuspendedByOperator, func(t *testing.T, h *harness, issue string, role claim.Role, _ http.Header) *httptest.ResponseRecorder {
		t.Helper()
		return h.operator(http.MethodPost, "/legion/v1/operator/claims/"+string(freshSessionToken(t, issue, role))+"/retry", nil)
	}}}, freshSessionDoors()...)
	for _, door := range doors {
		t.Run(door.name, func(t *testing.T) {
			h := freshSessionHarness(t, &phaseRestarter{})
			token, _ := h.launchIn("LEGION-208", "LEGION-209", claim.RoleTester)
			h.ready(token, "ses_tester", 1)
			parent := "LEGION-208"
			h.recordIssue(record.Issue{
				Key: "LEGION-209", Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Tree: "LEGION-208", Project: testProject,
				Title: "LEGION-209", Parent: &parent, Phase: phase.Done, Generation: 1, Status: "done",
			})
			// The death charges the first launch failure and the two relaunches the runtime refuses
			// the other two of the harness's three, so the retry's own launch is answered.
			refused := fake.SpawnResult{Err: errors.New("the pod died in workspace-init")}
			h.runtime.ScriptResume(refused, refused)
			m, _ := h.supervisor.Machine(token)
			loc := m.Claim().Locator
			if loc == nil {
				t.Fatal("the ready tester runs no process")
			}
			if err := m.Handle(h.ctx, supervise.RuntimeObservation{Observation: runtime.Observation{Locator: *loc, Kind: runtime.Gone}}); err == nil {
				t.Fatal("the relaunches the runtime refused answered no error")
			}
			if failed := h.stored(token); failed.State != supervise.StateFailed || failed.Budgets.LaunchFailures != 3 {
				t.Fatalf("the tester before the reset = %s with budgets %+v, want it failed with its launch budget of 3 spent", failed.State, failed.Budgets)
			}

			recorder := door.ask(t, h, "LEGION-209", claim.RoleTester, nil)

			if recorder.Code != http.StatusOK && recorder.Code != http.StatusAccepted {
				t.Fatalf("%s = %d, want 200 or 202; body %s", door.name, recorder.Code, recorder.Body)
			}
			if after := h.stored(token); after.State == supervise.StateFailed || after.Budgets != (supervise.Budgets{}) {
				t.Fatalf("the tester after %s = %s with budgets %+v, want it relaunched with its whole budget", door.name, after.State, after.Budgets)
			}
		})
	}
}
