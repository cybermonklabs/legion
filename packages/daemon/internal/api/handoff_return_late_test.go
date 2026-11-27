package api

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/plan"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// A tester's report can reach the route after a later version of the plan took the tester's step
// off its task's line: LEGION-208 stands in implementing at step 1 of task a, whose steps are now
// `implementer reviewer`, and its tester, whose run the daemon ended without its report, sends its
// completion, with no verdict, naming the head it tested and returnTo 1. The report names no step
// the task can return from, so the route answers 409 RETURN_REFUSED with what to send instead, and
// records nothing; the same report with no returnTo is then taken (200), on the tester's own row. A
// 500 would answer every retry of the same report the same way, since its key names the step.
func TestALateFailNamingAStepOffItsTasksLineIsRefusedNotFailed(t *testing.T) {
	const root = "LEGION-208"
	h, _, _ := newWorkflowHarness(t, workflow.Config{Project: testProject, Repositories: testRepositories,
		Steps: config.DesignGateRootIssues.Steps()})
	h.recordIssue(record.Issue{Key: root, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Tree: root,
		Project: testProject, Title: root, Phase: phase.Implementing, Generation: 1, Status: "in_progress", Rank: "U", HandedOver: true,
		LastDispatchSeq: 3})
	architect := newLiveClaim(t, h, root, claim.RoleArchitect)
	roles := plan.BuiltinRoles()
	if err := store.Transact(context.Background(), h.store.Pool(), func(ctx context.Context, tx pgx.Tx) error {
		store := record.NewStore()
		v := record.PlanVersion{Issue: root, Generation: 1, Version: 3, ArtifactID: planDoc, Source: record.PlanSourceOwner,
			GivenBy: architect.token, GivenAt: time.Date(2026, 11, 20, 9, 0, 0, 0, time.UTC)}
		task := record.PlanTask{Issue: root, Generation: 1, Task: "a", PlanVersion: 3, BlockText: "Build the widget.", State: record.TaskWaiting}
		steps := []record.PlanStep{
			{Issue: root, Generation: 1, Task: "a", Position: 1, Role: "implementer", Type: roles["implementer"].Type},
			{Issue: root, Generation: 1, Task: "a", Position: 2, Role: "reviewer", Type: roles["reviewer"].Type},
		}
		if err := store.PutPlan(ctx, tx, v, []record.PlanTask{task}, steps); err != nil {
			return err
		}
		return store.MoveTask(ctx, tx, root, 1, "a", record.TaskPlace{Kind: record.PlaceStep, Position: 1}, record.TaskRunning)
	}); err != nil {
		t.Fatalf("seed the plan of %s: %v", root, err)
	}
	seedEntry(t, h, root, claim.RoleTester, 1)
	tester := newLiveClaim(t, h, root, claim.RoleTester)
	written := tester.writeHandoff("test", 1)
	fail := func(returnTo *int) HandoffCompleteRequest {
		return HandoffCompleteRequest{GrantID: tester.grant(t), Summary: "the widget does not render", Commit: headSHA,
			HandoffID: written, HandoffVersion: 1, ReturnTo: returnTo}
	}

	refused(t, h.complete(fail(new(1))), http.StatusConflict, "RETURN_REFUSED")
	if got := recordedChild(t, h, root); got == nil || got.Phase != phase.Implementing {
		t.Fatalf("%s once the fail was refused = %#v, want it still in implementing", root, got)
	}

	if again := h.complete(fail(nil)); again.Code != http.StatusOK {
		t.Fatalf("the same fail with no returnTo = %d %s, want 200: it is the report of the run the daemon ended", again.Code, again.Body)
	}
	if got := recordedChild(t, h, root); got == nil || got.Phase != phase.Implementing {
		t.Fatalf("%s once the fail was taken = %#v, want it still in implementing: the report moves nothing", root, got)
	}
}
