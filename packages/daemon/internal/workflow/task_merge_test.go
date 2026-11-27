package workflow

import (
	"reflect"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// atMerge and atRetro are the places of a task's merge and its retro.
var (
	atMerge = record.TaskPlace{Kind: record.PlaceMerge}
	atRetro = record.TaskPlace{Kind: record.PlaceRetro}
)

// seedAAndB writes on key, at generation, the plan of a (`implementer`, whose block says merges)
// and b (`implementer`, no needs), each with an agent of its own, standing where aAt and bAt say
// and running: a on the issue's own branch, whose pull request is the issue's (stepRoot's #42 for
// LEGION-208, seedChildPR's #43 for LEGION-209), and b on <issue branch>.b with its pull request
// bPR open.
func seedAAndB(t *testing.T, pool *pgxpool.Pool, engine *Engine, key string, generation uint64, aAt, bAt record.TaskPlace, bPR int) {
	t.Helper()
	seedTasks(t, pool, key, generation,
		planTask{id: "a", steps: []string{"implementer"}, agents: []claim.Agent{"impl-a"}, state: record.TaskRunning, place: aAt},
		planTask{id: "b", steps: []string{"implementer"}, agents: []claim.Agent{"impl-b"}, state: record.TaskRunning, place: bAt})
	markMerges(t, pool, key, "a")
	seedTaskBranch(t, pool, key, "a", claim.IssueBranch(key))
	seedTaskBranch(t, pool, key, "b", claim.IssueBranch(key)+".b")
	seedTaskPR(t, pool, engine, key, "b", bPR)
}

// actEnd is the result of the newest end of the act of kind (its retro or its merge) of task in
// key's step records, "" where none ended.
func actEnd(t *testing.T, pool *pgxpool.Pool, key, task, kind string) string {
	t.Helper()
	result := ""
	for _, write := range stepWrites(t, pool, key) {
		if write.Task == task && write.Kind == kind && write.EndedAt != nil && write.Result != nil {
			result = *write.Result
		}
	}
	return result
}

// assertNoClose fails if key's outbox holds anything of a close of the issue: a status of done, an
// issue_close or a suspension of a claim, a suspension of its pod, a linger's close or a
// child-closed notice.
func assertNoClose(t *testing.T, pool *pgxpool.Pool, key string) {
	t.Helper()
	for _, write := range statusWrites(t, pool, key) {
		if write.Status == "done" {
			t.Errorf("the status writes of %s hold %+v, want no done: the issue goes on", key, write)
		}
	}
	for _, op := range []string{"issue_close", "suspend"} {
		if rows := superviseRowsOf(t, pool, key, op); len(rows) != 0 {
			t.Errorf("the %s rows of %s = %+v, want none: the issue goes on", op, key, rows)
		}
	}
	if rows := issueSuspensions(t, pool, key); len(rows) != 0 {
		t.Errorf("the issue_suspend rows of %s = %+v, want none", key, rows)
	}
	if rows := issueOutboxOf(t, pool, key, string(record.OutboxKindLingerClose)); len(rows) != 0 {
		t.Errorf("the linger_close rows of %s = %d, want none", key, len(rows))
	}
	if containsNotice(noticeKinds(t, pool, key), "child-closed") {
		t.Errorf("the notices of %s hold child-closed, want none: the issue goes on", key)
	}
}

// The merge of one task's pull request makes that task done, and the issue goes on while another
// task runs: of a (merges), at its merge, and b (no needs), at its first step, the merge of a's pull
// request leaves a done at its merge, b as it was, and the issue in implementing, with the end of
// a's merge recorded passed. Nothing of a close is queued: no status done, no issue_close and no
// suspension, no linger's close and no child-closed notice. The merge opens the production check
// of its own merge, a's, as each merge does. The same holds in a lingering tree, where no
// transition runs and the close writes its own rows.
//
// The fault it catches is the merge of a task that is not the last closing the issue (its control:
// closeMerged with no read of lastToFinish, the revision before, whose close in a lingering tree
// writes the status done and tells the architect above child-closed).
func TestTheMergeOfOneTaskMakesItDoneAndTheIssueGoesOn(t *testing.T) {
	for _, tc := range []struct {
		name      string
		lingering bool
	}{
		{"in a tree that runs", false},
		{"in a tree that lingers", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := migratedPool(t)
			engine := returnEngine()
			key, generation, number := "LEGION-208", uint64(1), 42
			if tc.lingering {
				until := stepClock.Add(time.Hour)
				seedIssue(t, pool, record.Issue{Key: "LEGION-208", Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion", Tree: "LEGION-208",
					Project: "LEGION", Title: "root", Phase: phase.Done, Generation: 1, Status: "done", Rank: "U", LingerUntil: &until, LastDispatchSeq: 1})
				child := stepChild(t, pool, "LEGION-209", phase.Implementing, nil)
				seedChildPR(t, pool, record.PullRequestOpen)
				key, generation, number = child.Key, child.Generation, 43
			} else {
				stepRoot(t, pool, phase.Implementing)
			}
			seedAAndB(t, pool, engine, key, generation, atMerge, atStep(1), 45)
			b := tasksOf(t, pool, engine, key)["b"]

			mustSpend(t, "the merge of a's pull request", applyFacts(t, pool, engine)("merged-a",
				intake.PullRequestMerged{Repo: "sjawhar/legion", Number: number, MergeSHA: "merge-a"}))

			tasks := tasksOf(t, pool, engine, key)
			assertTask(t, tasks, "a", record.TaskDone, atMerge)
			if !reflect.DeepEqual(tasks["b"], b) {
				t.Errorf("task b after a's merge = %+v, want it as it was, %+v", tasks["b"], b)
			}
			if issue := issueOf(t, pool, engine, key); issue.Phase != phase.Implementing || issue.Hold != nil || issue.Status == "done" {
				t.Errorf("%s after a's merge stands in %s (hold %+v) with status %q, want implementing, where b stands", key, issue.Phase, issue.Hold, issue.Status)
			}
			if end := actEnd(t, pool, key, "a", record.PlaceMerge); end != "passed" {
				t.Errorf("the end of a's merge is %q, want passed", end)
			}
			assertNoClose(t, pool, key)
			checksAre(t, pool, key, "merge-a")
		})
	}
}

