package workflow

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// dropPlanRows deletes every plan row of key, as the way back of the release whose owner's plan
// picks the steps drops the three tables.
func dropPlanRows(t *testing.T, pool *pgxpool.Pool, key string) {
	t.Helper()
	for _, table := range []string{"plan_steps", "plan_tasks", "issue_plans"} {
		if _, err := pool.Exec(t.Context(), "delete from "+table+" where issue = $1", key); err != nil {
			t.Fatal(err)
		}
	}
}

// stepRunsOf is each row of starts of key, `<task> <kind> <place> <starts> <run>`, in the store's order.
func stepRunsOf(t *testing.T, pool *pgxpool.Pool, key string) []string {
	t.Helper()
	rows, err := pool.Query(context.Background(), `select task || ' ' || kind || ' ' || place || ' ' || starts || ' ' || run
		from step_starts where issue = $1 order by task, kind, place`, key)
	if err != nil {
		t.Fatalf("read the runs of %s: %v", key, err)
	}
	defer rows.Close()
	var runs []string
	for rows.Next() {
		var run string
		if err := rows.Scan(&run); err != nil {
			t.Fatalf("read a run of %s: %v", key, err)
		}
		runs = append(runs, run)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read the runs of %s: %v", key, err)
	}
	return runs
}

// A run whose place no step of the walk of its run holds ends at the next settle of its issue,
// stopped, and the daemon logs why. The way back of a rollback dropped the plan rows of task a
// while the implementer ran task a's step 1, so after the roll forward the issue walks today's
// line, where no step is task a's: no start and no end of that line names the place, and the
// settle, which reads every open run of the issue a fact touched, is where it ends. Its end is the
// record its start wrote, ended, under the id of that start, and it names the session and the
// model of the daemon's read of the implementer at that end, as every end of a run does. The runs
// of today's line go on as any run does: the boot opens implementing's, the move to testing ends
// it and starts testing's, which stays open. The control is the same issue with its plan rows
// kept: task a's step 1 ends passed with the move, naming the same read, and nothing is logged.
func TestARunOnNoStepOfItsWalkEndsAtTheNextSettle(t *testing.T) {
	t.Parallel()
	const offWalk = "workflow: a run on no step of the walk of its run is ended"
	for _, dropped := range []bool{true, false} {
		name := "the plan rows dropped by the way back"
		if !dropped {
			name = "control: the plan rows kept"
		}
		t.Run(name, func(t *testing.T) {
			pool := migratedPool(t)
			engine, logged := loggedEngine()
			root := stepRoot(t, pool, phase.Admitted)
			implementer := treeRoleToken(t, root.Key, claim.RoleImplementer)
			engine.cfg.StepAgent = func(_ context.Context, _ pgx.Tx, agent claim.Token) StepAgent {
				if string(agent) != implementer {
					return StepAgent{}
				}
				return StepAgent{Session: "s1", Model: "first/first-model"}
			}
			seedStepsPlan(t, pool, root.Key, root.Generation, "implementer", "tester")
			move(t, pool, engine, root.Key, TriggerGateOpened, "", record.PhaseRow{})
			if dropped {
				dropPlanRows(t, pool, root.Key)
				// The roll forward's boot over the store the way back left.
				rebooted(t, pool)
			}
			move(t, pool, engine, root.Key, TriggerImplementationReady, "", record.PhaseRow{})

			taskA := record.StepPlace{Task: "a", Kind: "step", Place: 1}
			var ends []string
			for _, w := range stepWrites(t, pool, root.Key) {
				if w.Task == taskA.Task && w.Kind == taskA.Kind && w.Step == taskA.Place && isEnd(w) {
					if w.ID != StepID(root.Key, 1, taskA, 1) || w.Role != "implementer" || w.Type != "implementation" || w.StartedAt == nil {
						t.Fatalf("the end of task a's step 1 = %+v, want the record its start wrote", w)
					}
					if !w.EndedAt.Equal(stepClock) || w.Session == nil || *w.Session != "s1" || w.Model == nil || *w.Model != "first/first-model" {
						t.Fatalf("the end of task a's step 1 ended at %v with session %v and model %v, want %s, s1 and first/first-model: the read of the implementer at that end",
							w.EndedAt, w.Session, w.Model, stepClock)
					}
					ends = append(ends, resultOf(w))
				}
			}
			wantRuns, wantEnds := []string{"a step 1 1 ended", "default step 2 0 ended", "default step 3 1 open"}, []string{"stopped"}
			if !dropped {
				wantRuns, wantEnds = []string{"a step 1 1 ended", "a step 2 1 open"}, []string{"passed"}
			}
			if runs := stepRunsOf(t, pool, root.Key); !reflect.DeepEqual(runs, wantRuns) {
				t.Fatalf("the runs of %s after the move = %v, want %v", root.Key, runs, wantRuns)
			}
			if !reflect.DeepEqual(ends, wantEnds) {
				t.Fatalf("the ends of task a's step 1 = %v, want %v", ends, wantEnds)
			}
			if said := strings.Contains(logged.String(), offWalk); said != dropped ||
				dropped && !strings.Contains(logged.String(), "the plan rows that named its step are gone") {
				t.Fatalf("the log =\n%s\nwant the line %q naming why only where the plan rows are gone", logged.String(), offWalk)
			}
		})
	}
}

