package api

import (
	"net/http"
	"reflect"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/plan"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/treelifecycle"
)

// newSubOwnerClaim is newLiveClaim for the sub-owner of task on issue, a root of its own tree: the
// claim of role sub-owner whose agent plan.SubOwnerAgent names, created and launched the way
// the spawn route does, registered, and in the turn of a task it was given.
func newSubOwnerClaim(t *testing.T, h *harness, issue, task string) liveClaim {
	t.Helper()
	token, err := claim.AgentToken(testProject, issue, plan.SubOwnerAgent(task))
	if err != nil {
		t.Fatalf("the token of the sub-owner of task %s of %s: %v", task, issue, err)
	}
	if _, err := h.store.OpenTreeLifecycle(h.ctx, testProject, issue, treelifecycle.AuthorityWorkflow); err != nil {
		t.Fatalf("open the lifecycle of %s: %v", issue, err)
	}
	m, _, err := h.supervisor.Create(h.ctx, supervise.Claim{
		Token: token, Project: testProject, Tree: issue, Issue: issue, Role: claim.RoleSubOwner, State: supervise.StateQueued,
	}, "role prompt")
	if err != nil {
		t.Fatalf("create %s: %v", token, err)
	}
	if err := m.Handle(h.ctx, supervise.RequestSpawn{Claim: token}); err != nil {
		t.Fatalf("spawn %s: %v", token, err)
	}
	session := "ses_sub_owner_" + task + "_" + issue
	registration := h.registered(h.bootToken(token), session)
	working(t, h, token, issue)
	return liveClaim{h: h, token: token, session: session, secret: registration.Secret, tree: issue, issue: issue}
}

// register_plan takes a version of a plan from the sub-owner of a task of the issue, as from its
// owner: the grant of the sub-owner of task f of LEGION-208 gives version 4 of LEGION-208's plan
// document, answered 200 and recorded as the one fact the workflow reads (intake.PlanGiven), by
// the sub-owner's claim at the issue's generation. Which task the claim is the sub-owner of is the
// workflow's to read. The same grant giving a version of the plan of LEGION-209 is refused, and so
// is a phase worker's grant, as today; neither records anything.
//
// The fault it catches is a sub-owner that cannot give the parts of its task, or that gives the
// plan of another issue (its control: planOwner as it was, which answers the sub-owner's grant
// 403 ARCHITECT_REQUIRED).
func TestASubOwnerGivesAPlanOfItsIssueAlone(t *testing.T) {
	h, facts, statuses := planHarness(t)
	subOwner := newSubOwnerClaim(t, h, "LEGION-208", "f")
	worker := newLiveClaim(t, h, "LEGION-208", claim.RoleImplementer)
	statuses.givePlan(planDoc, 4, taskBlock("a", "implementer reviewer"))
	statuses.givePlan(childDoc, 2, taskBlock("a", "implementer reviewer"))

	if taken := registerPlan(h, subOwner.grant(t), "LEGION-208", planDoc, 4); taken.Code != http.StatusOK {
		t.Fatalf("the sub-owner's version 4 of the plan of LEGION-208 = %d %s, want 200", taken.Code, taken.Body)
	}
	want := []intake.Fact{givenPlan("LEGION-208", 1, subOwner.token, planDoc, 4, implementerReviewer...)}
	if got := facts.recorded(); !reflect.DeepEqual(got, want) {
		t.Fatalf("facts = %#v, want the sub-owner's version alone: %#v", got, want)
	}
	assertProcessedEvent(t, h, "plan:LEGION-208:1:"+planDoc+":4")

	refusedSaying(t, registerPlan(h, subOwner.grant(t), "LEGION-209", childDoc, 2), http.StatusForbidden, "SUB_OWNER_OUTSIDE_ISSUE",
		"a sub-owner of LEGION-208 gives a version of the plan of LEGION-208 alone.")
	refused(t, registerPlan(h, worker.grant(t), "LEGION-208", planDoc, 4), http.StatusForbidden, "ARCHITECT_REQUIRED")
	if got := facts.recorded(); !reflect.DeepEqual(got, want) {
		t.Fatalf("facts after the refused grants = %#v, want the sub-owner's version alone: %#v", got, want)
	}
}