// Only the merge that ends the issue waits for the issues beneath it. With LEGION-209 in the
// workflow beneath LEGION-208, whose a (merges) stands at its merge and b at its retro, the merge
// of a moves a to done at once: a is not the last task to finish. b's retro passes and its merge
// starts, and the merge of b's pull request waits: b is the last, so b stays at its merge, the
// issue stands in merging, and the architect is told once, in a pr-merged notice that names the
// child. When the child leaves the workflow, b is done at its merge, and the issue is done and its
// tree lingers: the merge of the issue is b's, its task with a merge still to come. b stands at its
// retro, not at its first step, at a's merge: a task that passes its last step while a child runs,
// and is the last to finish, is held before its retro (retroHeld), so the close wait meets the
// task whose retro ran beside another's, neither of them the last.
//
// The faults it catches are the merge of a task that is not the last waiting for the whole tree
// (its control: the wait with no read of lastToFinish, the revision before, under which a stays at
// its merge), a last merge that does not wait (its control: closeMerged with no wait at all, under
// which b is done while the child runs), and a last merge that never closes once the tree beneath
// it left (its control: the merge of the issue read from the one task no task needs, which this
// plan does not have, so b stays at its merge).
func TestOnlyTheLastMergeWaitsForTheIssuesBeneath(t *testing.T) {
	pool := migratedPool(t)
	engine := returnEngine()
	root := stepRoot(t, pool, phase.Retro)
	stepChild(t, pool, heldParentChild, phase.Implementing, nil)
	seedAAndB(t, pool, engine, root.Key, root.Generation, atMerge, atRetro, 45)
	apply := applyFacts(t, pool, engine)

	mustSpend(t, "the merge of a's pull request", apply("merged-a", intake.PullRequestMerged{Repo: "sjawhar/legion", Number: 42, MergeSHA: "merge-a"}))
	tasks := tasksOf(t, pool, engine, root.Key)
	assertTask(t, tasks, "a", record.TaskDone, atMerge)
	assertTask(t, tasks, "b", record.TaskRunning, atRetro)
	if kinds := noticeKinds(t, pool, root.Key); containsNotice(kinds, "pr-merged") {
		t.Fatalf("the notices of %s after a's merge = %v, want no pr-merged: a's merge waits for nobody", root.Key, kinds)
	}

	taskMoves(t, pool, engine, root.Key, "b", TriggerRetroCompleted, "")
	assertTask(t, tasksOf(t, pool, engine, root.Key), "b", record.TaskRunning, atMerge)
	mustSpend(t, "the merge of b's pull request", apply("merged-b", intake.PullRequestMerged{Repo: "sjawhar/legion", Number: 45, MergeSHA: "merge-b"}))

	assertTask(t, tasksOf(t, pool, engine, root.Key), "b", record.TaskRunning, atMerge)
	if issue := issueOf(t, pool, engine, root.Key); issue.Phase != phase.Merging {
		t.Fatalf("%s after b's merge stands in %s, want merging: its close waits for %s", root.Key, issue.Phase, heldParentChild)
	}
	want := waitingForTree(root.Key, record.PullRequest{Repo: "sjawhar/legion", Number: 45}, []string{heldParentChild})
	var told []string
	for _, notice := range noticesOfKind(t, pool, root.Key, "pr-merged") {
		told = append(told, notice.Reason)
	}
	if !reflect.DeepEqual(told, []string{want}) {
		t.Fatalf("the pr-merged notices of %s = %q, want the one that names %s: %q", root.Key, told, heldParentChild, want)
	}

	childLeavesDone(t, pool, engine)
	assertTask(t, tasksOf(t, pool, engine, root.Key), "b", record.TaskDone, atMerge)
	if issue := issueOf(t, pool, engine, root.Key); issue.Phase != phase.Done || !issue.Lingers() {
		t.Fatalf("%s once %s left stands in %s (lingers %t), want done and lingering: b's merge was its last", root.Key, heldParentChild, issue.Phase, issue.Lingers())
	}
}

