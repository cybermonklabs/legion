package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// heldTree is the tree readyImplementer's claim is of, whose work hold these tests close and open.
const heldTree = "LEGION-208"

// closeWorkHold is the close of heldTree's standing design gate under `gates.hold: work`, as the
// store keeps it: the tree's work_holds row, which the machine and the agent door read
// (supervise.Store.WorkHoldStands), over the tree's root issue, which the row names.
func closeWorkHold(t *testing.T, h *harness) {
	t.Helper()
	h.recordIssue(record.Issue{Key: heldTree, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets",
		Tree: heldTree, Project: testProject, Title: heldTree, Phase: phase.Implementing, Generation: 1, Status: "in_progress"})
	hold := record.WorkHold{Project: testProject, Tree: heldTree, RootGeneration: 1, Step: record.DesignStepSpec,
		ArtifactID: "artifact-spec", Version: 4, ClosedAt: time.Now().UTC()}
	if err := store.Transact(h.ctx, h.store.Pool(), func(ctx context.Context, tx pgx.Tx) error {
		return record.NewStore().PutWorkHold(ctx, tx, hold)
	}); err != nil {
		t.Fatalf("close the work hold of %s: %v", heldTree, err)
	}
}

// openWorkHold is the opening of heldTree's gate as the machine reads it: the tree's work_holds
// row is gone.
func openWorkHold(t *testing.T, h *harness) {
	t.Helper()
	if _, err := h.store.Pool().Exec(h.ctx, `delete from work_holds where tree = $1`, heldTree); err != nil {
		t.Fatalf("open the work hold of %s: %v", heldTree, err)
	}
}

// heldPhaseTask is the implementer's phase task the claim keeps through every suspension here.
const heldPhaseTask = "Implement the plan."

// implementerWithItsTask is readyImplementer holding its phase's task, unsent since no shim is
// connected.
func implementerWithItsTask(t *testing.T, h *harness) claim.Token {
	t.Helper()
	token := readyImplementer(t, h)
	m, _ := h.supervisor.Machine(token)
	if err := m.Handle(h.ctx, supervise.RequestDeliver{Claim: token, ID: "outbox:7", Row: 7, Task: heldPhaseTask,
		Phase: phase.Implementing, Generation: h.stored(token).Generation}); err != nil {
		t.Fatalf("queue the phase task: %v", err)
	}
	return token
}

// stoppedByTheHold is implementerWithItsTask stopped by the work hold after its tree's close.
func stoppedByTheHold(t *testing.T, h *harness) claim.Token {
	t.Helper()
	token := implementerWithItsTask(t, h)
	closeWorkHold(t, h)
	m, _ := h.supervisor.Machine(token)
	if err := m.Handle(h.ctx, supervise.RequestSuspend{Claim: token, By: supervise.SuspendedByHold,
		Reason: "the design gate of LEGION-208 closed", Deadline: time.Now()}); err != nil {
		t.Fatalf("the hold's stop of %s: %v", token, err)
	}
	if got := h.stored(token); got.State != supervise.StateSuspended || got.SuspendedBy != supervise.SuspendedByHold {
		t.Fatalf("the worker is %s suspended by %q, want it stopped by the hold", got.State, got.SuspendedBy)
	}
	return token
}

// controllerSuspends is the controller's own suspension of the implementer through its door.
func controllerSuspends(t *testing.T, h *harness, token claim.Token) {
	t.Helper()
	wantAgentActionAnswer(t, askAgentAction(t, h, "/legion/v1/agents/suspend", "LEGION-209", claim.RoleImplementer),
		http.StatusOK, AgentActionResponse{State: "suspended", SuspendedBy: "controller"})
	if got := h.stored(token); got.SuspendedBy != supervise.SuspendedByController {
		t.Fatalf("the worker is suspended by %q, want the controller's suspension", got.SuspendedBy)
	}
}

// heldClaimCases are the claims of a tree under the work hold that the controller's start meets
// (decision 16 of dispatch://INFRA-4528's plan): (a) a claim the hold stopped, (b) one the
// controller suspended before the close, which the hold's stop does not take over, (c) one the
// hold stopped that the controller's suspension took over, and (d) one the hold stopped that the
// controller's stop retired, which the opening skips. reachable names the acts that reach the
// claim's state once the gate is open: a resume and a fresh session reach a suspended agent, a
// retry and a fresh session a retired one; (a) has none, since the opening starts it itself.
var heldClaimCases = []struct {
	name      string
	walk      func(t *testing.T, h *harness) claim.Token
	reachable []string
}{
	{"(a) a claim the hold stopped", stoppedByTheHold, nil},
	{"(b) a claim the controller suspended before the close", func(t *testing.T, h *harness) claim.Token {
		token := implementerWithItsTask(t, h)
		controllerSuspends(t, h, token)
		closeWorkHold(t, h)
		return token
	}, []string{"resume", "fresh session"}},
	{"(c) a held claim the controller took over", func(t *testing.T, h *harness) claim.Token {
		token := stoppedByTheHold(t, h)
		controllerSuspends(t, h, token)
		return token
	}, []string{"resume", "fresh session"}},
	{"(d) a held claim the controller's stop retired", func(t *testing.T, h *harness) claim.Token {
		token := stoppedByTheHold(t, h)
		wantAgentActionAnswer(t, askAgentAction(t, h, "/legion/v1/agents/stop", "LEGION-209", claim.RoleImplementer),
			http.StatusOK, AgentActionResponse{State: "retired"})
		return token
	}, []string{"retry", "fresh session"}},
}

