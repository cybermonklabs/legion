package api

import (
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// While the project is paused, the claim door's resume and retry are refused PROJECT_PAUSED for
// the controller and for an architect alike, with the pause that stands and a sentence that names
// the controller's resume operation as what lifts the pause, and nothing of the claim changes: no
// claim operation undoes the pause (claimResume's event builder, claimRetry). The door's stop
// and suspend start nothing and still run: the test asks the controller's suspend and an
// architect's stop. The controller's pod update is refused PROJECT_PAUSED as well, before the
// daemon is asked anything (issuePodUpdate, refusedForPause).
//
// The negative control is the pause check undone (the refusedWhilePaused calls of claimResume's
// event builder and of claimRetry, and issuePodUpdate's refusedForPause): the resume and the
// retry reach the claim and are answered 200, and the pod update is answered 404 NO_POD by this
// daemon, which runs no pod update rounds.
func TestWhilePausedTheClaimDoorRefusesResumeAndRetryAndStillStopsAndSuspends(t *testing.T) {
	h := newPauseHarness(t, pauseCapable())
	grant := h.controllerSession()
	architect := treeArchitect(t, h.harness)
	// The implementer an architect suspended, which either caller's resume would lift, and the
	// tester an architect's stop retired, which either caller's retry would start.
	implementer := architectSuspendedImplementer(t, h.harness, architect)
	tester, _ := h.launchIn("LEGION-208", "LEGION-209", claim.RoleTester)
	h.ready(tester, "ses_tester", 1)
	wantClaimOperationAnswer(t, askAsArchitect(t, h.harness, architect, "/legion/v1/claims/stop", "LEGION-209", claim.RoleTester),
		http.StatusOK, ClaimOperationResponse{State: "retired"})
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

	type caller struct {
		name  string
		grant func() string
	}
	for _, by := range []caller{
		{"the controller", func() string { return grant }},
		{"an architect", func() string { return architect.grant(t) }},
	} {
		for _, request := range []struct {
			route string
			role  claim.Role
		}{
			{"/legion/v1/claims/resume", claim.RoleImplementer},
			{"/legion/v1/claims/retry", claim.RoleTester},
		} {
			failure := h.pauseRefused(request.route, ClaimRequest{GrantID: by.grant(), Issue: "LEGION-209", Role: string(request.role)},
				nil, http.StatusConflict, projectPaused)
			if failure.Pause == nil || failure.Pause.ID != began.ID || !strings.Contains(failure.Error, "the project is paused") ||
				!strings.Contains(failure.Error, "the controller's resume operation of its legion tool") {
				t.Fatalf("%s's %s of the %s refused %+v, want it to name pause %d and the controller's resume operation",
					by.name, request.route, request.role, failure, began.ID)
			}
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
	if got := h.stored(implementer); got.State != supervise.StateSuspended || got.SuspendedBy != supervise.SuspendedByArchitect {
		t.Fatalf("the implementer is %s by %q after the refused resumes, want suspended by the architect", got.State, got.SuspendedBy)
	}

	// A suspension and a stop start nothing, and still run: the controller's suspend and an
	// architect's stop.
	wantClaimOperationAnswer(t, askClaimOperation(t, h.harness, "/legion/v1/claims/suspend", "LEGION-209", claim.RoleReviewer),
		http.StatusOK, ClaimOperationResponse{State: "suspended", SuspendedBy: "controller"})
	wantClaimOperationAnswer(t, askAsArchitect(t, h.harness, architect, "/legion/v1/claims/stop", "LEGION-209", claim.RolePlanner),
		http.StatusOK, ClaimOperationResponse{State: "retired"})
	if _, open := h.svc.Paused(); !open {
		t.Fatal("the pause ended under the requests")
	}
}
