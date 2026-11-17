package workflow

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// ownersStop is the stop_task of task of key by the architect of LEGION-208, the tree's root, as
// the route hands it to the workflow (api's stopTask).
func ownersStop(t *testing.T, key, task string) intake.TaskStopped {
	t.Helper()
	return intake.TaskStopped{Issue: key, Task: task, By: claim.Token(token(t, "LEGION-208", claim.RoleArchitect))}
}

// stopsBy is the reason a task stopped by its owner keeps, and the reason its step's process is
// stopped with.
const stopsBy = "task a of LEGION-208 was stopped by its owner"

// The owner's stop_task of a task that is not done stops it where it stands, in one transaction
// (R16; C2-1): the task is stopped with its reason and keeps its place, its issue is held from
// the phase of that place for the stopped task, keeping the slot of that phase and writing no
// Dispatch status, the record of the running step ends stopped, and the process of the step's
// role is stopped with its session kept (step_stop, whose source is the daemon's). The caller
// is answered by its own call, and the architect the issue's notices reach is told in a
// task-stopped notice that names who stopped the task (the daemon's notice executor publishes it
// only to an architect that is not the one that stopped it). A merge GitHub still lands on a task
// stopped at its merge closes the issue, which a hold from merging of an issue with no plan rows
// does not. A stopped task whose issue then leaves the workflow is left, and keeps why it was
// stopped. An issue with no plan rows has no task to stop (R20), and a stop in a lingering tree
// changes nothing.
func TestStopTaskHoldsTheIssueAndStopsTheStep(t *testing.T) {
	t.Run("at testing", func(t *testing.T) {
		pool := migratedPool(t)
		engine := returnEngine()
		root := stepRoot(t, pool, phase.Admitted)
		seedStepsPlan(t, pool, root.Key, root.Generation, "implementer", "tester", "reviewer")
		move(t, pool, engine, root.Key, TriggerGateOpened, "", record.PhaseRow{})
		move(t, pool, engine, root.Key, TriggerImplementationReady, "", record.PhaseRow{})
		statuses := len(issueOutboxOf(t, pool, root.Key, string(record.OutboxKindDispatchStatus)))
		stop := ownersStop(t, root.Key, "a")

		if result := applyFacts(t, pool, engine)("stop", stop); result.Refusal != nil {
			t.Fatalf("the owner's stop was refused: %+v", result.Refusal)
		}

		issue := issueOf(t, pool, engine, root.Key)
		if issue.Phase != phase.Held || issue.Hold == nil || *issue.Hold != (record.Hold{From: phase.Testing, Reason: record.HoldTaskStopped}) {
			t.Fatalf("%s stands in %s with hold %+v, want held from testing for the stopped task", root.Key, issue.Phase, issue.Hold)
		}
		if !record.KeepsSlot(issue) {
			t.Fatalf("%s held from testing keeps no slot, want the slot of testing kept", root.Key)
		}
		if task := taskOf(t, pool, engine, root.Key); task.State != record.TaskStopped || task.StoppedReason != stopsBy ||
			task.Place != (record.TaskPlace{Kind: record.PlaceStep, Position: 2}) {
			t.Fatalf("task a is %s (%q) at %+v, want stopped at step 2 with the reason %q", task.State, task.StoppedReason, task.Place, stopsBy)
		}
		if stops := superviseRowsOf(t, pool, root.Key, "step_stop"); len(stops) != 1 || stops[0].Role != claim.RoleTester ||
			stops[0].Generation != root.Generation || stops[0].Reason != stopsBy {
			t.Fatalf("the step stops = %+v, want one of the tester naming why", stops)
		}
		if suspends := superviseRowsOf(t, pool, root.Key, "suspend"); len(suspends) != 0 {
			t.Fatalf("the stop wrote the suspends %+v, want none: a stopped task's issue stays in the workflow", suspends)
		}
		if got := len(issueOutboxOf(t, pool, root.Key, string(record.OutboxKindDispatchStatus))); got != statuses {
			t.Fatalf("the stop wrote %d Dispatch status rows, want none: held has no status", got-statuses)
		}
		writes := stepWrites(t, pool, root.Key)
		if end := writes[len(writes)-1]; !isEnd(end) || end.Role != "tester" || resultOf(end) != "stopped" || end.ReturnedTo != nil {
			t.Fatalf("the last step record write = %+v (result %q), want the tester's end, stopped", end, resultOf(end))
		}
		notices := noticesOf(t, pool, root.Key, "task-stopped")
		want := record.Notice{Kind: "task-stopped", Role: claim.RoleTester, Phase: phase.Testing, StoppedBy: stop.By,
			Reason: stopsBy + ", the architect legion-legion-legion-208-architect"}
		if len(notices) != 1 || !reflect.DeepEqual(notices[0], want) {
			t.Fatalf("the task-stopped notices = %+v, want the one %+v", notices, want)
		}

		// A stop of a task already stopped changes nothing, and is no refusal.
		before := standingOf(t, pool, engine, root.Key, true)
		if result := applyFacts(t, pool, engine)("stop-again", stop); result.Refusal != nil {
			t.Fatalf("the owner's second stop was refused: %+v", result.Refusal)
		}
		if after := standingOf(t, pool, engine, root.Key, true); after.outbox != before.outbox || after.issue.Phase != before.issue.Phase ||
			!reflect.DeepEqual(after.task, before.task) {
			t.Fatalf("the second stop changed %s:\n before %+v\n after  %+v", root.Key, before, after)
		}

		// The stranded report passes over the tester's claim the stop suspended: the daemon
		// suspended it, and no rule resumes a held issue's step. The same claim an operator
		// suspended is listed, since only the operator's resume runs it again.
		for _, tc := range []struct {
			by       supervise.SuspendSource
			stranded int
		}{{supervise.SuspendedByDaemon, 0}, {supervise.SuspendedByOperator, 1}} {
			tester := supervise.Claim{Token: claim.Token(token(t, root.Key, claim.RoleTester)), Project: "legion", Tree: root.Key, Issue: root.Key,
				Role: claim.RoleTester, Generation: root.Generation, State: supervise.StateSuspended, SuspendedBy: tc.by, Session: "ses-tester"}
			var got []supervise.Claim
			applyInTx(t, pool, func(ctx context.Context, tx pgx.Tx) error {
				var err error
				got, err = Stranded(ctx, tx, engine.store, "LEGION", nil, []supervise.Claim{tester}, nil, stepClock.Add(time.Hour))
				return err
			})
			if len(got) != tc.stranded {
				t.Fatalf("the stranded report over the tester suspended by %s lists %v, want %d", tc.by, got, tc.stranded)
			}
		}
	})

	t.Run("at accepting the owner's own step stops no process", func(t *testing.T) {
		pool := migratedPool(t)
		engine := returnEngine()
		root := stepRoot(t, pool, phase.Admitted)
		seedStepsPlan(t, pool, root.Key, root.Generation, "implementer", "tester", "acceptance:owner")
		move(t, pool, engine, root.Key, TriggerGateOpened, "", record.PhaseRow{})
		move(t, pool, engine, root.Key, TriggerImplementationReady, "", record.PhaseRow{})
		move(t, pool, engine, root.Key, TriggerTesterPassed, "", record.PhaseRow{})

		if result := applyFacts(t, pool, engine)("stop", ownersStop(t, root.Key, "a")); result.Refusal != nil {
			t.Fatalf("the owner's stop was refused: %+v", result.Refusal)
		}

		if issue := issueOf(t, pool, engine, root.Key); issue.Phase != phase.Held || issue.Hold == nil || *issue.Hold != (record.Hold{From: phase.Accepting, Reason: record.HoldTaskStopped}) {
			t.Fatalf("%s stands in %s with hold %+v, want held from accepting for the stopped task", root.Key, issue.Phase, issue.Hold)
		}
		if stops := superviseRowsOf(t, pool, root.Key, "step_stop"); len(stops) != 0 {
			t.Fatalf("the step stops = %+v, want none: the owner's own step runs no process of Legion's", stops)
		}
		if task := taskOf(t, pool, engine, root.Key); task.State != record.TaskStopped || task.Place != (record.TaskPlace{Kind: record.PlaceStep, Position: 3}) {
			t.Fatalf("task a is %s at %+v, want stopped at step 3", task.State, task.Place)
		}
	})

	t.Run("at merging the merger stops, and a merge GitHub still lands closes the issue, which a hold of no plan rows does not", func(t *testing.T) {
		pool := migratedPool(t)
		engine := returnEngine()
		root := stepRoot(t, pool, phase.Admitted)
		seedStepsPlan(t, pool, root.Key, root.Generation, "implementer")
		move(t, pool, engine, root.Key, TriggerGateOpened, "", record.PhaseRow{})
		move(t, pool, engine, root.Key, TriggerImplementationReady, "", record.PhaseRow{})
		move(t, pool, engine, root.Key, TriggerRetroCompleted, "", record.PhaseRow{})
		assertWalksAt(t, pool, engine, root.Key, phase.Merging, record.TaskPlace{Kind: record.PlaceMerge})
		apply := applyFacts(t, pool, engine)

		if result := apply("stop", ownersStop(t, root.Key, "a")); result.Refusal != nil {
			t.Fatalf("the owner's stop was refused: %+v", result.Refusal)
		}
		if stops := superviseRowsOf(t, pool, root.Key, "step_stop"); len(stops) != 1 || stops[0].Role != claim.RoleMerger {
			t.Fatalf("the step stops = %+v, want one of the merger", stops)
		}

		if result := apply("merged", intake.PullRequestMerged{Repo: "sjawhar/legion", Number: 42, MergeSHA: "merge-42"}); result.Refusal != nil {
			t.Fatalf("the merge was refused: %+v", result.Refusal)
		}
		if issue := issueOf(t, pool, engine, root.Key); issue.Phase != phase.Done || issue.Hold != nil {
			t.Fatalf("%s stands in %s (hold %+v) after its merge, want done", root.Key, issue.Phase, issue.Hold)
		}
		if task := taskOf(t, pool, engine, root.Key); task.State != record.TaskDone {
			t.Fatalf("task a is %s after its issue's merge, want done", task.State)
		}
		// The stop ended the merger's run; the merge that closes the issue ends it no second time.
		var ends []string
		for _, write := range stepWrites(t, pool, root.Key) {
			if isEnd(write) && write.Role == "merger" {
				ends = append(ends, resultOf(write))
			}
		}
		if !reflect.DeepEqual(ends, []string{"stopped"}) {
			t.Fatalf("the merger's step record ends %v, want the one end of the stop, stopped", ends)
		}

		// The R20 twin: an issue with no plan rows held from merging, by a failed merger's claim,
		// is not closed by its merge, as today: what ends that hold closes it on the merge recorded.
		unplanned := migratedPool(t)
		held := stepRoot(t, unplanned, phase.Held)
		applyInTx(t, unplanned, func(ctx context.Context, tx pgx.Tx) error {
			held.Hold = &record.Hold{From: phase.Merging}
			return engine.store.PutIssue(ctx, tx, held)
		})
		if result := applyFacts(t, unplanned, engine)("merged", intake.PullRequestMerged{Repo: "sjawhar/legion", Number: 42, MergeSHA: "merge-42"}); result.Refusal != nil {
			t.Fatalf("the merge of the issue with no plan rows was refused: %+v", result.Refusal)
		}
		if issue := issueOf(t, unplanned, engine, held.Key); issue.Phase != phase.Held || issue.Hold == nil || *issue.Hold != (record.Hold{From: phase.Merging}) {
			t.Fatalf("%s with no plan rows stands in %s (hold %+v) after its merge, want still held from merging", held.Key, issue.Phase, issue.Hold)
		}
		want := record.Notice{Kind: "pr-merged", Role: claim.RoleArchitect, Reason: "pull request #42 merged while LEGION-208 was in held, not merging"}
		if notices := noticesOf(t, unplanned, held.Key, "pr-merged"); len(notices) != 1 || !reflect.DeepEqual(notices[0], want) {
			t.Fatalf("the pr-merged notices = %+v, want the one %+v", notices, want)
		}
	})

	t.Run("a task the plan does not hold, and an issue with no plan rows, change nothing", func(t *testing.T) {
		pool := migratedPool(t)
		engine := returnEngine()
		planned := stepRoot(t, pool, phase.Admitted)
		seedStepsPlan(t, pool, planned.Key, planned.Generation, "implementer", "tester")
		move(t, pool, engine, planned.Key, TriggerGateOpened, "", record.PhaseRow{})
		apply := applyFacts(t, pool, engine)
		for _, tc := range []struct {
			name    string
			fact    intake.TaskStopped
			planned bool
			want    *intake.Refusal
		}{
			{"a task the plan does not hold", ownersStop(t, planned.Key, "b"), true, intake.TaskNotFound(planned.Key, "b", []string{"a"})},
		} {
			before := standingOf(t, pool, engine, planned.Key, tc.planned)
			result := apply(tc.name, tc.fact)
			if result.Refusal == nil || result.Refusal.Status != tc.want.Status || result.Refusal.Code != tc.want.Code || result.Refusal.Message != tc.want.Message {
				t.Fatalf("%s = %+v, want %+v", tc.name, result.Refusal, tc.want)
			}
			if after := standingOf(t, pool, engine, planned.Key, tc.planned); after.outbox != before.outbox || after.issue.Phase != before.issue.Phase || !reflect.DeepEqual(after.task, before.task) {
				t.Fatalf("%s changed %s:\n before %+v\n after  %+v", tc.name, planned.Key, before, after)
			}
		}

		unplanned := migratedPool(t)
		root := stepRoot(t, unplanned, phase.Testing)
		before := standingOf(t, unplanned, engine, root.Key, false)
		result := applyFacts(t, unplanned, engine)("stop", ownersStop(t, root.Key, "a"))
		if want := intake.TaskNotFound(root.Key, "a", nil); result.Refusal == nil || result.Refusal.Code != want.Code || result.Refusal.Message != want.Message {
			t.Fatalf("the stop of a task of an issue with no plan rows = %+v, want %+v", result.Refusal, want)
		}
		if after := standingOf(t, unplanned, engine, root.Key, false); after.outbox != before.outbox || after.issue.Phase != phase.Testing || after.issue.Hold != nil {
			t.Fatalf("the refused stop changed %s:\n before %+v\n after  %+v", root.Key, before, after)
		}
	})

	t.Run("a stop in a lingering tree changes nothing", func(t *testing.T) {
		pool := migratedPool(t)
		engine := returnEngine()
		root := stepRoot(t, pool, phase.Admitted)
		seedStepsPlan(t, pool, root.Key, root.Generation, "implementer", "tester")
		move(t, pool, engine, root.Key, TriggerGateOpened, "", record.PhaseRow{})
		lingering := issueOf(t, pool, engine, root.Key)
		until := stepClock.Add(time.Hour)
		lingering.LingerUntil = &until
		applyInTx(t, pool, func(ctx context.Context, tx pgx.Tx) error { return engine.store.PutIssue(ctx, tx, lingering) })
		before := standingOf(t, pool, engine, root.Key, true)
		result := applyFacts(t, pool, engine)("stop", ownersStop(t, root.Key, "a"))
		want := "the tree LEGION-208 is lingering after it left the workflow, so the owner's stop of task a of LEGION-208 changed nothing"
		if result.Refusal == nil || result.Refusal.Code != "TREE_LINGERING" || result.Refusal.Message != want {
			t.Fatalf("the stop in a lingering tree = %+v, want TREE_LINGERING %q", result.Refusal, want)
		}
		if after := standingOf(t, pool, engine, root.Key, true); after.outbox != before.outbox || after.issue.Phase != before.issue.Phase || !reflect.DeepEqual(after.task, before.task) {
			t.Fatalf("the refused stop changed %s:\n before %+v\n after  %+v", root.Key, before, after)
		}
	})

	t.Run("a stopped task whose issue leaves the workflow is left and keeps why it was stopped", func(t *testing.T) {
		pool := migratedPool(t)
		engine := returnEngine()
		root := stepRoot(t, pool, phase.Admitted)
		seedStepsPlan(t, pool, root.Key, root.Generation, "implementer", "tester")
		move(t, pool, engine, root.Key, TriggerGateOpened, "", record.PhaseRow{})
		if result := applyFacts(t, pool, engine)("stop", ownersStop(t, root.Key, "a")); result.Refusal != nil {
			t.Fatalf("the owner's stop was refused: %+v", result.Refusal)
		}
		leave := func(pool *pgxpool.Pool, key string) {
			t.Helper()
			applyInTx(t, pool, func(ctx context.Context, tx pgx.Tx) error {
				issue, err := engine.store.Issue(ctx, tx, key)
				if err != nil {
					return err
				}
				return engine.leave(ctx, tx, *issue, "backlog")
			})
		}
		leave(pool, root.Key)
		if task := taskOf(t, pool, engine, root.Key); task.State != record.TaskLeft || task.StoppedReason != stopsBy ||
			task.Place != (record.TaskPlace{Kind: record.PlaceStep, Position: 1}) {
			t.Fatalf("task a after its issue left = %s (%q) at %+v, want left at step 1 with the reason %q", task.State, task.StoppedReason, task.Place, stopsBy)
		}

		// The R20 twin: an issue with no plan rows leaves as today, and has no task rows after it.
		unplanned := migratedPool(t)
		bare := stepRoot(t, unplanned, phase.Implementing)
		leave(unplanned, bare.Key)
		applyInTx(t, unplanned, func(ctx context.Context, tx pgx.Tx) error {
			stored, err := engine.store.Plan(ctx, tx, bare.Key, bare.Generation)
			if err == nil && stored != nil {
				t.Errorf("the issue with no plan rows has the plan %+v after it left, want none", stored)
			}
			return err
		})
		if issue := issueOf(t, unplanned, engine, bare.Key); issue.Phase != phase.Done || issue.LingerUntil == nil {
			t.Fatalf("%s with no plan rows stands in %s (linger %v) after it left, want done and lingering, as today", bare.Key, issue.Phase, issue.LingerUntil)
		}
	})

	// A task that is done, or that left the workflow with its issue before it was done, has
	// nothing left to stop: each stop is refused with its own text before the store's stop, which
	// refuses both states itself and would fail the whole fact, and changes nothing. A child's
	// leave lingers no tree, so its stop would reach the store's stop; a root's leave lingers its
	// tree, and the task's own state is still what the refusal says, not the linger.
	t.Run("a task that is done and a task that left are refused and change nothing", func(t *testing.T) {
		leave := func(t *testing.T, pool *pgxpool.Pool, engine *Engine, key, status string) {
			t.Helper()
			applyInTx(t, pool, func(ctx context.Context, tx pgx.Tx) error {
				issue, err := engine.store.Issue(ctx, tx, key)
				if err != nil {
					return err
				}
				return engine.leave(ctx, tx, *issue, status)
			})
		}
		const doneText = "is done, so nothing of it is left to stop. This stop changed nothing."
		const leftText = "left the workflow with its issue before it was done, so nothing of it is left to stop. This stop changed nothing."
		for _, tc := range []struct {
			name, key, status, state, want string
		}{
			{name: "a child's task that is done", key: "LEGION-209", status: "done", state: record.TaskDone, want: "task a of LEGION-209 " + doneText},
			{name: "a child's task that left the workflow with its issue", key: "LEGION-209", status: "backlog", state: record.TaskLeft, want: "task a of LEGION-209 " + leftText},
			{name: "a root's task that left the workflow with its issue", key: "LEGION-208", status: "backlog", state: record.TaskLeft, want: "task a of LEGION-208 " + leftText},
		} {
			t.Run(tc.name, func(t *testing.T) {
				pool := migratedPool(t)
				engine := returnEngine()
				root := stepRoot(t, pool, phase.Admitted)
				issue := root
				if tc.key != root.Key {
					issue = stepChild(t, pool, tc.key, phase.Admitted, nil)
				}
				seedStepsPlan(t, pool, issue.Key, issue.Generation, "implementer")
				move(t, pool, engine, issue.Key, TriggerGateOpened, "", record.PhaseRow{})
				leave(t, pool, engine, issue.Key, tc.status)
				if task := taskOf(t, pool, engine, issue.Key); task.State != tc.state {
					t.Fatalf("task a of %s is %s before the stop, want %s", issue.Key, task.State, tc.state)
				}
				before := standingOf(t, pool, engine, issue.Key, true)
				result := applyFacts(t, pool, engine)("stop", ownersStop(t, issue.Key, "a"))
				if result.Refusal == nil || result.Refusal.Status != 409 || result.Refusal.Code != "TASK_DONE" || result.Refusal.Message != tc.want {
					t.Fatalf("the stop of %s = %+v, want 409 TASK_DONE %q", tc.name, result.Refusal, tc.want)
				}
				if after := standingOf(t, pool, engine, issue.Key, true); after.outbox != before.outbox || !reflect.DeepEqual(after.issue, before.issue) || !reflect.DeepEqual(after.task, before.task) {
					t.Fatalf("the refused stop changed %s:\n before %+v\n after  %+v", issue.Key, before, after)
				}
			})
		}
	})
}
