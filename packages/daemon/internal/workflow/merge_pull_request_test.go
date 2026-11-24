package workflow

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// mergingTasks seeds LEGION-208 at implementing and its child LEGION-209 in merging at generation
// 3, whose plan holds task a, done, on the child's branch with its pull request #43 in aState, and
// task b, which needs a and so is the task that merges, at its merge on legion/LEGION-209.b with
// its own pull request #45 in bState. It answers b's pull request as seeded.
func mergingTasks(t *testing.T, pool *pgxpool.Pool, aState, bState record.PullRequestState) record.PullRequest {
	t.Helper()
	stepRoot(t, pool, phase.Implementing)
	child := stepChild(t, pool, "LEGION-209", phase.Merging, nil)
	seedTasks(t, pool, child.Key, child.Generation,
		planTask{id: "a", steps: []string{"implementer"}, agents: []claim.Agent{"implementer"}, state: record.TaskDone, place: atStep(1)},
		planTask{id: "b", steps: []string{"implementer"}, agents: []claim.Agent{"implementer-2"}, needs: []string{"a"}, state: record.TaskRunning,
			place: record.TaskPlace{Kind: record.PlaceMerge}})
	seedTaskBranch(t, pool, child.Key, "a", "legion/LEGION-209")
	seedTaskBranch(t, pool, child.Key, "b", "legion/LEGION-209.b")
	a := record.PullRequest{State: aState, Issue: child.Key, Repo: "sjawhar/legion", Number: 43, Branch: "legion/LEGION-209", HeadSHA: "head-a", Failing: []string{}}
	b := record.PullRequest{State: bState, Issue: child.Key, Repo: "sjawhar/legion", Number: 45, Branch: "legion/LEGION-209.b", HeadSHA: "head-b", Failing: []string{}}
	for _, pr := range []*record.PullRequest{&a, &b} {
		if pr.State == record.PullRequestMerged {
			pr.MergeSHA = "merge-" + pr.HeadSHA
		}
		seedPR(t, pool, *pr)
	}
	return b
}

// The merge of an issue is the pull request of its task that merges, here b, the one task of its
// plan with a merge (record.Store.MergePullRequest), and no other task's. LEGION-209 stands in merging
// with task a done on the issue's branch, where its pull request #43 merged, and task b, which needs
// a, at its merge, with its own pull request #45 still open. The issue's phase is not finished
// (PhaseFinished), and the boot's sweep of merged issues (sweepMerged) closes nothing: the issue
// stands in merging, b at its merge, with no production check. Once #45 merges, the phase is
// finished, and the sweep closes the issue as merged on b's merge: it is done, b is done at its
// merge, its leave retires each claim but the claim of its check's agent, the fresh check-b since
// b has no tester step, and the production check it opens is on #45 and its merge commit.
//
// The fault it catches is the work of a task that does not merge read as the issue's merge, which
// ends the issue before its merge (its control: the readers on the issue's first pull request in
// task order, a's #43, so the phase reads finished while #45 is open).
func TestTheMergeOfAnIssueIsItsMergingTasksPullRequest(t *testing.T) {
	pool := migratedPool(t)
	engine := returnEngine()
	b := mergingTasks(t, pool, record.PullRequestMerged, record.PullRequestOpen)
	finished := func() bool {
		t.Helper()
		var got bool
		onStored(t, pool, engine, "LEGION-209", func(ctx context.Context, tx pgx.Tx, issue record.Issue) (err error) {
			got, err = PhaseFinished(ctx, engine.store, tx, issue)
			return err
		})
		return got
	}
	sweep := func() {
		t.Helper()
		applyInTx(t, pool, engine, func(ctx context.Context, tx pgx.Tx) error { return engine.sweepMerged(ctx, tx) })
	}
	merge := record.TaskPlace{Kind: record.PlaceMerge}

	if finished() {
		t.Fatal("LEGION-209's merging is finished while b's pull request #45 is open, want it not: a's #43 is the work of a task that does not merge")
	}
	sweep()
	if got := issueAt(t, pool, "LEGION-209"); got != phase.Merging {
		t.Fatalf("LEGION-209 stands in %s after the sweep while #45 is open, want merging", got)
	}
	assertTask(t, tasksOf(t, pool, engine, "LEGION-209"), "b", record.TaskRunning, merge)
	if check := productionCheck(t, pool, "LEGION-209"); check != nil {
		t.Fatalf("the sweep opened the production check %+v while #45 is open, want none", check)
	}

	b.State, b.MergeSHA = record.PullRequestMerged, "merge-45"
	seedPR(t, pool, b)
	if !finished() {
		t.Fatal("LEGION-209's merging is not finished once b's pull request #45 merged, want it finished")
	}
	sweep()
	if got := issueAt(t, pool, "LEGION-209"); got != phase.Done {
		t.Fatalf("LEGION-209 stands in %s after the sweep of its merged #45, want done", got)
	}
	assertTask(t, tasksOf(t, pool, engine, "LEGION-209"), "b", record.TaskDone, merge)
	fresh, err := claim.AgentToken("legion", "LEGION-209", "check-b")
	if err != nil {
		t.Fatal(err)
	}
	if closed := superviseRequests(t, pool, "issue_close"); closed["LEGION-209/implementer"] != 1 || closed["LEGION-209/tester"] != 1 ||
		len(claimRows(t, pool, "LEGION-209", "issue_close", fresh)) != 0 {
		t.Fatalf("issue_close rows %v, want the implementer's and the tester's, and none naming check-b's claim %s, which works the production check of b's merge", closed, fresh)
	}
	if check := productionCheck(t, pool, "LEGION-209"); check == nil || check.Number != 45 || check.MergeSHA != "merge-45" {
		t.Fatalf("the production check after the close = %+v, want one on #45 at merge-45, the merging task's pull request", check)
	}
}

