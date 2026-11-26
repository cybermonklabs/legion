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

// A stopped task's late report is the report of the step the stop ended alone, read by place and
// never by phase: in `implementer linter checker` the linter and the checker both run as step, and
// the owner stops task a at step 3, the checker's.
//
// (a) The checker's report of the run the stop ended is refused TASK_STOPPED
// (intake.LateReportOfStoppedTask), names step 3, moves nothing, and keeps its handoff record as
// the checker's last one.
//
// (b) The linter's report of its step 2, a run an event ended before the stop with no report, is
// no report of the stopped step, though its step runs as step too: it is taken onto the linter's
// row (leftItsPhase) and moves nothing, the issue held for its stopped task as before.
//
// (c) lateReport itself, asked of each role as a report of the step the stop ended
// (stoppedStepReported), refuses the checker's and answers nil for the linter's: the linter's own
// place, step 2, is not where the task stopped (Walk.placeOfRole against Walk.Place).
func TestALateReportOfAStoppedTaskIsTheStoppedStepsAlone(t *testing.T) {
	pool := migratedPool(t)
	engine := deploymentEngine()
	root := stepRoot(t, pool, phase.Step)
	seedStepsAt(t, pool, root.Key, root.Generation, 3, map[string]string{"linter": "testing", "checker": "testing"}, "implementer", "linter", "checker")
	checker := seedPhaseWorker(t, pool, root.Key, "checker", "")
	linter := seedPhaseWorker(t, pool, root.Key, "linter", "")
	// Each of the two steps started in this run: its role's row holds the task of an entry
	// (record.PhaseRow.EntryTask), which is what makes a report of that run the report of a
	// started step (ranUnreported), as enter writes it.
	seedPhase(t, pool, record.PhaseRow{Issue: root.Key, Role: "checker", Claim: checker, Entry: 1, EntryTask: "check this change"})
	seedPhase(t, pool, record.PhaseRow{Issue: root.Key, Role: "linter", Claim: linter, Entry: 1, EntryTask: "lint this change"})
	apply := applyFacts(t, pool, engine)
	if result := apply("stop", ownersStop(t, root, "a")); result.Refusal != nil {
		t.Fatalf("the owner's stop was refused: %+v", result.Refusal)
	}
	stopped := record.Hold{From: phase.Step, Reason: record.HoldTaskStopped}
	if held := issueOf(t, pool, engine, root.Key); held.Phase != phase.Held || held.Hold == nil || *held.Hold != stopped ||
		len(held.Running) != 0 || stoppedStepOf(t, pool, held).Role != "checker" {
		t.Fatalf("premise: %s after the stop stands in %s with hold %+v, the running steps %+v and the stopped step %+v, want held %+v at the checker's, no step running",
			root.Key, held.Phase, held.Hold, held.Running, stoppedStepOf(t, pool, held), stopped)
	}
	if task := taskOf(t, pool, engine, root.Key); task.State != record.TaskStopped || task.Place != stepPlace(3) {
		t.Fatalf("premise: task a = %s at %+v, want stopped at step 3", task.State, task.Place)
	}

	t.Run("a: the checker's report of the run the stop ended is refused", func(t *testing.T) {
		before := reportedOf(t, pool, engine, root.Key, "checker", true)
		assertLateReport(t, pool, engine, "checker", apply("checker-late", reportOf("checker", checker, "check-1")), "TASK_STOPPED",
			"task a of LEGION-208 was stopped by its owner. Step 3 (checker) has ended. This report moved nothing. dispatch steps --issue LEGION-208 shows how that run ended."+usedTail,
			"check-1", before)
	})

	t.Run("b: the linter's report of its step 2 is no report of the stopped step", func(t *testing.T) {
		issue, task := issueOf(t, pool, engine, root.Key), taskOf(t, pool, engine, root.Key)
		if result := apply("linter-late", reportOf("linter", linter, "lint-1")); result.Refusal != nil {
			t.Fatalf("the linter's report of its step 2 = %+v, want it taken: the stop ended step 3, the checker's", result.Refusal)
		}
		if row := roleRowOf(t, pool, engine, root.Key, "linter"); row.HandoffRecord != "lint-1" || !row.Completed() {
			t.Fatalf("the linter's row = %+v, want its report recorded on it", row)
		}
		if after := issueOf(t, pool, engine, root.Key); after.Phase != issue.Phase || !reflect.DeepEqual(after.Hold, issue.Hold) ||
			!reflect.DeepEqual(after.Running, issue.Running) || stoppedStepOf(t, pool, after) != stoppedStepOf(t, pool, issue) {
			t.Fatalf("the linter's report moved %s to %s with hold %+v at the step %+v, want it as it stood: %s with hold %+v at the step %+v",
				root.Key, after.Phase, after.Hold, stoppedStepOf(t, pool, after), issue.Phase, issue.Hold, stoppedStepOf(t, pool, issue))
		}
		if after := taskOf(t, pool, engine, root.Key); !reflect.DeepEqual(after, task) {
			t.Fatalf("task a after the linter's report = %+v, want %+v as it stood", after, task)
		}
	})

	t.Run("c: lateReport refuses the stopped step's report alone", func(t *testing.T) {
		lateOf := func(role claim.Role, token claim.Token) *intake.Refusal {
			t.Helper()
			var refusal *intake.Refusal
			applyInTx(t, pool, engine, func(ctx context.Context, tx pgx.Tx) error {
				issue, err := engine.store.Issue(ctx, tx, root.Key)
				if err != nil {
					return err
				}
				s, err := firstScope(ctx, tx, engine, *issue)
				if err != nil {
					return err
				}
				refusal, err = engine.lateReport(ctx, tx, s, intake.HandoffComplete{Issue: root.Key, Role: role, Claim: token, Generation: issue.Generation},
					phase.Step, stoppedStepReported)
				return err
			})
			return refusal
		}
		// Both runs are unreported here, so only the place tells the two apart: the checker's refused
		// report kept its handoff record as the last one alone (record.PhaseRow.LastHandoff), and the
		// linter's taken report is cleared off its row, as before it reported.
		if _, err := pool.Exec(t.Context(), `update phases set handoff_record = '', handoff_commit = '' where issue = $1 and role = 'linter'`, root.Key); err != nil {
			t.Fatalf("clear the linter's report: %v", err)
		}
		if row := roleRowOf(t, pool, engine, root.Key, "checker"); row.Completed() {
			t.Fatalf("premise: the checker's row = %+v, want its run unreported", row)
		}
		if late := lateOf("checker", checker); late == nil || late.Code != "TASK_STOPPED" {
			t.Fatalf("lateReport of the checker's report = %+v, want TASK_STOPPED", late)
		}
		if late := lateOf("linter", linter); late != nil {
			t.Fatalf("lateReport of the linter's report = %+v, want nil: the linter's step 2 is not step 3, where the task stopped", late)
		}
	})
}

// stoppedStepOf is the step the stop of issue's task ended, as the store reads it
// (record.StoppedStep), the zero step where there is none.
func stoppedStepOf(t *testing.T, pool *pgxpool.Pool, issue record.Issue) record.RunningStep {
	t.Helper()
	var step record.RunningStep
	if err := pgx.BeginFunc(t.Context(), pool, func(tx pgx.Tx) error {
		var err error
		step, _, err = record.NewStore().StoppedStep(t.Context(), tx, issue)
		return err
	}); err != nil {
		t.Fatalf("read the stopped step of %s: %v", issue.Key, err)
	}
	return step
}
