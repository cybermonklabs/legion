package record

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/store"
)

// heldPlanTasks puts version 1 of the plan of planIssue at generation 1: task a running at its
// second step, task b waiting, and task c running at its first step, each with the steps
// implementer and reviewer.
func heldPlanTasks(t *testing.T, tx pgx.Tx) {
	t.Helper()
	v := PlanVersion{Issue: planIssue, Generation: 1, Version: 1, ArtifactID: planAuthor, Source: PlanSourceOwner, GivenBy: planOwner, GivenAt: planGivenAt}
	tasks := []PlanTask{
		{Issue: planIssue, Generation: 1, Task: "a", PlanVersion: 1, BlockText: "Do a.", State: TaskRunning, Place: TaskPlace{Kind: PlaceStep, Position: 2}},
		{Issue: planIssue, Generation: 1, Task: "b", PlanVersion: 1, BlockText: "Do b.", State: TaskWaiting},
		{Issue: planIssue, Generation: 1, Task: "c", PlanVersion: 1, BlockText: "Do c.", State: TaskRunning, Place: TaskPlace{Kind: PlaceStep, Position: 1}},
	}
	var steps []PlanStep
	for _, task := range tasks {
		steps = append(steps,
			PlanStep{Issue: planIssue, Generation: 1, Task: task.Task, Position: 1, Role: "implementer", Type: "implementation"},
			PlanStep{Issue: planIssue, Generation: 1, Task: task.Task, Position: 2, Role: "reviewer", Type: "review"})
	}
	putPlan(t, tx, v, tasks, steps)
}

// refusedEach fails unless each write is refused, each in a transaction of its own so a refusal by
// the database leaves the next one a usable transaction. check names the constraint a write the
// database refuses must break ("" for a write the store refuses before it writes), and the
// refusal of a write the store refuses must name its task.
func refusedEach(t *testing.T, st *store.Store, writes []refusedWrite) {
	t.Helper()
	for _, write := range writes {
		err := store.Transact(context.Background(), st.Pool(), func(ctx context.Context, tx pgx.Tx) error { return write.write(tx) })
		var pgErr *pgconn.PgError
		switch {
		case err == nil:
			t.Errorf("%s was written, want it refused", write.name)
		case write.check != "" && (!errors.As(err, &pgErr) || pgErr.Code != "23514" || pgErr.ConstraintName != write.check):
			t.Errorf("%s = %v, want the check %s to refuse it (23514)", write.name, err, write.check)
		case write.check == "" && !strings.Contains(err.Error(), "task "+write.task+" "):
			t.Errorf("%s = %v, want a refusal naming task %s", write.name, err, write.task)
		}
	}
}

type refusedWrite struct {
	name, task, check string
	write             func(tx pgx.Tx) error
}

