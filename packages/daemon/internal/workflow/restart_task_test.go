package workflow

import (
	"bytes"
	"log/slog"
	"reflect"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// personsTodo is a person's move of key to todo in the Dispatch dashboard: the event after the
// one the record took last (stepRoot's sequence 1).
func personsTodo(key string) intake.DispatchIssue {
	return intake.DispatchIssue{Key: key, ResolvedRepo: "sjawhar/legion", Seq: 2, Type: "issue.updated", Status: "todo", Title: "root", Rank: "U"}
}

// restartedBy is the reason the worker of the first step of a task a todo starts again is told.
const restartedBy = "Reason: the issue was set to todo while its task was stopped."

// loggedEngine is returnEngine logging into a buffer the test reads.
func loggedEngine() (*Engine, *bytes.Buffer) {
	engine := returnEngine()
	logged := &bytes.Buffer{}
	engine.log = slog.New(slog.NewTextHandler(logged, nil))
	return engine, logged
}

// stoppedAtTesting is LEGION-208 walking the plan `implementer tester`, its task a stopped by its
// owner at step 2, so the root is held from testing for the stopped task.
func stoppedAtTesting(t *testing.T, pool *pgxpool.Pool, engine *Engine) record.Issue {
	t.Helper()
	root := stepRoot(t, pool, phase.Admitted)
	seedStepsPlan(t, pool, root.Key, root.Generation, "implementer", "tester")
	move(t, pool, engine, root.Key, TriggerGateOpened, "", record.PhaseRow{})
	move(t, pool, engine, root.Key, TriggerImplementationReady, "", record.PhaseRow{})
	if result := applyFacts(t, pool, engine)("stop", ownersStop(t, root, "a")); result.Refusal != nil {
		t.Fatalf("the owner's stop was refused: %+v", result.Refusal)
	}
	held := issueOf(t, pool, engine, root.Key)
	if held.Phase != phase.Held || held.Hold == nil || *held.Hold != (record.Hold{From: phase.Testing, Reason: record.HoldTaskStopped}) {
		t.Fatalf("%s after the stop stands in %s with hold %+v, want held from testing for the stopped task", root.Key, held.Phase, held.Hold)
	}
	return held
}

// A person's todo on a root held because its owner stopped its task starts the task again at its
// first step, in one transaction (restartTask): the hold is cleared, the task runs at step 1 with
// its stop's reason gone, the issue stands in the phase of step 1, and that step's role is
// started at a raised entry with a task that names why; the task's pass rises by one, and no
// pair, round or hook is counted. The step the stop ended is not ended again, and the start of
// step 1 is written. The Dispatch status of step 1's phase is written over the todo Dispatch
// shows. A todo on a root held for the issues beneath it leaves the hold, writes back over the todo
// the status the record keeps, and one log line says why. A task that is done, or left the workflow
// with its issue, after a person moved its stopped root to done or to backlog has nothing left to
// start: a todo on it is refused nothing and starts nothing. The twin: a todo on a root with no plan
// rows that a failed claim holds puts back the phase the hold is from and starts its role, as today.
func TestATodoOnAStoppedRootStartsItsTaskAtStepOne(t *testing.T) {
	t.Run("a task stopped at testing starts again at step 1", func(t *testing.T) {
		pool := migratedPool(t)
		engine, logged := loggedEngine()
		root := stoppedAtTesting(t, pool, engine)
		stopped := taskOf(t, pool, engine, root.Key)
		entry := roleRowOf(t, pool, engine, root.Key, claim.RoleImplementer).Entry
		writes, statuses, prompts := len(stepWrites(t, pool, root.Key)), len(statusWrites(t, pool, root.Key)), len(hookPrompts(t, pool))
		logged.Reset()

		if result := applyFacts(t, pool, engine)("todo", personsTodo(root.Key)); result.Refusal != nil {
			t.Fatalf("the todo was refused: %+v", result.Refusal)
		}

		issue := issueOf(t, pool, engine, root.Key)
		if issue.Phase != phase.Implementing || issue.Hold != nil {
			t.Fatalf("%s after the todo stands in %s with hold %+v, want implementing, the phase of step 1, with no hold", root.Key, issue.Phase, issue.Hold)
		}
		if issue.Status != "in_progress" || issue.DispatchStatus != "todo" || issue.LastDispatchSeq != 2 {
			t.Fatalf("%s after the todo has status %q, Dispatch at %q and sequence %d, want in_progress, todo and 2", root.Key, issue.Status, issue.DispatchStatus, issue.LastDispatchSeq)
		}
		task := taskOf(t, pool, engine, root.Key)
		if task.State != record.TaskRunning || task.StoppedReason != "" || task.Place != (record.TaskPlace{Kind: record.PlaceStep, Position: 1}) {
			t.Fatalf("task a after the todo is %s (%q) at %+v, want running at step 1 with no stop reason", task.State, task.StoppedReason, task.Place)
		}
		want := stopped.Counts
		want.Pass++
		if !sameReturnCounts(task.Counts, want) {
			t.Fatalf("task a counts %+v after the todo, want %+v: the pass raised by one, no pair and no round", task.Counts, want)
		}
		if got := len(hookPrompts(t, pool)); got != prompts {
			t.Fatalf("the todo fired %d hook prompts, want none", got-prompts)
		}
		row := roleRowOf(t, pool, engine, root.Key, claim.RoleImplementer)
		if row.Entry != entry+1 || !row.EntryMovedBack || !strings.Contains(row.EntryTask, restartedBy) {
			t.Fatalf("the implementer's row has entry %d (moved back %v) and task %q, want entry %d, raised as a move back, whose task holds %q",
				row.Entry, row.EntryMovedBack, row.EntryTask, entry+1, restartedBy)
		}
		starts := superviseRowsOf(t, pool, root.Key, "start")
		if start := starts[len(starts)-1]; start.Role != claim.RoleImplementer || start.Phase != phase.Implementing || start.Entry != row.Entry || start.Task != row.EntryTask {
			t.Fatalf("the last start = %+v, want the implementer's start of implementing at entry %d with its row's task", start, row.Entry)
		}
		// The implementer's step of the plan `implementer tester` is step 1 of task a, which the
		// gate's opening started once: the restart is that place's second run.
		added := stepWrites(t, pool, root.Key)[writes:]
		if len(added) != 1 || !isStart(added[0]) || added[0].Role != "implementer" || added[0].ID != StepID(root.Key, root.Generation, record.StepPlace{Task: "a", Kind: record.PlaceStep, Place: 1}, 2) {
			t.Fatalf("the todo wrote the step records %+v, want only the start of the second run of task a's step 1", added)
		}
		var ends []string
		for _, write := range stepWrites(t, pool, root.Key) {
			if isEnd(write) && write.Role == "tester" {
				ends = append(ends, resultOf(write))
			}
		}
		if !reflect.DeepEqual(ends, []string{"stopped"}) {
			t.Fatalf("the tester's step record ends %v, want the one end of the stop, stopped", ends)
		}
		if got := statusWrites(t, pool, root.Key)[statuses:]; len(got) != 1 || got[0] != record.OrdinaryStatusWrite("in_progress", "todo", "") {
			t.Fatalf("the todo queued the status writes %+v, want the one of implementing over the todo Dispatch shows", got)
		}
		line := logged.String()
		if strings.Count(line, "workflow: a stopped task starts again at its first step") != 1 || !strings.Contains(line, "act=task_restarted") ||
			!strings.Contains(line, "the issue was set to todo while its task was stopped") || strings.Contains(line, "workflow: a held phase is released") {
			t.Fatalf("the log of the todo = %q, want one line of the task restarted, act task_restarted, naming the todo, and no release of a held phase", line)
		}
	})

	t.Run("a todo on a root held for its children leaves the hold and says why", func(t *testing.T) {
		pool := migratedPool(t)
		engine, logged := loggedEngine()
		root := stepRoot(t, pool, phase.Retro)
		heldWork{from: phase.Retro, reason: record.HoldForChildren}.stand(t, pool, root)
		task, outbox, statuses := taskOf(t, pool, engine, root.Key), outboxRowsOf(t, pool, root.Key), len(statusWrites(t, pool, root.Key))

		if result := applyFacts(t, pool, engine)("todo", personsTodo(root.Key)); result.Refusal != nil {
			t.Fatalf("the todo was refused: %+v", result.Refusal)
		}

		issue := issueOf(t, pool, engine, root.Key)
		if issue.Phase != phase.Held || issue.Hold == nil || *issue.Hold != (record.Hold{From: phase.Retro, Reason: record.HoldForChildren}) {
			t.Fatalf("%s after the todo stands in %s with hold %+v, want held from retro for its children still", root.Key, issue.Phase, issue.Hold)
		}
		if issue.Status != "in_progress" || issue.DispatchStatus != "todo" || issue.LastDispatchSeq != 2 {
			t.Fatalf("%s after the todo has status %q, Dispatch at %q and sequence %d, want in_progress kept, todo and 2 recorded", root.Key, issue.Status, issue.DispatchStatus, issue.LastDispatchSeq)
		}
		if after := taskOf(t, pool, engine, root.Key); !reflect.DeepEqual(after, task) {
			t.Fatalf("task a after the todo = %+v, want %+v as it stood", after, task)
		}
		// The write carries the hold's retro-held sentence, so the person who set the todo reads why
		// the board went back.
		why := retroHeldNotice(root.Key, []string{heldBeneath}).Reason
		if got, writes := outboxRowsOf(t, pool, root.Key), statusWrites(t, pool, root.Key); got != outbox+1 || len(writes) != statuses+1 ||
			writes[len(writes)-1] != record.OrdinaryStatusWrite("in_progress", "todo", why) {
			t.Fatalf("the todo queued %d outbox rows of %s, its status writes now %+v; want one, the status the record keeps, in_progress, written back over the todo with the hold's reason, and nothing started",
				got-outbox, root.Key, writes)
		}
		if line := logged.String(); strings.Count(line, "workflow: a todo leaves the hold of a root that waits for the issues beneath it") != 1 || !strings.Contains(line, heldBeneath) {
			t.Fatalf("the log of the todo = %q, want one line saying the hold stays, naming %s", line, heldBeneath)
		}
	})

	// A task done, or left the workflow with its issue, after its owner stopped it: a person's move
	// of the root to done, or to backlog, ends the stopped task with its run where it stands
	// (leave, leaveState), and a left task keeps its stop's reason (record.Store.MoveTask). A
	// person's todo after it has nothing of that task to start: it is refused nothing, the root
	// stays done at its generation (a closed root's re-admission is admission's, a stub here), the
	// task keeps its state, its place and its reason, the implementer's entry stands, no start of the
	// implementer is queued, and no step record or status is written.
	for _, tc := range []struct {
		name, status string
		state        record.TaskState
		reason       string
	}{
		{"a task that is done", "done", record.TaskDone, ""},
		{"a task that left the workflow with its issue", "backlog", record.TaskLeft, stopsBy},
	} {
		t.Run(tc.name+" starts nothing at a todo", func(t *testing.T) {
			pool := migratedPool(t)
			engine, _ := loggedEngine()
			root := stoppedAtTesting(t, pool, engine)
			apply := applyFacts(t, pool, engine)
			moved := personsTodo(root.Key)
			moved.Status = tc.status
			if result := apply("move", moved); result.Refusal != nil {
				t.Fatalf("the person's move to %s was refused: %+v", tc.status, result.Refusal)
			}
			task := taskOf(t, pool, engine, root.Key)
			if task.State != tc.state || task.StoppedReason != tc.reason || task.Place != (record.TaskPlace{Kind: record.PlaceStep, Position: 2}) {
				t.Fatalf("task a after the move to %s is %s (%q) at %+v, want %s at step 2 with the reason %q", tc.status, task.State, task.StoppedReason, task.Place, tc.state, tc.reason)
			}
			entry := roleRowOf(t, pool, engine, root.Key, claim.RoleImplementer).Entry
			writes, statuses := len(stepWrites(t, pool, root.Key)), len(statusWrites(t, pool, root.Key))
			starts := len(roleStartsOf(t, pool, root.Key, claim.RoleImplementer))
			todo := personsTodo(root.Key)
			todo.Seq = 3

			if result := apply("todo", todo); result.Refusal != nil {
				t.Fatalf("the todo after the move to %s was refused: %+v", tc.status, result.Refusal)
			}

			if issue := issueOf(t, pool, engine, root.Key); issue.Phase != phase.Done || issue.Generation != root.Generation {
				t.Fatalf("%s after the todo stands in %s at generation %d, want done at %d: nothing of its run starts again", root.Key, issue.Phase, issue.Generation, root.Generation)
			}
			if after := taskOf(t, pool, engine, root.Key); !reflect.DeepEqual(after, task) {
				t.Fatalf("task a after the todo = %+v, want %+v as it stood", after, task)
			}
			if got := roleRowOf(t, pool, engine, root.Key, claim.RoleImplementer).Entry; got != entry {
				t.Fatalf("the implementer's entry is %d after the todo, want %d", got, entry)
			}
			if got := len(roleStartsOf(t, pool, root.Key, claim.RoleImplementer)); got != starts {
				t.Fatalf("the todo queued %d starts of the implementer, want none", got-starts)
			}
			if w, s := len(stepWrites(t, pool, root.Key)), len(statusWrites(t, pool, root.Key)); w != writes || s != statuses {
				t.Fatalf("the todo wrote %d step records and %d status writes, want none", w-writes, s-statuses)
			}
		})
	}

	t.Run("the twin: a root with no plan rows that a failed claim holds is released as today", func(t *testing.T) {
		pool := migratedPool(t)
		engine, logged := loggedEngine()
		root := stepRoot(t, pool, phase.Held)
		root.Hold = &record.Hold{From: phase.Implementing}
		seedIssue(t, pool, root)

		if result := applyFacts(t, pool, engine)("todo", personsTodo(root.Key)); result.Refusal != nil {
			t.Fatalf("the todo was refused: %+v", result.Refusal)
		}

		if issue := issueOf(t, pool, engine, root.Key); issue.Phase != phase.Implementing || issue.Hold != nil {
			t.Fatalf("%s with no plan rows after the todo stands in %s with hold %+v, want implementing with no hold", root.Key, issue.Phase, issue.Hold)
		}
		starts := roleStartsOf(t, pool, root.Key, claim.RoleImplementer)
		if len(starts) != 1 || starts[0].Phase != phase.Implementing || !strings.Contains(starts[0].Task, "Reason: the issue was set to todo while its phase was held.") {
			t.Fatalf("the implementer's starts = %+v, want one of implementing naming the release", starts)
		}
		if got := statusWrites(t, pool, root.Key); len(got) != 1 || got[0] != record.OrdinaryStatusWrite("in_progress", "todo", "") {
			t.Fatalf("the status writes = %+v, want the one of implementing over the todo", got)
		}
		if line := logged.String(); !strings.Contains(line, "workflow: a held phase is released") || strings.Contains(line, "task_restarted") {
			t.Fatalf("the log of the todo = %q, want the release of a held phase and no restart of a task", line)
		}
	})
}
