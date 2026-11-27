package workflow

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// waitsOnItsChild walks LEGION-208, standing at admitted, through the plan of steps words to its
// first step, then files LEGION-209 under it open, and moves it out of implementing as the
// implementer's completion does: the move's start of the next step is held, so the root waits in
// awaiting_children holding it (dispatch://INFRA-3320 R9). It answers the root as the store holds
// it then.
func waitsOnItsChild(t *testing.T, pool *pgxpool.Pool, engine *Engine, words ...string) record.Issue {
	t.Helper()
	root := stepRoot(t, pool, phase.Admitted)
	seedStepsPlan(t, pool, root.Key, root.Generation, words...)
	move(t, pool, engine, root.Key, TriggerGateOpened, "", record.PhaseRow{})
	mirrorChild(t, pool, cwChild, root.Key, "todo", 1)
	move(t, pool, engine, root.Key, TriggerImplementationReady, "", record.PhaseRow{})
	waiting := issueOf(t, pool, engine, root.Key)
	if waiting.Phase != phase.AwaitingChildren || waiting.ChildrenWait == nil {
		t.Fatalf("%s after the implementer's completion stands in %s holding %+v, want awaiting_children", root.Key, waiting.Phase, waiting.ChildrenWait)
	}
	return waiting
}

// returnsFromChildren ends the wait of key's open child and takes key back from awaiting_children,
// as the admission pass does on the pass that finds its last open child gone.
func returnsFromChildren(t *testing.T, pool *pgxpool.Pool, engine *Engine, key string) {
	t.Helper()
	mirrorChild(t, pool, cwChild, key, "done", 2)
	applyInTx(t, pool, engine, func(ctx context.Context, tx pgx.Tx) error {
		issue, err := engine.store.Issue(ctx, tx, key)
		if err != nil {
			return err
		}
		return engine.ReturnFromChildren(ctx, tx, *issue)
	})
}

// A held start of testing, reviewing or accepting comes back as the first step of the issue's walk
// after the implementer's (record.ReturnStep, dispatch://INFRA-3320 R12). The plan `implementer
// reviewer` holds no testing step, so its held reviewing comes back as reviewing, the start it
// held: the task stays at step 2, where it stood through the wait, the reviewer is started, and
// nobody else, on the return's own task, which names the step it starts, the held start and its
// reason. The awaiting-children notice and a refusal over the wait say it goes back to reviewing,
// the start it holds. A return as testing whatever the walk holds would have no place to move the
// task to.
func TestAReturnOnAWalkWithNoTesterStartsTheReviewer(t *testing.T) {
	pool := migratedPool(t)
	engine := returnEngine()
	waiting := waitsOnItsChild(t, pool, engine, "implementer", "reviewer")
	if waiting.ChildrenWait.Start != phase.Reviewing {
		t.Fatalf("%s holds the start of %s, want reviewing", waiting.Key, waiting.ChildrenWait.Start)
	}
	told := noticesOfKind(t, pool, waiting.Key, AwaitingChildrenNoticeKind)
	if len(told) != 1 || !strings.Contains(told[0].Reason, "goes back to the start it held by itself, on a task") {
		t.Fatalf("the awaiting-children notices = %+v, want one that says the parent goes back to the start it held", told)
	}
	refused := applyFacts(t, pool, engine)("stop", ownersStop(t, waiting, "a"))
	refusedWith(t, refused, waitingOnChildren, "goes back to reviewing, the start it holds, by itself")
	testers := len(roleStartsOf(t, pool, waiting.Key, claim.RoleTester))

	returnsFromChildren(t, pool, engine, waiting.Key)

	assertWalksAt(t, pool, engine, waiting.Key, phase.Reviewing, record.TaskPlace{Kind: record.PlaceStep, Position: 2})
	if got := len(roleStartsOf(t, pool, waiting.Key, claim.RoleTester)); got != testers {
		t.Fatalf("the return started the tester %d times, want never: the walk holds no testing step", got-testers)
	}
	reviewers := roleStartsOf(t, pool, waiting.Key, claim.RoleReviewer)
	if len(reviewers) != 1 || reviewers[0].Phase != phase.Reviewing {
		t.Fatalf("the reviewer's starts = %+v, want one of reviewing", reviewers)
	}
	for _, want := range []string{
		waiting.Key + " goes back from awaiting_children now that no Dispatch child of it is open, and starts reviewing, the start it held, " +
			"since no round before the wait ran on a base that carries its children's merges.",
		"The held start of reviewing was for this: " + waiting.ChildrenWait.Reason,
	} {
		if !strings.Contains(reviewers[0].Task, want) {
			t.Fatalf("the reviewer's task %q does not say %q", reviewers[0].Task, want)
		}
	}
}