// Only the retro of the task that ends the issue is held for the issues beneath it. Of a and b, each
// `implementer` with no acceptance step, both merging, with LEGION-209 in the workflow beneath
// LEGION-208: a passes its step while b implements, and its retro starts, with no hold and no
// retro-held notice. a's retro passes and its pull request merges, so a is done. b passes its step:
// b is the last task to finish, so it is held before its retro for the issues beneath, hold children,
// with no retro started, and its owner is told once, in a retro-held notice naming the child.
//
// The fault it catches is an early task's retro held for a child, so a task that merges by itself
// waits for the whole tree (its control: retroHeld with no read of lastToFinish, the revision
// before, which holds a).
func TestOnlyTheLastTasksRetroIsHeldForTheIssuesBeneath(t *testing.T) {
	pool := migratedPool(t)
	engine := returnEngine()
	root := stepRoot(t, pool, phase.Implementing)
	stepChild(t, pool, heldParentChild, phase.Implementing, nil)
	seedAAndB(t, pool, engine, root.Key, root.Generation, atStep(1), atStep(1), 45)

	taskMoves(t, pool, engine, root.Key, "a", TriggerImplementationReady, "")
	a := tasksOf(t, pool, engine, root.Key)["a"]
	if a.State != record.TaskRunning || a.Place != atRetro || a.HoldReason != "" {
		t.Fatalf("task a after its step passed is %s at %+v, hold %q, want running at its retro with no hold: b runs", a.State, a.Place, a.HoldReason)
	}
	if starts := actStarts(t, pool, root.Key, "a", record.PlaceRetro); starts != 1 {
		t.Fatalf("the retro of a counts %d starts, want 1: a is not the last task to finish", starts)
	}
	if held := noticesOfKind(t, pool, root.Key, "retro-held"); len(held) != 0 {
		t.Fatalf("the retro-held notices of %s = %+v, want none", root.Key, held)
	}

	taskMoves(t, pool, engine, root.Key, "a", TriggerRetroCompleted, "")
	mustSpend(t, "the merge of a's pull request", applyFacts(t, pool, engine)("merged-a",
		intake.PullRequestMerged{Repo: "sjawhar/legion", Number: 42, MergeSHA: "merge-a"}))
	assertTask(t, tasksOf(t, pool, engine, root.Key), "a", record.TaskDone, atMerge)

	taskMoves(t, pool, engine, root.Key, "b", TriggerImplementationReady, "")
	b := tasksOf(t, pool, engine, root.Key)["b"]
	if b.State != record.TaskHeld || b.Place != atRetro || b.HoldReason != record.HoldForChildren {
		t.Fatalf("task b after its step passed is %s at %+v, hold %q, want held at its retro for the issues beneath", b.State, b.Place, b.HoldReason)
	}
	if starts := actStarts(t, pool, root.Key, "b", record.PlaceRetro); starts != 0 {
		t.Fatalf("the retro of b counts %d starts, want none: it is held", starts)
	}
	held := noticesOfKind(t, pool, root.Key, "retro-held")
	if want := retroHeldNotice(root.Key, acceptanceHold{code: signOffChildInWorkflow, names: heldParentChild}, false).Reason; len(held) != 1 || held[0].Reason != want {
		t.Fatalf("the retro-held notices of %s = %+v, want one: %q", root.Key, held, want)
	}
}

