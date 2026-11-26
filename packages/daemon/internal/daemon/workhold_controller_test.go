package daemon

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/credential"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// The controller's agent door over a work-hold rig (#4065's agent actions, api's agentDoor), as
// the daemon's API serves it: the controller's grant, the rig's supervisor, and its workflow and
// admission as the handlers the claimstart path's phase restart applies its fact through
// (restartWorkedPhase).
const (
	agentResume       = "/legion/v1/agents/resume"
	agentRetry        = "/legion/v1/agents/retry"
	agentSuspend      = "/legion/v1/agents/suspend"
	agentStop         = "/legion/v1/agents/stop"
	agentFreshSession = "/legion/v1/claims/fresh-session"
)

// askTheControllerDoor posts route for role's agent of the rig's issue with a controller grant.
func askTheControllerDoor(r *workHoldRig, route string, role claim.Role) *httptest.ResponseRecorder {
	r.t.Helper()
	grants := credential.New(nil)
	grant, err := grants.MintController("ses_controller", 1)
	if err != nil {
		r.t.Fatalf("mint a controller grant: %v", err)
	}
	handler := api.NewServer("127.0.0.1", 0, api.Options{
		Supervisor: r.sup, Project: r.tokenProject, DispatchProject: r.tree.project, Grants: grants,
		Controller: r.trees, Trees: r.trees, Pool: r.pool, Record: r.records, Releaser: r.rt,
		Handlers: []intake.Handler{r.engine, r.admission}, RoleFor: workflow.RoleFor, Log: quietLogger(),
	}).Handler
	body, err := json.Marshal(api.ClaimRequest{GrantID: grant.ID, Issue: r.tree.key, Role: string(role)})
	if err != nil {
		r.t.Fatal(err)
	}
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequestWithContext(r.ctx, http.MethodPost, route, bytes.NewReader(body)))
	return recorder
}

// theControllerTakesOver is the controller's suspension of role's held claim, which takes the
// hold's stop over (decision 16).
func theControllerTakesOver(r *workHoldRig, role claim.Role) {
	r.t.Helper()
	if recorder := askTheControllerDoor(r, agentSuspend, role); recorder.Code != http.StatusOK {
		r.t.Fatalf("the controller's suspend of the %s = %d, want 200; body %s", role, recorder.Code, recorder.Body)
	}
	if c := r.machine(role).Claim(); c.SuspendedBy != supervise.SuspendedByController || c.WorkHeld() {
		r.t.Fatalf("the %s is suspended by %q, WorkHeld %v; want the controller's suspension in the hold's place", role, c.SuspendedBy, c.WorkHeld())
	}
}

// theControllerStops is the controller's stop of role's held claim, which retires it; the opening
// skips a retired claim.
func theControllerStops(r *workHoldRig, role claim.Role) {
	r.t.Helper()
	if recorder := askTheControllerDoor(r, agentStop, role); recorder.Code != http.StatusOK {
		r.t.Fatalf("the controller's stop of the %s = %d, want 200; body %s", role, recorder.Code, recorder.Body)
	}
	if c := r.machine(role).Claim(); c.State != supervise.StateRetired {
		r.t.Fatalf("the %s is %s after the controller's stop, want it retired", role, c.State)
	}
}

// refusedByTheHold is the agent door's refusal of the controller's start of role's agent while
// the tree's work hold stands: 409 HELD_BY_WORK_HOLD, with nothing of the claim changed.
func refusedByTheHold(r *workHoldRig, route string, role claim.Role) {
	r.t.Helper()
	before := r.machine(role).Claim()
	recorder := askTheControllerDoor(r, route, role)
	var failure api.Failure
	if err := json.Unmarshal(recorder.Body.Bytes(), &failure); err != nil || recorder.Code != http.StatusConflict || failure.Code != "HELD_BY_WORK_HOLD" {
		r.t.Fatalf("the controller's %s of the %s = %d %q, want 409 HELD_BY_WORK_HOLD; body %s", route, role, recorder.Code, failure.Code, recorder.Body)
	}
	if after := r.machine(role).Claim(); after.State != before.State || after.SuspendedBy != before.SuspendedBy || after.Session != before.Session {
		r.t.Fatalf("the refused %s left the %s %s by %q on %q, want it %s by %q on %q", route, role,
			after.State, after.SuspendedBy, after.Session, before.State, before.SuspendedBy, before.Session)
	}
}

// takenOnceOpen is the controller's start of role's agent once the gate is open: answered as a
// start, and the agent is launched.
func takenOnceOpen(r *workHoldRig, route string, role claim.Role) {
	r.t.Helper()
	launched := r.launches(role)
	recorder := askTheControllerDoor(r, route, role)
	if recorder.Code != http.StatusOK && recorder.Code != http.StatusAccepted {
		r.t.Fatalf("the controller's %s of the %s once the gate opened = %d, want it taken; body %s", route, role, recorder.Code, recorder.Body)
	}
	r.drain()
	if got := r.launches(role); got != launched+1 {
		r.t.Fatalf("the controller's %s of the %s once the gate opened launched it %d times, want once", route, role, got-launched)
	}
}

// H17 for the controller (decision 16): a claim the controller suspended before the close is no
// claim the hold stops: the close sends it nothing and leaves its suspension the controller's,
// since the controller's suspension outranks the hold's stop; the controller's resume is refused
// while the gate is closed; the opening starts nothing of it; and the controller's resume starts
// it once the gate is open.
func TestWorkHoldLeavesAClaimTheControllerSuspendedBeforeTheClose(t *testing.T) {
	r := newWorkHoldRig(t, legionTree(phase.Testing, config.HoldWork))
	r.live(claim.RoleArchitect)
	r.live(claim.RoleTester)
	theControllerSuspends := func() {
		if recorder := askTheControllerDoor(r, agentSuspend, claim.RoleTester); recorder.Code != http.StatusOK {
			t.Fatalf("the controller's suspend of the tester = %d, want 200; body %s", recorder.Code, recorder.Body)
		}
	}
	theControllerSuspends()
	launched, told := r.launches(claim.RoleTester), len(r.published(claim.RoleTester))

	r.close(4)
	r.drain()

	if c := r.machine(claim.RoleTester).Claim(); c.State != supervise.StateSuspended || c.SuspendedBy != supervise.SuspendedByController {
		t.Fatalf("after the close the tester is %s by %q, want the controller's suspension left as it was", c.State, c.SuspendedBy)
	}
	if got := len(r.published(claim.RoleTester)); got != told {
		t.Fatalf("the close told the controller's suspended tester %d messages, want none", got-told)
	}
	refusedByTheHold(r, agentResume, claim.RoleTester)

	r.approve(4, 12)
	r.drain()

	if c := r.machine(claim.RoleTester).Claim(); c.State != supervise.StateSuspended || c.SuspendedBy != supervise.SuspendedByController {
		t.Fatalf("after the opening the tester is %s by %q, want the opening to leave the controller's suspension", c.State, c.SuspendedBy)
	}
	if got := r.launches(claim.RoleTester); got != launched {
		t.Fatalf("the opening launched the controller's suspended tester %d times, want none", got-launched)
	}
	takenOnceOpen(r, agentResume, claim.RoleTester)
}