// After a roll forward, an issue still held task-stopped or children whose plan rows the way back
// dropped — one a way back run without its update left — is a plain hold: no plan holds a run
// that has no plan rows (planHold). The retry releases a stopped root into the phase it is held
// from, a person's todo on it starts that phase's role, and the last child's leave of a parent held
// for its children applies and starts no retro. Each control is the same issue with its rows
// kept, which its plan holds.
func TestAHoldWhosePlanRowsTheWayBackDroppedIsAPlainHold(t *testing.T) {
	t.Parallel()
	for _, dropped := range []bool{false, true} {
		suffix := ": control, rows kept"
		if dropped {
			suffix = ": rows dropped by the way back"
		}
		t.Run("a retry of a stopped root"+suffix, func(t *testing.T) {
			pool := migratedPool(t)
			engine := returnEngine()
			root := stoppedAtTesting(t, pool, engine)
			if dropped {
				dropPlanRows(t, pool, root.Key)
			}
			result := applyFacts(t, pool, engine)("retry", intake.RetryOrEscalate{Issue: root.Key, Decision: intake.RetryDecision, Reason: "go on"})
			got := issueOf(t, pool, engine, root.Key)
			if !dropped {
				if result.Refusal == nil || result.Refusal.Code != "TASK_STOPPED" || got.Phase != phase.Held {
					t.Fatalf("the retry = %+v, %s in %s; want TASK_STOPPED and the hold kept", result.Refusal, root.Key, got.Phase)
				}
				return
			}
			if result.Refusal != nil || got.Phase != phase.Testing {
				t.Fatalf("the retry = %+v, %s in %s; want the plain hold released into testing", result.Refusal, root.Key, got.Phase)
			}
		})

		t.Run("a person's todo on a stopped root"+suffix, func(t *testing.T) {
			pool := migratedPool(t)
			engine := returnEngine()
			root := stoppedAtTesting(t, pool, engine)
			if dropped {
				dropPlanRows(t, pool, root.Key)
			}
			statuses := len(issueOutboxOf(t, pool, root.Key, string(record.OutboxKindDispatchStatus)))
			if result := applyFacts(t, pool, engine)("todo", personsTodo(root.Key)); result.Refusal != nil {
				t.Fatalf("the todo = %+v, want it taken", result.Refusal)
			}
			got := issueOf(t, pool, engine, root.Key)
			if !dropped {
				if got.Phase != phase.Implementing {
					t.Fatalf("%s stands in %s after the todo, want the stopped task started again at its first step", root.Key, got.Phase)
				}
				return
			}
			// The release of a plain hold starts the phase's role again and queues that phase's
			// status (releaseHeldRoot's retryPhase); the stop's own path, which takes a task back to
			// its first step, writes none.
			if now := len(issueOutboxOf(t, pool, root.Key, string(record.OutboxKindDispatchStatus))); got.Phase != phase.Testing || got.Hold != nil || now != statuses+1 {
				t.Fatalf("%s stands in %s (hold %+v) with %d status writes after the todo (before %d), want testing released with its status written", root.Key, got.Phase, got.Hold, now, statuses)
			}
		})

		t.Run("the last child's leave of a parent held for its children"+suffix, func(t *testing.T) {
			pool := migratedPool(t)
			engine := returnEngine()
			root := stepRoot(t, pool, phase.Admitted)
			seedStepsPlan(t, pool, root.Key, root.Generation, "implementer", "reviewer")
			move(t, pool, engine, root.Key, TriggerGateOpened, "", record.PhaseRow{})
			move(t, pool, engine, root.Key, TriggerImplementationReady, "", record.PhaseRow{})
			// The child enters once the parent stands in reviewing, as TestAParentWithNoAcceptanceIsHeldBeforeItsRetro's does.
			stepChild(t, pool, heldParentChild, phase.Implementing, nil)
			approveReview(t, pool, engine)
			if held := issueOf(t, pool, engine, root.Key); !record.HeldForChildren(held) {
				t.Fatalf("premise: %s stands in %s hold %+v, want held for children", root.Key, held.Phase, held.Hold)
			}
			if dropped {
				dropPlanRows(t, pool, root.Key)
			}
			// The done goes through the mirror of Dispatch first, as the daemon applies it
			// (childLeavesDone): the hold over the parent's open Dispatch children reads the child's
			// copy row.
			childLeavesDone(t, pool, engine)
			parent := issueOf(t, pool, engine, root.Key)
			if !dropped {
				if parent.Phase != phase.Retro {
					t.Fatalf("%s stands in %s after its last child left, want its retro started", root.Key, parent.Phase)
				}
				return
			}
			if parent.Phase != phase.Held {
				t.Fatalf("%s stands in %s after its last child left, want it still held: a plain hold starts no retro", root.Key, parent.Phase)
			}
		})
	}
}