// A task is held where it stands, for the reason of its hold: a running task held as its owner
// escalated it keeps its place and reads held with that reason, and its release writes it running
// at the same place with no reason. A hold of a held task takes the new reason, as a failed claim
// its owner then escalates does, and a stop of a held task ends the hold with it. A task that is
// not running or held, a hold for a stop's reason, a release of a task that is not held and a move
// into held are refused before anything is written, and the store itself refuses a hold reason on
// a task that is not held.
func TestAHeldTaskKeepsItsPlaceAndItsReason(t *testing.T) {
	ctx := context.Background()
	st := migratedStore(t)
	inTx(t, st, func(tx pgx.Tx) { heldPlanTasks(t, tx) })
	second := TaskPlace{Kind: PlaceStep, Position: 2}
	refusedEach(t, st, []refusedWrite{
		{name: "a hold of a waiting task", task: "b", write: func(tx pgx.Tx) error {
			return NewStore().HoldTask(ctx, tx, planIssue, 1, "b", HoldEscalated)
		}},
		{name: "a hold for the reason of a stop", task: "a", write: func(tx pgx.Tx) error {
			return NewStore().HoldTask(ctx, tx, planIssue, 1, "a", HoldTaskStopped)
		}},
		{name: "a hold of a task the plan does not hold", task: "z", write: func(tx pgx.Tx) error {
			return NewStore().HoldTask(ctx, tx, planIssue, 1, "z", "")
		}},
		{name: "a release of a running task", task: "a", write: func(tx pgx.Tx) error {
			return NewStore().ReleaseTask(ctx, tx, planIssue, 1, "a")
		}},
		{name: "a move into held", task: "a", write: func(tx pgx.Tx) error {
			return NewStore().MoveTask(ctx, tx, planIssue, 1, "a", second, TaskHeld)
		}},
		{name: "a hold reason on a running task", check: "plan_tasks_hold_reason_held_check", write: func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `update plan_tasks set hold_reason = 'escalated' where issue = $1 and task = 'a'`, planIssue)
			return err
		}},
	})

	inTx(t, st, func(tx pgx.Tx) {
		must(t, NewStore().HoldTask(ctx, tx, planIssue, 1, "a", HoldEscalated))
		if a := taskOf(t, tx, planIssue, 1, "a"); a.State != TaskHeld || a.Place != second || a.HoldReason != HoldEscalated {
			t.Fatalf("task a held as escalated reads %s at %+v for %q, want held at %+v for escalated", a.State, a.Place, a.HoldReason, second)
		}
		must(t, NewStore().ReleaseTask(ctx, tx, planIssue, 1, "a"))
		if a := taskOf(t, tx, planIssue, 1, "a"); a.State != TaskRunning || a.Place != second || a.HoldReason != "" {
			t.Fatalf("task a released reads %s at %+v for %q, want running at %+v for none", a.State, a.Place, a.HoldReason, second)
		}
		must(t, NewStore().HoldTask(ctx, tx, planIssue, 1, "a", ""))
		must(t, NewStore().HoldTask(ctx, tx, planIssue, 1, "a", HoldEscalated))
		if a := taskOf(t, tx, planIssue, 1, "a"); a.State != TaskHeld || a.HoldReason != HoldEscalated {
			t.Fatalf("task a held for a failed claim, then escalated, reads %s for %q, want held for escalated", a.State, a.HoldReason)
		}
		must(t, NewStore().StopTask(ctx, tx, planIssue, 1, "a", "the owner stopped it"))
		if a := taskOf(t, tx, planIssue, 1, "a"); a.State != TaskStopped || a.Place != second || a.HoldReason != "" || a.StoppedReason != "the owner stopped it" {
			t.Fatalf("held task a stopped reads %s at %+v for %q, stopped for %q; want stopped at %+v, no hold reason, for the stop's reason",
				a.State, a.Place, a.HoldReason, a.StoppedReason, second)
		}
	})
}

// The step at a running task's place waits for its task's join or for its agent since a moment,
// and the store keeps the two together: a wait names its moment, no wait names none, and a move of
// the task ends its wait and its hold. A wait of a task that is not running, a wait with no moment,
// no wait with a moment and a wait of no kind are refused before anything is written, and the
// store itself refuses a wait with no moment.
func TestAStepWaitIsWrittenWithItsMomentAndAMoveEndsIt(t *testing.T) {
	ctx := context.Background()
	st := migratedStore(t)
	inTx(t, st, func(tx pgx.Tx) { heldPlanTasks(t, tx) })
	t0, t1 := planGivenAt.Add(time.Minute), planGivenAt.Add(2*time.Minute)
	refusedEach(t, st, []refusedWrite{
		{name: "a wait of a waiting task", task: "b", write: func(tx pgx.Tx) error {
			return NewStore().SetStepWait(ctx, tx, planIssue, 1, "b", StepWaitJoin, &t0)
		}},
		{name: "a wait with no moment", task: "a", write: func(tx pgx.Tx) error {
			return NewStore().SetStepWait(ctx, tx, planIssue, 1, "a", StepWaitJoin, nil)
		}},
		{name: "no wait with a moment", task: "a", write: func(tx pgx.Tx) error {
			return NewStore().SetStepWait(ctx, tx, planIssue, 1, "a", StepWaitNone, &t0)
		}},
		{name: "a wait of no kind", task: "a", write: func(tx pgx.Tx) error {
			return NewStore().SetStepWait(ctx, tx, planIssue, 1, "a", StepWait("review"), &t0)
		}},
		{name: "a wait of a task the plan does not hold", task: "z", write: func(tx pgx.Tx) error {
			return NewStore().SetStepWait(ctx, tx, planIssue, 1, "z", StepWaitJoin, &t0)
		}},
		{name: "a wait for the agent with no moment", check: "plan_tasks_step_wait_since_check", write: func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `update plan_tasks set step_wait = 'agent' where issue = $1 and task = 'a'`, planIssue)
			return err
		}},
	})

	waits := func(tx pgx.Tx, task string, want StepWait, since *time.Time) {
		t.Helper()
		got := taskOf(t, tx, planIssue, 1, task)
		if got.StepWait != want || (got.StepWaitSince == nil) != (since == nil) || (since != nil && !got.StepWaitSince.Equal(*since)) {
			t.Fatalf("the step of task %s waits for %q since %v, want %q since %v", task, got.StepWait, got.StepWaitSince, want, since)
		}
	}
	inTx(t, st, func(tx pgx.Tx) {
		must(t, NewStore().SetStepWait(ctx, tx, planIssue, 1, "a", StepWaitJoin, &t0))
		waits(tx, "a", StepWaitJoin, &t0)
		must(t, NewStore().SetStepWait(ctx, tx, planIssue, 1, "a", StepWaitAgent, &t1))
		waits(tx, "a", StepWaitAgent, &t1)
		must(t, NewStore().SetStepWait(ctx, tx, planIssue, 1, "a", StepWaitNone, nil))
		waits(tx, "a", StepWaitNone, nil)

		must(t, NewStore().SetStepWait(ctx, tx, planIssue, 1, "a", StepWaitAgent, &t1))
		must(t, NewStore().MoveTask(ctx, tx, planIssue, 1, "a", TaskPlace{Kind: PlaceStep, Position: 1}, TaskRunning))
		waits(tx, "a", StepWaitNone, nil)

		must(t, NewStore().HoldTask(ctx, tx, planIssue, 1, "c", HoldEscalated))
		must(t, NewStore().MoveTask(ctx, tx, planIssue, 1, "c", TaskPlace{Kind: PlaceStep, Position: 2}, TaskRunning))
		if c := taskOf(t, tx, planIssue, 1, "c"); c.State != TaskRunning || c.HoldReason != "" {
			t.Fatalf("held task c moved reads %s for %q, want running with no hold reason", c.State, c.HoldReason)
		}
	})
}

