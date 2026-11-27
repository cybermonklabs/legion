package workflow

import (
	"context"
	"reflect"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// seedAAtItsMerge writes on LEGION-208 the plan of a (`implementer`, whose block says merges),
// standing at its merge on legion/LEGION-208.a with its pull request #7 open at head h7, and b
// (`implementer`, no needs), at its first step on the issue's own branch, with stepRoot's #42.
func seedAAtItsMerge(t *testing.T, pool *pgxpool.Pool, root record.Issue) {
	t.Helper()
	seedTasks(t, pool, root.Key, root.Generation,
		planTask{id: "a", steps: []string{"implementer"}, agents: []claim.Agent{"impl-a"}, state: record.TaskRunning, place: atMerge},
		planTask{id: "b", steps: []string{"implementer"}, agents: []claim.Agent{"impl-b"}, state: record.TaskRunning, place: atStep(1)})
	markMerges(t, pool, root.Key, "a")
	seedTaskBranch(t, pool, root.Key, "a", "legion/"+root.Key+".a")
	seedPR(t, pool, record.PullRequest{State: record.PullRequestOpen, Issue: root.Key, Repo: "sjawhar/legion", Number: 7,
		Branch: "legion/" + root.Key + ".a", HeadSHA: "h7", Failing: []string{}})
}

// mergingAtNow is MergingAt of pull request number of key's own repository ("") at head, read in one
// transaction: the pull request it answers, and why the workflow no longer works that merge, "" while
// it does.
func mergingAtNow(t *testing.T, pool *pgxpool.Pool, engine *Engine, key string, number int, head string) (*record.PullRequest, string) {
	t.Helper()
	var pr *record.PullRequest
	var why string
	applyInTx(t, pool, engine, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		_, pr, why, err = MergingAt(ctx, engine.store, tx, key, "", number, head)
		return err
	})
	return pr, why
}

// A removal from the merge queue of one task's pull request reaches its merger while another task
// runs: of a (merges), at its merge with #7 open at h7, and b (no needs), implementing, so the issue
// stands in implementing, MergingAt of #7 at h7 answers that the workflow works a's merge there.
// Once a is moved back to its first step, it answers why it no longer does, by a's place.
//
// The fault it catches is a task's pull request taken out of the queue while another task
// implements, of which the merger is never told, so the merge waits for ever (its control:
// leftMergingAt reading the issue's phase, the revision before, which answers "the issue stands in
// implementing, not merging").
func TestAQueueRemovalOfOneTasksPullRequestReachesItsMerger(t *testing.T) {
	pool := migratedPool(t)
	engine := returnEngine()
	root := stepRoot(t, pool, phase.Implementing)
	seedAAtItsMerge(t, pool, root)

	pr, why := mergingAtNow(t, pool, engine, root.Key, 7, "h7")
	if why != "" || pr == nil || pr.Number != 7 || pr.Task != "a" {
		t.Fatalf("MergingAt of #7 at h7 = %+v, %q; want a's #7 with no reason: the workflow works a's merge at h7", pr, why)
	}

	seedRecord(t, pool, func(tx pgx.Tx) error {
		return seedMove(t.Context(), tx, root.Key, root.Generation, "a", atStep(1), record.TaskRunning)
	})
	if _, why := mergingAtNow(t, pool, engine, root.Key, 7, "h7"); why != "task a stands at step 1 (implementer), not at its merge" {
		t.Fatalf("MergingAt of #7 at h7 once a is back at its first step answers %q, want a's place", why)
	}
}

// The merger's move back returns the task whose merge it runs, and that task alone: of a (merges),
// at its merge, and b, implementing, the merger's request_backward_move to implementing moves a to
// its first step with its implementer started. b's row, its place and its counts, is as it was, so
// no review round is counted for it.
//
// The fault it catches is a move back of one task's merge that moves or counts another task (its
// control: the move read on the issue's other task, b, which moves b and leaves a at its merge).
func TestTheMergersMoveBackReturnsItsTaskAlone(t *testing.T) {
	pool := migratedPool(t)
	engine := returnEngine()
	root := stepRoot(t, pool, phase.Implementing)
	seedAAtItsMerge(t, pool, root)
	b := tasksOf(t, pool, engine, root.Key)["b"]
	starts := len(roleStartsOf(t, pool, root.Key, claim.RoleImplementer))

	mustSpend(t, "the merger's move back", applyFacts(t, pool, engine)("merger-back", intake.BackwardMove{Issue: root.Key, Requester: claim.RoleMerger,
		To: phase.Implementing, Reason: "the merge queue refused the head", By: claim.Token(treeRoleToken(t, root.Key, claim.RoleMerger))}))

	tasks := tasksOf(t, pool, engine, root.Key)
	assertWaits(t, tasks, "a", atStep(1), record.StepWaitNone)
	implementers := roleStartsOf(t, pool, root.Key, claim.RoleImplementer)
	if len(implementers) != starts+1 {
		t.Fatalf("the implementer's starts of %s after the merger's move back = %d, want %d: a's implementer starts", root.Key, len(implementers), starts+1)
	}
	assertStartOf(t, implementers[len(implementers)-1], root.Key, "a", "impl-a")
	if !reflect.DeepEqual(tasks["b"], b) {
		t.Fatalf("task b after the merger moved a back = %+v, want it as it was, %+v", tasks["b"], b)
	}
}