// controllerStarts are the controller's acts that start an agent: its resume, its retry (which
// reaches a retired phase worker through its phase's restart, claimstart's restartWorkedPhase)
// and its fresh session.
var controllerStarts = []struct{ name, route string }{
	{"resume", "/legion/v1/agents/resume"},
	{"retry", "/legion/v1/agents/retry"},
	{"fresh session", "/legion/v1/claims/fresh-session"},
}

// wantHeldByWorkHold is the agent door's refusal of a start of a phase worker of a held tree.
func wantHeldByWorkHold(t *testing.T, recorder *httptest.ResponseRecorder) {
	t.Helper()
	if recorder.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409; body %s", recorder.Code, recorder.Body)
	}
	var failure Failure
	decodeInto(t, recorder, &failure)
	if failure.Code != heldByWorkHold {
		t.Fatalf("the refusal's code is %q, want %s; body %s", failure.Code, heldByWorkHold, recorder.Body)
	}
}

// t-6, decision 16: while the tree's work hold stands the controller's resume, retry and fresh
// session of its phase worker are each refused 409 HELD_BY_WORK_HOLD at the agent door, whatever
// suspended the worker, and nothing of the claim changes: its state, its source, its session and
// its phase task stay, and no phase restart is applied, so no start row is written.
func TestWorkHoldRefusesTheControllersAgentActionsWhileTheTreeIsHeld(t *testing.T) {
	for _, tc := range heldClaimCases {
		for _, act := range controllerStarts {
			t.Run(tc.name+"/"+act.name, func(t *testing.T) {
				facts := &phaseRestarter{}
				h := freshSessionHarness(t, facts)
				token := tc.walk(t, h)
				before := h.stored(token)

				wantHeldByWorkHold(t, askAgentAction(t, h, act.route, "LEGION-209", claim.RoleImplementer))

				after := h.stored(token)
				if after.State != before.State || after.SuspendedBy != before.SuspendedBy || after.Session != before.Session {
					t.Fatalf("the refused %s left the claim %s by %q on session %q, want it %s by %q on session %q",
						act.name, after.State, after.SuspendedBy, after.Session, before.State, before.SuspendedBy, before.Session)
				}
				if after.Session != "ses_implementer" {
					t.Fatalf("the refused %s left the claim on session %q, want its own session kept", act.name, after.Session)
				}
				if before.Pending != nil && (after.Pending == nil || after.Pending.Task != heldPhaseTask) {
					t.Fatalf("the refused %s left the claim holding %+v, want its phase task %q", act.name, after.Pending, heldPhaseTask)
				}
				if applied := facts.recorded(); len(applied) != 0 {
					t.Fatalf("the refused %s applied %#v, want no phase restart and so no start row", act.name, applied)
				}
			})
		}
	}
}

// t-6, decision 16: once the gate opens, the controller's start of a claim the opening does not
// start — one it suspended before the close or took over from the hold, or one its stop retired —
// is taken as it is taken of any claim it reaches.
func TestWorkHoldTakesTheControllersAgentActionsOnceTheTreeOpens(t *testing.T) {
	for _, tc := range heldClaimCases {
		for _, act := range controllerStarts {
			if !slices.Contains(tc.reachable, act.name) {
				continue
			}
			t.Run(tc.name+"/"+act.name, func(t *testing.T) {
				facts := &phaseRestarter{}
				h := freshSessionHarness(t, facts)
				token := tc.walk(t, h)
				retired := h.stored(token).State == supervise.StateRetired
				openWorkHold(t, h)

				recorder := askAgentAction(t, h, act.route, "LEGION-209", claim.RoleImplementer)

				if recorder.Code != http.StatusOK && recorder.Code != http.StatusAccepted {
					t.Fatalf("the controller's %s after the opening = %d, want it taken; body %s", act.name, recorder.Code, recorder.Body)
				}
				if retired {
					if applied := facts.recorded(); len(applied) != 1 {
						t.Fatalf("the controller's %s of the retired worker applied %#v, want its phase's restart", act.name, applied)
					}
					return
				}
				if got := h.stored(token); got.State != supervise.StateLaunching {
					t.Fatalf("after the controller's %s the claim is %s, want it launching", act.name, got.State)
				}
			})
		}
	}
}