// The step that has waited longest for its agent is the one the agent goes to next: of the steps
// of running tasks that wait for agent k, the one whose wait began first, and of two that began at
// one moment the first in task id order. Tasks x and y each have a step of agent k, which waits
// since t2 and t1: AgentWaiter(k) answers y; once y waits no more, x; with x and y both waiting
// since t2, x. A step that waits for its join (w, since t0), a step of another agent (m, since t0)
// and a step that waited for k before its task was stopped (v, since t0) are never k's answer, and
// with no step left waiting for k the answer is "". The agent of a step whose row names none is
// its role (PlanStep.AgentName): u's tester step waits for the agent tester. The store itself
// refuses a wait for the agent with no moment.
//
// The fault it catches is a freed agent that goes to a step other than the one that waited longest
// for it (its controls: the read ordered by task id alone, which answers x; and the read of every
// step's wait whatever it waits for, which answers w).
func TestTheStepThatWaitedLongestForItsAgentIsNext(t *testing.T) {
	ctx := context.Background()
	st := migratedStore(t)
	t0 := planGivenAt.Add(time.Minute)
	t1, t2 := t0.Add(time.Minute), t0.Add(2*time.Minute)
	inTx(t, st, func(tx pgx.Tx) {
		v := PlanVersion{Issue: planIssue, Generation: 1, Version: 1, ArtifactID: planAuthor, Source: PlanSourceOwner, GivenBy: planOwner, GivenAt: planGivenAt}
		var tasks []PlanTask
		var steps []PlanStep
		for _, task := range []struct {
			id    string
			agent claim.Agent
		}{{"m", "m"}, {"v", "k"}, {"w", "k"}, {"x", "k"}, {"y", "k"}} {
			tasks = append(tasks, PlanTask{Issue: planIssue, Generation: 1, Task: task.id, PlanVersion: 1, BlockText: "Do " + task.id + ".",
				State: TaskRunning, Place: TaskPlace{Kind: PlaceStep, Position: 1}})
			steps = append(steps, PlanStep{Issue: planIssue, Generation: 1, Task: task.id, Position: 1, Role: "implementer", Type: "implementation",
				AgentKey: string(task.agent), Agent: task.agent})
		}
		tasks = append(tasks, PlanTask{Issue: planIssue, Generation: 1, Task: "u", PlanVersion: 1, BlockText: "Do u.",
			State: TaskRunning, Place: TaskPlace{Kind: PlaceStep, Position: 2}})
		steps = append(steps,
			PlanStep{Issue: planIssue, Generation: 1, Task: "u", Position: 1, Role: "implementer", Type: "implementation", Agent: "implementer"},
			PlanStep{Issue: planIssue, Generation: 1, Task: "u", Position: 2, Role: "tester", Type: "testing"})
		putPlan(t, tx, v, tasks, steps)
	})
	refusedEach(t, st, []refusedWrite{
		{name: "a wait for the agent with no moment", check: "plan_tasks_step_wait_since_check", write: func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `update plan_tasks set step_wait = 'agent' where issue = $1 and task = 'x'`, planIssue)
			return err
		}},
	})

	next := func(tx pgx.Tx, agent claim.Agent, want string) {
		t.Helper()
		got, err := NewStore().AgentWaiter(ctx, tx, planIssue, 1, agent)
		if err != nil {
			t.Fatalf("the step that waited longest for agent %s: %v", agent, err)
		}
		if got != want {
			t.Fatalf("the step that waited longest for agent %s is task %q's, want task %q's", agent, got, want)
		}
	}
	inTx(t, st, func(tx pgx.Tx) {
		next(tx, "k", "")
		must(t, NewStore().SetStepWait(ctx, tx, planIssue, 1, "v", StepWaitAgent, &t0))
		must(t, NewStore().StopTask(ctx, tx, planIssue, 1, "v", "the owner stopped it"))
		must(t, NewStore().SetStepWait(ctx, tx, planIssue, 1, "w", StepWaitJoin, &t0))
		must(t, NewStore().SetStepWait(ctx, tx, planIssue, 1, "m", StepWaitAgent, &t0))
		must(t, NewStore().SetStepWait(ctx, tx, planIssue, 1, "x", StepWaitAgent, &t2))
		must(t, NewStore().SetStepWait(ctx, tx, planIssue, 1, "y", StepWaitAgent, &t1))
		must(t, NewStore().SetStepWait(ctx, tx, planIssue, 1, "u", StepWaitAgent, &t1))
		next(tx, "k", "y")
		next(tx, "m", "m")
		next(tx, "tester", "u")
		must(t, NewStore().SetStepWait(ctx, tx, planIssue, 1, "y", StepWaitNone, nil))
		next(tx, "k", "x")
		must(t, NewStore().SetStepWait(ctx, tx, planIssue, 1, "y", StepWaitAgent, &t2))
		next(tx, "k", "x")
		must(t, NewStore().SetStepWait(ctx, tx, planIssue, 1, "x", StepWaitNone, nil))
		must(t, NewStore().SetStepWait(ctx, tx, planIssue, 1, "y", StepWaitNone, nil))
		next(tx, "k", "")
	})
}