// The owner's stop_task of a parent waiting in awaiting_children is refused WAITING_ON_CHILDREN,
// naming its open children and ending with the architect's levers over them, as its sign-off and
// its move back are (waitingRefusal, dispatch://INFRA-3320 R11), and writes nothing: no hold is
// from awaiting_children (issues_held_from_check), so the stop's hold could not stand there. The
// task keeps running at the place of the start the parent holds, and the outbox is as it was.
func TestAStopOfAWaitingParentsTaskIsRefused(t *testing.T) {
	pool := migratedPool(t)
	engine := returnEngine()
	waiting := waitsOnItsChild(t, pool, engine, "implementer", "tester")
	outbox := outboxRowsOf(t, pool, waiting.Key)

	result := applyFacts(t, pool, engine)("stop", ownersStop(t, waiting, "a"))

	message := refusedWith(t, result, waitingOnChildren, cwChild+" (todo)")
	for _, want := range []string{
		"It stands in no phase that stop_task acts on, so this stop_task changed nothing",
		"goes back to testing, the start it holds, by itself",
		record.StatusNotTakenYet + " " + record.EndChildRule,
	} {
		if !strings.Contains(message, want) {
			t.Fatalf("the refusal %q does not say %q", message, want)
		}
	}
	assertWalksAt(t, pool, engine, waiting.Key, phase.AwaitingChildren, record.TaskPlace{Kind: record.PlaceStep, Position: 2})
	if got := outboxRowsOf(t, pool, waiting.Key); got != outbox {
		t.Fatalf("the refused stop wrote %d outbox rows, want none", got-outbox)
	}
}

// A restart of a stopped task at its first step is a phase start like any other
// (dispatch://INFRA-3320 R9): a person's todo on a root held for its stopped task, with a Dispatch
// child of the root open, moves the task to step 1 and the root to awaiting_children holding the
// start of implementing, step 1's phase, with the task its worker would have been given, and
// starts nobody. Dispatch is written the status of awaiting_children over the todo, and the
// architect is told once. When the child is done the return starts the implementer on the task
// the restart held, which names why.
func TestARestartUnderAnOpenChildWaitsOnTheChild(t *testing.T) {
	pool := migratedPool(t)
	engine := returnEngine()
	root := stoppedAtTesting(t, pool, engine)
	mirrorChild(t, pool, cwChild, root.Key, "todo", 1)
	implementers, statuses := len(roleStartsOf(t, pool, root.Key, claim.RoleImplementer)), len(statusWrites(t, pool, root.Key))

	if result := applyFacts(t, pool, engine)("todo", personsTodo(root.Key)); result.Refusal != nil {
		t.Fatalf("the todo was refused: %+v", result.Refusal)
	}

	waiting := issueOf(t, pool, engine, root.Key)
	if waiting.Phase != phase.AwaitingChildren || waiting.Hold != nil || waiting.ChildrenWait == nil ||
		waiting.ChildrenWait.Start != phase.Implementing || !strings.Contains(waiting.ChildrenWait.Task, restartedBy) {
		t.Fatalf("%s after the todo stands in %s (hold %+v) holding %+v, want awaiting_children holding implementing on a task that holds %q",
			root.Key, waiting.Phase, waiting.Hold, waiting.ChildrenWait, restartedBy)
	}
	if task := taskOf(t, pool, engine, root.Key); task.State != record.TaskRunning || task.StoppedReason != "" ||
		task.Place != (record.TaskPlace{Kind: record.PlaceStep, Position: 1}) {
		t.Fatalf("task a after the todo is %s (%q) at %+v, want running at step 1 with no stop reason", task.State, task.StoppedReason, task.Place)
	}
	if got := len(roleStartsOf(t, pool, root.Key, claim.RoleImplementer)); got != implementers {
		t.Fatalf("the todo started the implementer %d times, want never while the child is open", got-implementers)
	}
	if got := statusWrites(t, pool, root.Key)[statuses:]; len(got) != 1 || got[0] != record.OrdinaryStatusWrite("in_progress", "todo", "") {
		t.Fatalf("the todo queued the status writes %+v, want the one of awaiting_children over the todo Dispatch shows", got)
	}
	if told := noticesOfKind(t, pool, root.Key, AwaitingChildrenNoticeKind); len(told) != 1 || told[0].Phase != phase.Testing ||
		!strings.Contains(told[0].Reason, "holding the start of implementing: the issue was set to todo while its task was stopped.") {
		t.Fatalf("the awaiting-children notices = %+v, want one of the restart held, from testing", told)
	}

	returnsFromChildren(t, pool, engine, root.Key)

	assertWalksAt(t, pool, engine, root.Key, phase.Implementing, record.TaskPlace{Kind: record.PlaceStep, Position: 1})
	started := roleStartsOf(t, pool, root.Key, claim.RoleImplementer)
	if len(started) != implementers+1 || !strings.Contains(started[len(started)-1].Task, restartedBy) {
		t.Fatalf("the implementer's starts = %+v, want one more, on the task the restart held, which holds %q", started, restartedBy)
	}
}