// productionChecksOf is the merge commit of each production_checks row of key, oldest first.
func productionChecksOf(t *testing.T, pool *pgxpool.Pool, key string) []string {
	t.Helper()
	rows, err := pool.Query(t.Context(), `select merge_sha from production_checks where issue = $1 order by opened_at`, key)
	if err != nil {
		t.Fatalf("read the production checks of %s: %v", key, err)
	}
	merges, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatalf("read the production checks of %s: %v", key, err)
	}
	return merges
}

// A version that removes the last task that is not done ends the issue. Of a (merges) and b (no
// needs), a merged and is done, and b, stopped, holds the issue (task_stopped); version 2 removes
// b. The issue stands done, its Dispatch status done is queued, and it leaves the workflow: a root
// lingers its tree (a linger_close row), a child tells the architect above it child-closed. The
// issue holds one production check, of a's merge: the version opens no check of its own.
//
// The fault it catches is an issue whose tasks are all done that never leaves the workflow, so its
// root never lingers and its claims are never retired (its control: planGiven with no leave, which
// leaves the issue held for the stopped task it no longer has, with no linger and no
// child-closed).
func TestAVersionThatRemovesTheLastUndoneTaskEndsTheIssue(t *testing.T) {
	for _, tc := range []struct {
		name  string
		child bool
	}{
		{"a root", false},
		{"a child", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := migratedPool(t)
			engine := returnEngine()
			root := stepRoot(t, pool, phase.Implementing)
			issue, number := root, 42
			if tc.child {
				issue = stepChild(t, pool, heldParentChild, phase.Implementing, nil)
				seedChildPR(t, pool, record.PullRequestOpen)
				number = 43
			}
			seedAAndB(t, pool, engine, issue.Key, issue.Generation, atMerge, atStep(1), 45)
			apply := applyFacts(t, pool, engine)
			mustSpend(t, "the merge of a's pull request", apply("merged-a", intake.PullRequestMerged{Repo: "sjawhar/legion", Number: number, MergeSHA: "merge-a"}))
			if result := apply("stop-b", ownersStop(t, issue, "b")); result.Refusal != nil {
				t.Fatalf("the owner's stop of b was refused: %+v", result.Refusal)
			}
			if held := issueOf(t, pool, engine, issue.Key); !record.HeldFor(held, record.HoldTaskStopped) {
				t.Fatalf("premise: %s with a done and b stopped stands in %s (hold %+v), want held for the stopped task", issue.Key, held.Phase, held.Hold)
			}
			aBlock := keyed(t, "a", "", []string{"implementer"})
			aBlock.Merges = true

			mustTake(t, "version 2, which removes the stopped b", apply("version-2", laterVersion(t, issue.Key, issue.Generation, 2, aBlock)))

			ended := issueOf(t, pool, engine, issue.Key)
			if ended.Phase != phase.Done || ended.Hold != nil {
				t.Fatalf("%s after version 2 stands in %s (hold %+v), want done: each of its tasks is done", issue.Key, ended.Phase, ended.Hold)
			}
			if writes := statusWrites(t, pool, issue.Key); len(writes) == 0 || writes[len(writes)-1].Status != "done" {
				t.Fatalf("the status writes of %s = %+v, want the last of done", issue.Key, writes)
			}
			if tc.child {
				if notices := noticesOfKind(t, pool, issue.Key, "child-closed"); len(notices) != 1 {
					t.Fatalf("the child-closed notices of %s = %+v, want one: the child left the workflow", issue.Key, notices)
				}
			} else if !ended.Lingers() || len(issueOutboxOf(t, pool, issue.Key, string(record.OutboxKindLingerClose))) != 1 {
				t.Fatalf("%s after version 2 lingers %t with %d linger_close rows, want its tree lingering with one", issue.Key, ended.Lingers(),
					len(issueOutboxOf(t, pool, issue.Key, string(record.OutboxKindLingerClose))))
			}
			if checks := productionChecksOf(t, pool, issue.Key); !reflect.DeepEqual(checks, []string{"merge-a"}) {
				t.Fatalf("the production checks of %s are of the merges %v, want one, of a's merge-a", issue.Key, checks)
			}
		})
	}
}