// A merged issue whose plan holds several tasks closes when the last issue beneath it leaves the
// workflow, on its merging task's merge (closeAwaitingMerge). LEGION-209 stands in merging with task
// a done and its pull request #43 still open, and task b, the task that merges, at its merge with
// its pull request #45 merged; its child LEGION-210 is still implementing, so its close waits. When
// LEGION-210 is done, LEGION-209 closes: it is done, and b is done at its merge.
//
// The fault it catches is a merged issue that never closes because the wait reads another task's
// open pull request as its merge, or reads the close on the issue rather than on its merging task,
// whose walk a plan of several tasks does not give (its controls: the read on the issue's first
// pull request in task order, a's open #43, so LEGION-209 stays in merging; the close on the
// issue, so the departure's fact fails on the walk).
func TestAMergedIssueOfSeveralTasksClosesWhenTheLastIssueBeneathItLeaves(t *testing.T) {
	pool := migratedPool(t)
	engine := returnEngine()
	mergingTasks(t, pool, record.PullRequestOpen, record.PullRequestMerged)
	parent := "LEGION-209"
	seedIssue(t, pool, record.Issue{Key: "LEGION-210", Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion", Tree: "LEGION-208", Project: "LEGION",
		Title: "LEGION-210", Parent: &parent, Phase: phase.Implementing, Generation: 1, Status: "in_progress", Rank: "W", LastDispatchSeq: 1})

	mustSpend(t, "LEGION-210's done", applyFacts(t, pool, engine)("grandchild-done", intake.DispatchIssue{Key: "LEGION-210", ResolvedRepo: "sjawhar/legion", Seq: 2,
		Type: "issue.closed", Status: "done", Title: "LEGION-210", Parent: "LEGION-209", Rank: "W"}))

	if got := issueAt(t, pool, "LEGION-209"); got != phase.Done {
		t.Fatalf("LEGION-209 stands in %s once the last issue beneath it left, want done: b's #45 merged", got)
	}
	assertTask(t, tasksOf(t, pool, engine, "LEGION-209"), "b", record.TaskDone, record.TaskPlace{Kind: record.PlaceMerge})
}
