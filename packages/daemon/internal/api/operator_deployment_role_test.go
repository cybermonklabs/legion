package api

import (
	"errors"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// failedChecker is the operator's server applying facts to handler, the root LEGION-208, and
// LEGION-209 standing at step 2 of `implementer checker`, its checker's step, with the checker's
// claim failed: the runtime refused every launch of it. The server's deployment defines the
// checker (checkerDeployment), since the spawn route refuses a role no deployment defines, and it
// is given no role of a phase: it reads the role that works the issue's phase from the issue.
func failedChecker(t *testing.T, handler intake.Handler) (*harness, claim.Token) {
	t.Helper()
	h := newHarness(t)
	h.handler = NewServer("127.0.0.1", 8437, Options{
		Steps:      config.DesignGateRootIssues.Steps(),
		Supervisor: h.supervisor, BootTokens: h.tokens, Project: testProject, OperatorToken: testOperatorToken, Controller: h.store,
		Pool: h.store.Pool(), Record: record.NewStore(), Releaser: h.runtime, Trees: h.store, Handlers: []intake.Handler{handler},
		Deployment: checkerDeployment(),
	}).Handler
	h.operator(http.MethodPost, "/legion/v1/operator/claims", spawnBody())
	refused := fake.SpawnResult{Err: errors.New("the runtime refused the launch")}
	h.runtime.ScriptSpawn(refused, refused, refused)
	worker := spawnBody()
	worker.Issue, worker.Role = "LEGION-209", "checker"
	if recorder := h.operator(http.MethodPost, "/legion/v1/operator/claims", worker); recorder.Code != http.StatusInternalServerError {
		t.Fatalf("spawn of a checker whose every launch the runtime refused = %d, want 500; body %s", recorder.Code, recorder.Body)
	}
	workingIssue(t, h, phase.Step)
	seedCheckerStep(t, h, "LEGION-209")
	token, err := claim.NewToken(testProject, worker.Issue, worker.Role)
	if err != nil {
		t.Fatal(err)
	}
	return h, token
}

// The operator's retry of a failed claim of a role the deployment defines, whose issue stands at
// that role's step, starts the step again through the workflow (intake.RestartPhase), as it does a
// phase of Legion's, and answers 202 with the claim as it is until that start runs, asking the
// runtime nothing itself.
func TestTheOperatorRetriesADeploymentRolesClaim(t *testing.T) {
	facts := &phaseRestarter{}
	h, token := failedChecker(t, facts)
	calls := len(h.runtime.Calls())

	recorder := h.operator(http.MethodPost, "/legion/v1/operator/claims/"+string(token)+"/retry", nil)

	if recorder.Code != http.StatusAccepted {
		t.Fatalf("retry of a failed checker = %d, want 202; body %s", recorder.Code, recorder.Body)
	}
	want := []intake.Fact{intake.RestartPhase{Issue: "LEGION-209", Role: "checker", Claim: token}}
	if got := facts.recorded(); !reflect.DeepEqual(got, want) {
		t.Fatalf("the retry applied %#v, want %#v", got, want)
	}
	if got := h.runtime.Calls()[calls:]; len(got) != 0 {
		t.Errorf("the route asked the runtime %+v itself, want the step's start to", got)
	}
}

// A fresh session of a claim of a role the deployment defines that owes no task, whose issue stands
// at that role's step, starts the step again (intake.RestartPhase), so the claim does not come
// back in a new session with nothing to do; the controller's fresh_session answers it with no
// phase it could not restart.
func TestAFreshSessionOfADeploymentRolesClaimRestartsItsStep(t *testing.T) {
	facts := &phaseRestarter{}
	h := newHarness(t)
	h.handler = NewServer("127.0.0.1", 8437, Options{
		Supervisor: h.supervisor, BootTokens: h.tokens, Project: testProject, OperatorToken: testOperatorToken, Controller: h.store,
		Pool: h.store.Pool(), Record: record.NewStore(), Releaser: h.runtime, Trees: h.store, Handlers: []intake.Handler{facts},
	}).Handler
	token, _ := h.launchIn("LEGION-208", "LEGION-209", "checker")
	h.ready(token, "ses_checker", 1)
	workingIssue(t, h, phase.Step)
	seedCheckerStep(t, h, "LEGION-209")
	if recorder := h.operator(http.MethodPost, "/legion/v1/operator/claims/"+string(token)+"/stop", nil); recorder.Code != http.StatusOK {
		t.Fatalf("stop = %d, want 200; body %s", recorder.Code, recorder.Body)
	}
	if held := h.stored(token); supervise.HoldsTask(held.Pending) {
		t.Fatalf("the stopped checker holds a task: %+v", held.Pending)
	}
	grant := controllerGrant(t, h)

	recorder := h.request(http.MethodPost, "/legion/v1/claims/fresh-session", ClaimRequest{GrantID: grant.GrantID, Issue: "LEGION-209", Role: "checker"}, nil)

	// The controller's door answers its own success, 200 with no sentence of a restart it could not
	// run (FreshSessionResponse.PhaseNotRestarted), whatever the start of the step reports.
	if recorder.Code != http.StatusOK || strings.TrimSpace(recorder.Body.String()) != "{}" {
		t.Fatalf("the fresh session of a stopped checker = %d %s, want 200 naming no phase it could not restart", recorder.Code, recorder.Body)
	}
	applied := facts.recorded()
	if len(applied) != 1 {
		t.Fatalf("the fresh session applied %#v, want one RestartPhase", applied)
	}
	if fact, ok := applied[0].(intake.RestartPhase); !ok || fact.Issue != "LEGION-209" || fact.Role != "checker" {
		t.Fatalf("the fresh session applied %#v, want RestartPhase{LEGION-209, checker}", applied[0])
	}
}
