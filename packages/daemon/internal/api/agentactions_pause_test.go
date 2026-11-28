package api

import (
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// While the project is paused, the agent door's resume and retry are refused PROJECT_PAUSED for
// the controller, with the pause that stands and a sentence that names the controller's resume
// operation as what lifts the pause, and nothing of the claim changes: no agent action undoes the
// pause (resumeAgent's event builder, retryAgent). The door's stop and suspend start nothing and
// still run: the test asks the controller's suspend and stop. The controller's pod update is
// refused PROJECT_PAUSED as well, before the daemon is asked anything (issuePodUpdate,
// refusedForPause).
//
// The negative control is the pause check undone (the refusedWhilePaused calls of resumeAgent's
// event builder and of retryAgent, and issuePodUpdate's refusedForPause): the resume and the
// retry reach the agent and are answered 200, and the pod update is answered 404 NO_POD by this
// daemon, which runs no pod update rounds.
func TestWhilePausedTheAgentDoorRefusesResumeAndRetryAndStillStopsAndSuspends(t *testing.T) {
	h := newPauseHarness(t, pauseCapable())
	grant := h.controllerSession()
	// The implementer the controller suspended, which its resume would lift, and the tester its
	// stop retired, which its retry would start.
	implementer := controllerSuspendedImplementer(t, h.harness)
	tester, _ := h.launchIn("LEGION-208", "LEGION-209", claim.RoleTester)
	h.ready(tester, "ses_tester", 1)
	wantAgentActionAnswer(t, askAgentAction(t, h.harness, "/legion/v1/agents/stop", "LEGION-209", claim.RoleTester),
		http.StatusOK, AgentActionResponse{State: "retired"})
	reviewer, _ := h.launchIn("LEGION-208", "LEGION-209", claim.RoleReviewer)
	h.ready(reviewer, "ses_reviewer", 1)
	planner, _ := h.launchIn("LEGION-208", "LEGION-209", claim.RolePlanner)
	h.ready(planner, "ses_planner", 1)

	began := beganPause(t, h.pauseOK(operatorPauseRoute, OperatorPauseRequest{Reason: "a provider outage"}, bearer))
	before, err := h.store.Claims(h.ctx)
	if err != nil {
		t.Fatal(err)
	}
	calls := len(h.runtime.Calls())

	for _, request := range []struct {
		route string
		role  claim.Role
	}{
		{"/legion/v1/agents/resume", claim.RoleImplementer},
		{"/legion/v1/agents/retry", claim.RoleTester},
	} {
		failure := h.pauseRefused(request.route, ClaimRequest{GrantID: grant, Issue: "LEGION-209", Role: string(request.role)},
			nil, http.StatusConflict, projectPaused)
		if failure.Pause == nil || failure.Pause.ID != began.ID || !strings.Contains(failure.Error, "the project is paused") ||
			!strings.Contains(failure.Error, "the controller's resume operation of its legion tool") {
			t.Fatalf("the controller's %s of the %s refused %+v, want it to name pause %d and the controller's resume operation",
				request.route, request.role, failure, began.ID)
		}
	}
	podUpdate := h.pauseRefused("/legion/v1/issues/pod-update", PodUpdateRequest{GrantID: grant, Issue: "LEGION-209"}, nil,
		http.StatusConflict, projectPaused)
	if podUpdate.Pause == nil || podUpdate.Pause.ID != began.ID || !strings.Contains(podUpdate.Error, "the controller's resume operation") {
		t.Fatalf("the pod update refused %+v, want it to name pause %d and the controller's resume operation", podUpdate, began.ID)
	}
	after, err := h.store.Claims(h.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(after, before) {
		t.Fatalf("the refused requests changed the claims:\nbefore %+v\nafter  %+v", before, after)
	}
	if got := len(h.runtime.Calls()); got != calls {
		t.Fatalf("the refused requests reached the runtime: %v", h.runtime.Methods()[calls:])
	}
	if got := h.stored(implementer); got.State != supervise.StateSuspended || got.SuspendedBy != supervise.SuspendedByController {
		t.Fatalf("the implementer is %s by %q after the refused resume, want suspended by the controller", got.State, got.SuspendedBy)
	}

	// A suspension and a stop start nothing, and still run: the controller's suspend and stop.
	wantAgentActionAnswer(t, askAgentAction(t, h.harness, "/legion/v1/agents/suspend", "LEGION-209", claim.RoleReviewer),
		http.StatusOK, AgentActionResponse{State: "suspended", SuspendedBy: "controller"})
	wantAgentActionAnswer(t, askAgentAction(t, h.harness, "/legion/v1/agents/stop", "LEGION-209", claim.RolePlanner),
		http.StatusOK, AgentActionResponse{State: "retired"})
	if _, open := h.svc.Paused(); !open {
		t.Fatal("the pause ended under the requests")
	}
}
