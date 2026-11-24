package workflow

import (
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// A root whose plan holds several tasks leaves the workflow as a root of one task does: it stands
// done, its tree lingers, and each of its tasks ends where it stands, in the state its leave gives
// it (endTaskWithLeave, leaveState). The merge of the pull request #45 of task b, the task that
// merges, closes LEGION-208 with a done at its step and b done at its merge, and opens the
// production check on #45. A person's backlog on LEGION-208 while a tests at its step 2 and c waits
// for it leaves a left at step 2 and c left at no step.
//
// The fault it catches is a root of several tasks that cannot leave: the leave walking the issue
// as one task, which a plan of several does not give, so the merge's fact and the person's move
// fail and the root stays in the workflow (its control: endTaskWithLeave on the issue's one walk).
func TestARootOfSeveralTasksLeavesWithEachTaskWhereItStands(t *testing.T) {
	t.Run("the merge of its task that merges", func(t *testing.T) {
		pool := migratedPool(t)
		engine := returnEngine()
		root := stepRoot(t, pool, phase.Merging)
		seedTasks(t, pool, root.Key, root.Generation,
			planTask{id: "a", steps: []string{"implementer"}, agents: []claim.Agent{"implementer"}, state: record.TaskDone, place: atStep(1)},
			planTask{id: "b", steps: []string{"implementer"}, agents: []claim.Agent{"implementer-2"}, needs: []string{"a"}, state: record.TaskRunning,
				place: record.TaskPlace{Kind: record.PlaceMerge}})
		seedTaskBranch(t, pool, root.Key, "a", "legion/LEGION-208")
		seedTaskBranch(t, pool, root.Key, "b", "legion/LEGION-208.b")
		seedPR(t, pool, record.PullRequest{State: record.PullRequestOpen, Issue: root.Key, Repo: "sjawhar/legion", Number: 45,
			Branch: "legion/LEGION-208.b", HeadSHA: "head-b", Failing: []string{}})

		mustSpend(t, "the merge of #45", applyFacts(t, pool, engine)("merged", intake.PullRequestMerged{Repo: "sjawhar/legion", Number: 45, MergeSHA: "merge-45"}))

		if got := issueOf(t, pool, engine, root.Key); got.Phase != phase.Done || got.LingerUntil == nil {
			t.Fatalf("LEGION-208 stands in %s with linger %v after the merge of #45, want done with its tree lingering", got.Phase, got.LingerUntil)
		}
		tasks := tasksOf(t, pool, engine, root.Key)
		assertTask(t, tasks, "a", record.TaskDone, atStep(1))
		assertTask(t, tasks, "b", record.TaskDone, record.TaskPlace{Kind: record.PlaceMerge})
		if check := productionCheck(t, pool, root.Key); check == nil || check.Number != 45 || check.MergeSHA != "merge-45" {
			t.Fatalf("the production check after the close = %+v, want one on #45 at merge-45", check)
		}
	})

	t.Run("a person's backlog", func(t *testing.T) {
		pool := migratedPool(t)
		engine := returnEngine()
		root := stepRoot(t, pool, phase.Testing)
		seedTasks(t, pool, root.Key, root.Generation,
			planTask{id: "a", steps: []string{"implementer", "tester"}, agents: []claim.Agent{"implementer", "tester"}, state: record.TaskRunning, place: atStep(2)},
			planTask{id: "c", steps: []string{"implementer"}, agents: []claim.Agent{"implementer-2"}, needs: []string{"a"}, state: record.TaskWaiting})

		mustSpend(t, "the backlog", applyFacts(t, pool, engine)("backlog", intake.DispatchIssue{Key: root.Key, ResolvedRepo: "sjawhar/legion", Seq: 2,
			Type: "issue.updated", Status: "backlog", Title: "root", Rank: "U"}))

		if got := issueOf(t, pool, engine, root.Key); got.Phase != phase.Done || got.LingerUntil == nil {
			t.Fatalf("LEGION-208 stands in %s with linger %v after the backlog, want done with its tree lingering", got.Phase, got.LingerUntil)
		}
		tasks := tasksOf(t, pool, engine, root.Key)
		assertTask(t, tasks, "a", record.TaskLeft, atStep(2))
		assertTask(t, tasks, "c", record.TaskLeft, record.TaskPlace{})
	})
}
