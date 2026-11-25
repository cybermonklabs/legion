package workflow

import (
	"bytes"
	"log/slog"
	"reflect"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
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
	return stopsAtTesting(t, pool, engine, stepRoot(t, pool, phase.Admitted))
}

// stopsAtTesting walks root, standing at admitted, through the plan `implementer tester` to its
// step 2, where its owner stops its task a, so the root is held from testing for the stopped task.
func stopsAtTesting(t *testing.T, pool *pgxpool.Pool, engine *Engine, root record.Issue) record.Issue {
	t.Helper()
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
	t.Parallel()
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
		// The write carries why the hold stands, written for the person who set the todo and read
		// beside the board's move: the issues the retro waits for. The architect's rules for
		// ending a child stay in the architect's retro-held notice, which this text is not.
		why := root.Key + " waits before its retro while these issues beneath it are still in the workflow: " + heldBeneath + ". " + retroHeldStarts
		if notice := retroHeldNotice(root.Key, acceptanceHold{code: signOffChildInWorkflow, names: heldBeneath}, false).Reason; !strings.Contains(notice, record.EndChildRule) || !strings.Contains(notice, record.StatusNotTakenYet) {
			t.Fatalf("the architect's retro-held notice = %q, want it carrying the rules for ending a child and the sentence on a status not taken yet", notice)
		}
		if got, writes := outboxRowsOf(t, pool, root.Key), statusWrites(t, pool, root.Key); got != outbox+1 || len(writes) != statuses+1 ||
			writes[len(writes)-1] != record.OrdinaryStatusWrite("in_progress", "todo", why) {
			t.Fatalf("the todo queued %d outbox rows of %s, its status writes now %+v; want one, the status the record keeps, in_progress, written back over the todo with the reason %q for the person, and nothing started",
				got-outbox, root.Key, writes, why)
		}
		if line := logged.String(); strings.Count(line, "workflow: a todo leaves the hold of a root that waits before its retro") != 1 || !strings.Contains(line, heldBeneath) {
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

// stoppedWithItsGateOpen is stoppedAtTesting on a root whose gate stands open at version 3 of its
// spec from the start.
func stoppedWithItsGateOpen(t *testing.T, pool *pgxpool.Pool, engine *Engine) record.Issue {
	t.Helper()
	root := stepRoot(t, pool, phase.Admitted)
	seedGate(t, pool, record.DesignGate{Issue: root.Key, ArtifactID: gateDoc, LatestVersion: 3, ApprovedVersion: new(3), EverOpened: true})
	return stopsAtTesting(t, pool, engine, root)
}

// restartsAtStepOne is a person's todo on key, a root held for its stopped task, which restarts
// the task at step 1, in implementing.
func restartsAtStepOne(t *testing.T, pool *pgxpool.Pool, engine *Engine, key string) {
	t.Helper()
	if result := applyFacts(t, pool, engine)("todo", personsTodo(key)); result.Refusal != nil {
		t.Fatalf("the todo was refused: %+v", result.Refusal)
	}
	assertWalksAt(t, pool, engine, key, phase.Implementing, record.TaskPlace{Kind: record.PlaceStep, Position: 1})
}

// A restart takes the issue out of held into the phase of step 1, as a move takes an issue into a
// phase (transition), so after its start it acts on the pull request's merge forward as a move
// does there (restartTask, mergeForwardOnEntry): a move into implementing drops the record's ask,
// since the implementer works the head there, so an ask made before the stop no longer holds back
// a later ask of that head (mergeForward asks once for each head). The news a restart queues is
// TestEachReleaseOutOfHeldQueuesTheReconcileOfNewsItsHoldOwedNothing's. The deployment gates the
// spec of each root, and the root's gate stands open at version 3 from the start.
func TestARestartDoesWhatAMoveDoesAfterItsStart(t *testing.T) {
	t.Parallel()
	t.Run("an ask of the merge forward made before the stop is dropped", func(t *testing.T) {
		pool := migratedPool(t)
		engine := gatedStepEngine(config.DesignGateRootIssues, nil)
		root := stoppedWithItsGateOpen(t, pool, engine)
		pr := recordedPR(t, pool, root.Key)
		pr.MergeForward = &record.MergeForward{Head: pr.HeadSHA, Base: "main", Role: claim.RoleImplementer, Phase: phase.Testing,
			Why: record.ConflictCheckNotGreen, AskedAt: stepClock}
		seedPR(t, pool, pr)

		restartsAtStepOne(t, pool, engine, root.Key)

		if mf := recordedPR(t, pool, root.Key).MergeForward; mf != nil {
			t.Fatalf("after the restart into implementing the record's merge forward = %+v, want none, as a move into implementing leaves it", mf)
		}
	})
}

// Each release out of held puts the issue back in a phase and, after the start of that phase's
// worker, queues the reconcile of the tree's news as a move does (queueMovedNews): a hold owes the
// issue nothing (record.OwedNews names a held issue UntoldHeld), so a version of the design
// approved while it was held would reach the holder of the phase entered with no change set
// recorded, which that start cannot carry. The three releases are the restart of a stopped task
// at step 1 (restartTask); the retry of any other hold back into the phase it is from
// (retryPhase), here a person's todo on a root with no plan rows that a failed claim holds; and
// the start of a parent's retro once nothing beneath it is in the workflow (StartHeldRetro), for
// a parent walking `implementer reviewer`. Each is crossed with its control, a holder that took
// the approved version, which is owed nothing and queues no reconcile. The deployment gates the
// spec of each root, and no reconcile stands queued before the release, so the count is the
// release's own.
func TestEachReleaseOutOfHeldQueuesTheReconcileOfNewsItsHoldOwedNothing(t *testing.T) {
	t.Parallel()
	for _, release := range []struct {
		name string
		// held seeds LEGION-208 held as the release finds it, and answers it.
		held func(t *testing.T, pool *pgxpool.Pool, engine *Engine) record.Issue
		// release takes key out of held and checks where it then stands.
		release func(t *testing.T, pool *pgxpool.Pool, engine *Engine, key string)
	}{
		{name: "the restart of a stopped task", held: stoppedWithItsGateOpen, release: restartsAtStepOne},
		{
			name: "the retry of a held phase",
			held: func(t *testing.T, pool *pgxpool.Pool, engine *Engine) record.Issue {
				t.Helper()
				root := stepRoot(t, pool, phase.Held)
				root.Hold = &record.Hold{From: phase.Implementing}
				seedIssue(t, pool, root)
				return root
			},
			release: func(t *testing.T, pool *pgxpool.Pool, engine *Engine, key string) {
				t.Helper()
				if result := applyFacts(t, pool, engine)("todo", personsTodo(key)); result.Refusal != nil {
					t.Fatalf("the todo was refused: %+v", result.Refusal)
				}
				if issue := issueOf(t, pool, engine, key); issue.Phase != phase.Implementing || issue.Hold != nil {
					t.Fatalf("%s after the todo stands in %s with hold %+v, want implementing with no hold", key, issue.Phase, issue.Hold)
				}
			},
		},
		{
			name: "the start of a held retro",
			held: func(t *testing.T, pool *pgxpool.Pool, engine *Engine) record.Issue {
				t.Helper()
				root := stepRoot(t, pool, phase.Admitted)
				seedGate(t, pool, record.DesignGate{Issue: root.Key, ArtifactID: gateDoc, LatestVersion: 3, ApprovedVersion: new(3), EverOpened: true})
				seedStepsPlan(t, pool, root.Key, root.Generation, "implementer", "reviewer")
				stepChild(t, pool, heldParentChild, phase.Implementing, nil)
				move(t, pool, engine, root.Key, TriggerGateOpened, "", record.PhaseRow{})
				move(t, pool, engine, root.Key, TriggerImplementationReady, "", record.PhaseRow{})
				approveReview(t, pool, engine)
				held := issueOf(t, pool, engine, root.Key)
				if !record.HeldForChildren(held) {
					t.Fatalf("%s after the approval stands in %s with hold %+v, want held from retro for its children", root.Key, held.Phase, held.Hold)
				}
				return held
			},
			release: func(t *testing.T, pool *pgxpool.Pool, engine *Engine, key string) {
				t.Helper()
				childLeavesDone(t, pool, engine)
				assertWalksAt(t, pool, engine, key, phase.Retro, record.TaskPlace{Kind: record.PlaceRetro})
			},
		},
	} {
		for _, tc := range []struct {
			name string
			took int
			want int
		}{
			{name: "a holder that took the version before the approved one", took: 3, want: 1},
			{name: "control: a holder that took the approved version", took: 4, want: 0},
		} {
			t.Run(release.name+", "+tc.name, func(t *testing.T) {
				pool := migratedPool(t)
				engine := gatedStepEngine(config.DesignGateRootIssues, nil)
				root := release.held(t, pool, engine)
				// A person approves version 4 of the spec while the root is held: the hold owes it
				// nothing, so no reconcile stands for it.
				seedGate(t, pool, record.DesignGate{Issue: root.Key, ArtifactID: gateDoc, LatestVersion: 4, ApprovedVersion: new(4), EverOpened: true})
				seedTold(t, pool, root.Key, record.AudienceHolder, map[record.DesignStep]int{record.DesignStepSpec: tc.took})
				seedTold(t, pool, root.Key, record.AudienceArchitect, map[record.DesignStep]int{record.DesignStepSpec: 4})
				assertOutboxCount(t, pool, "design_changes", 0)

				release.release(t, pool, engine, root.Key)

				assertOutboxCount(t, pool, "design_changes", tc.want)
			})
		}
	}
}
