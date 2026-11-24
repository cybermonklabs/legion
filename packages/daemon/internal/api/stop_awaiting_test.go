package api

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// A root that waits on a person before its first step stands in awaiting_person, where the
// record holds no hold (the issues table's issues_held_from_check and issues_awaiting_shape), so
// stop_task cannot stop its task there. The route and the daemon's own workflow answer the stop
// 409 TASK_AWAITING_PERSON, naming what the owner can do, and write nothing: the root still waits
// on its person, holds no hold, and its plan stands as it did, none for a root whose owner gave
// none (the stop settles no plan) and the owner's task still waiting where it gave one.
func TestAStopOfATaskWhoseRootWaitsOnAPersonIsRefused(t *testing.T) {
	const root, task = "LEGION-208", "a"
	for _, tc := range []struct {
		name string
		// task is the task the stop names: the task default of the project's default steps that the
		// first step would take where the owner gave no plan, or the owner's task a.
		task string
		// gives says whether the owner gives its plan through register_plan before the stop.
		gives bool
	}{
		{name: "with no plan given", task: "default"},
		{name: "with the owner's plan", task: task, gives: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, _, _ := newWorkflowHarness(t, workflow.Config{Project: testProject, Repositories: testRepositories,
				Steps: config.DesignGateRootIssues.Steps()})
			since := time.Date(2026, 11, 20, 9, 0, 0, 0, time.UTC)
			h.recordIssue(record.Issue{Key: root, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Tree: root,
				Project: testProject, Title: root, Phase: phase.AwaitingPerson, Generation: 1, Status: "in_progress", Rank: "U", HandedOver: true,
				LastDispatchSeq: 3, Awaiting: &record.Awaiting{Ask: "ask-1", Since: record.EventMark{Seq: 3, At: since}}})
			architect := newLiveClaim(t, h, root, claim.RoleArchitect)
			if tc.gives {
				h.handoffs.documents = map[string]string{planDoc: root}
				h.handoffs.approvals = map[string]dispatch.Approval{planDoc: {State: "none", LatestVersion: 1}}
				h.handoffs.plans = map[string]dispatch.DocumentVersion{}
				h.handoffs.givePlan(planDoc, 1, taskBlock(task, "implementer reviewer"))
				if given := registerPlan(h, architect.grant(t), root, planDoc, 1); given.Code != http.StatusOK {
					t.Fatalf("the owner's plan for %s while it waits on a person = %d %s, want 200", root, given.Code, given.Body)
				}
			}
			before := planTaskStates(t, h, root)

			refusal := intake.TaskAwaitsPerson(root, tc.task)
			refusedSaying(t, stopTask(h, architect.grant(t), root, tc.task), refusal.Status, refusal.Code, refusal.Message)

			got := recordedChild(t, h, root)
			if got == nil || got.Phase != phase.AwaitingPerson || got.Hold != nil || got.Awaiting == nil || got.Awaiting.Ask != "ask-1" {
				t.Fatalf("%s once its stop was refused = %#v, want it still awaiting the person on ask-1 with no hold", root, got)
			}
			if after := planTaskStates(t, h, root); len(after) != len(before) || (tc.gives && after[task] != record.TaskWaiting) {
				t.Fatalf("the plan tasks of %s once its stop was refused = %v, want them as they stood before it, %v", root, after, before)
			}
		})
	}
}

// planTaskStates is the state of each task of key's plan at generation 1, by task, as the
// daemon's store holds them: empty where it holds no plan rows.
func planTaskStates(t *testing.T, h *harness, key string) map[string]record.TaskState {
	t.Helper()
	states := map[string]record.TaskState{}
	if err := store.Transact(context.Background(), h.store.Pool(), func(ctx context.Context, tx pgx.Tx) error {
		stored, err := record.NewStore().Plan(context.Background(), tx, key, 1)
		if err != nil || stored == nil {
			return err
		}
		for _, task := range stored.Tasks {
			states[task.Task] = task.State
		}
		return nil
	}); err != nil {
		t.Fatalf("read the plan of %s: %v", key, err)
	}
	return states
}
