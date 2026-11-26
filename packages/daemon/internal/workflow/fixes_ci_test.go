package workflow

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/plan"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// A run whose first step starts in this release takes a fix on each of its tasks
// (record.PlanTask.FixesCI), whatever plan the first step takes: the default rows it writes, an
// owner's plan given at generation 0 and carried from its start, and an owner's plan given at the
// run's own generation, which it keeps. Each plan reads false before the first step, as a plan
// the release before stored does, so the flag is the first step's. Its red is the first step's
// mark left out of takeFirstPlan: the carried plan's task then reads false after the first step.
func TestARunThatStartsUnderThisReleaseTakesTheFlag(t *testing.T) {
	t.Run("the default rows", func(t *testing.T) {
		pool := migratedPool(t)
		engine := returnEngine()
		root := stepRoot(t, pool, phase.Admitted)

		firstStep(t, pool, engine, root.Key)

		if task := taskOf(t, pool, engine, root.Key); task.Task != plan.DefaultTask || !task.FixesCI {
			t.Fatalf("the task after the first step = %+v, want default with fixes_ci true", task)
		}
	})

	t.Run("an owner's plan given at generation 0 and carried from its start", func(t *testing.T) {
		pool := migratedPool(t)
		engine := returnEngine()
		recordedAt(t, pool, "LEGION-208", "LEGION-208")
		mustTake(t, "version 1 at generation 0", applyFacts(t, pool, engine)(planID("LEGION-209", 0, 1),
			ownersPlan(t, "LEGION-209", 0, 1, givenTask(t, "Build the widget.", "implementer", "reviewer"))))
		if waiting := planStandingOf(t, pool, engine, "LEGION-209", 0).task(t); waiting.FixesCI {
			t.Fatalf("the plan waiting at generation 0 = %+v, want fixes_ci false before any first step", waiting)
		}
		recordedAt(t, pool, "LEGION-209", "LEGION-208")

		firstStep(t, pool, engine, "LEGION-209")

		if task := taskOf(t, pool, engine, "LEGION-209"); task.Task != "a" || !task.FixesCI {
			t.Fatalf("the carried task after the first step = %+v, want a with fixes_ci true", task)
		}
	})

	t.Run("an owner's plan given at the run's own generation", func(t *testing.T) {
		pool := migratedPool(t)
		engine := returnEngine()
		root := stepRoot(t, pool, phase.Admitted)
		mustTake(t, "version 1", applyFacts(t, pool, engine)(planID(root.Key, root.Generation, 1),
			ownersPlan(t, root.Key, root.Generation, 1, givenTask(t, "Build the widget.", "implementer", "reviewer"))))
		if given := taskOf(t, pool, engine, root.Key); given.FixesCI {
			t.Fatalf("the plan given before the first step = %+v, want fixes_ci false", given)
		}

		firstStep(t, pool, engine, root.Key)

		if task := taskOf(t, pool, engine, root.Key); task.Task != "a" || !task.FixesCI {
			t.Fatalf("the kept task after the first step = %+v, want a with fixes_ci true", task)
		}
	})
}

// A task that a later version adds takes the flag of its run (record.Store.RunFixesCI), read
// before the version removes any task. This release runs one task for each issue, so a version
// adds a task where it replaces the stopped one. A run whose first step started before the
// release reads false on every task, as the upgrade wrote them, and the task its later version
// adds reads false too. The twin: a run of this release, its task marked at its first step, gives
// the task the version adds the flag.
func TestARunningRunOfTheReleaseBeforeKeepsNoFlag(t *testing.T) {
	for _, tc := range []struct {
		name string
		mark func(ctx context.Context, tx pgx.Tx, engine *Engine, issue record.Issue) error
		want bool
	}{
		{"a run of the release before", func(ctx context.Context, tx pgx.Tx, _ *Engine, issue record.Issue) error {
			_, err := tx.Exec(ctx, `update plan_tasks set fixes_ci = false where issue = $1 and generation = $2`, issue.Key, int64(issue.Generation))
			return err
		}, false},
		{"the twin: a run of this release", func(ctx context.Context, tx pgx.Tx, engine *Engine, issue record.Issue) error {
			return engine.store.MarkRunFixesCI(ctx, tx, issue.Key, issue.Generation)
		}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := migratedPool(t)
			engine := returnEngine()
			apply := applyFacts(t, pool, engine)
			root := stepRoot(t, pool, phase.Admitted)
			key, gen := root.Key, root.Generation
			firstStep(t, pool, engine, key)
			applyInTx(t, pool, engine, func(ctx context.Context, tx pgx.Tx) error { return tc.mark(ctx, tx, engine, root) })
			if running := taskOf(t, pool, engine, key); running.FixesCI != tc.want {
				t.Fatalf("the running task default = %+v, want fixes_ci %t", running, tc.want)
			}
			if result := apply("stop", ownersStop(t, root, plan.DefaultTask)); result.Refusal != nil {
				t.Fatalf("the owner's stop of default was refused: %+v", result.Refusal)
			}

			mustTake(t, "version 1, which replaces default by a", apply(planID(key, gen, 1),
				ownersPlan(t, key, gen, 1, givenTask(t, "Build the widget.", "implementer", "reviewer"))))

			if task := taskOf(t, pool, engine, key); task.Task != "a" || task.FixesCI != tc.want {
				t.Fatalf("the task version 1 added = %+v, want a with fixes_ci %t, its run's", task, tc.want)
			}
		})
	}
}

// The testing move leaves a check that a CI job runs to the fix: an issue in testing at its tested
// head, where a check that no CI job runs passes and one that a CI job runs failed, goes to
// review on the tester's completion. The review's approval and the owner's sign-off pass the task's
// last step, and its end holds the failed check for CI, from the retro; the check's pass at the
// head releases the hold, and the retro starts.
func TestTheJoinsTestingMoveLeavesCIToTheFix(t *testing.T) {
	r := newStatesRig(t, phase.Testing)
	// The tree runs, as its root's admission opens it, so the release of the end hold starts the
	// retro (startHeldRetro reads record.TreeRunning).
	seedTreeLifecycle(t, r.pool, "LEGION-208")
	r.states(headOne, 1, ofSet(passOf("k1", "tester"), "k1", "k2"), ofSet(inCI(stateOf("k2", "no", "fail", "ci"), "failure"), "k1", "k2"))
	r.complete(headOne, 2)
	assertPhase(t, r.pool, phase.Reviewing)

	move(t, r.pool, r.engine, "LEGION-208", TriggerReviewApproved, "", record.PhaseRow{})
	assertPhase(t, r.pool, phase.Accepting)
	if result := signsOff(t, r.pool, r.engine, "LEGION-208"); result.Refusal != nil {
		t.Fatalf("the owner's sign-off was refused: %+v", result.Refusal)
	}
	if issue := issueOf(t, r.pool, r.engine, "LEGION-208"); issue.Hold == nil || *issue.Hold != (record.Hold{From: phase.Retro, Reason: record.HoldForCI}) {
		t.Fatalf("LEGION-208 after the sign-off stands in %s with hold %+v, want held at its end for its CI", issue.Phase, issue.Hold)
	}

	r.states(headOne, 3, ofSet(inCI(stateOf("k2", stateYes, "", "ci"), "success"), "k1", "k2"))
	assertPhase(t, r.pool, phase.Retro)
}