// A hand-over that carries its run copies each task's hold and its step's wait with the rest of
// where it stands, since the run goes on there; the copy that starts the plan from its start holds
// no task and has no step wait.
func TestACarriedPlanKeepsEachTasksHoldAndWait(t *testing.T) {
	ctx := context.Background()
	st := migratedStore(t)
	t0 := planGivenAt.Add(time.Minute)
	inTx(t, st, func(tx pgx.Tx) {
		heldPlanTasks(t, tx)
		must(t, NewStore().HoldTask(ctx, tx, planIssue, 1, "a", HoldEscalated))
		must(t, NewStore().SetStepWait(ctx, tx, planIssue, 1, "c", StepWaitJoin, &t0))
		must(t, NewStore().CarryPlan(ctx, tx, planIssue, 1, 2, true))
		must(t, NewStore().CarryPlan(ctx, tx, planIssue, 1, 3, false))

		if a := taskOf(t, tx, planIssue, 2, "a"); a.State != TaskHeld || a.HoldReason != HoldEscalated || a.Place != (TaskPlace{Kind: PlaceStep, Position: 2}) {
			t.Fatalf("held task a carried reads %s at %+v for %q, want held at step 2 for escalated", a.State, a.Place, a.HoldReason)
		}
		if c := taskOf(t, tx, planIssue, 2, "c"); c.StepWait != StepWaitJoin || c.StepWaitSince == nil || !c.StepWaitSince.Equal(t0) {
			t.Fatalf("task c carried waits for %q since %v, want join since %v", c.StepWait, c.StepWaitSince, t0)
		}
		for _, task := range []string{"a", "b", "c"} {
			if got := taskOf(t, tx, planIssue, 3, task); got.State != TaskWaiting || got.HoldReason != "" || got.StepWait != StepWaitNone || got.StepWaitSince != nil {
				t.Fatalf("task %s copied from its start reads %s for %q, waiting for %q since %v; want waiting with no hold and no step wait",
					task, got.State, got.HoldReason, got.StepWait, got.StepWaitSince)
			}
		}
	})
}
