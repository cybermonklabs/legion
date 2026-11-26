package workflow

import (
	"context"
	"reflect"
	"slices"
	"testing"

	"github.com/jackc/pgx/v5"

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
// place, step 2, is not where the task stopped (Walk.runPlace of the linter's row against Walk.Place).
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
	if held := issueOf(t, pool, engine, root.Key); held.Phase != phase.Held || held.Hold == nil || *held.Hold != stopped || held.StepRole != "checker" {
		t.Fatalf("premise: %s after the stop stands in %s with hold %+v at the %q step, want held %+v at the checker's", root.Key, held.Phase, held.Hold, held.StepRole, stopped)
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
		if after := issueOf(t, pool, engine, root.Key); after.Phase != issue.Phase || !reflect.DeepEqual(after.Hold, issue.Hold) || after.StepRole != issue.StepRole {
			t.Fatalf("the linter's report moved %s to %s with hold %+v at the %q step, want it as it stood: %s with hold %+v at the %q step",
				root.Key, after.Phase, after.Hold, after.StepRole, issue.Phase, issue.Hold, issue.StepRole)
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
				w, err := engine.walk(ctx, tx, *issue)
				if err != nil {
					return err
				}
				rows, err := engine.store.Phases(ctx, tx, root.Key)
				if err != nil {
					return err
				}
				refusal, err = engine.lateReport(ctx, tx, *issue, w, intake.HandoffComplete{Issue: root.Key, Role: role, Claim: token, Generation: issue.Generation},
					phase.Step, stoppedStepReported, w.completionRow(rows, role, 0, false, record.StandsAt(*issue)))
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

// A role that works two steps reports each by the step whose run it was, not by its role: in
// `implementer:a reviewer:r implementer:a reviewer:r`, the approving review moves the task on from
// step 2 before the reviewer completes, and the owner stops the task at step 4, the reviewer's
// later step.
//
// (a) The reviewer's report of step 2, the run the approval ended with no report, is taken onto
// step 2's row and written onto step 2's record, moving nothing: the role works the step the task
// stands at, but the run is step 2's, which the task has left (leftItsPhase). Before, the role
// alone decided, so the report was read as one of the stopped step, which lateReport passes for a
// run at another place, and it was refused HANDOFF_NOT_CURRENT_PHASE: step 2's findings were lost.
//
// (b) The reviewer's report of step 4, the run the stop ended, is refused TASK_STOPPED as before.
func TestARolesReportOfItsEarlierStepIsTakenAfterAStopAtItsLaterStep(t *testing.T) {
	pool := migratedPool(t)
	engine := returnEngine()
	root := stepRoot(t, pool, phase.Admitted)
	seedStepsPlan(t, pool, root.Key, root.Generation, "implementer:a", "reviewer:r", "implementer:a", "reviewer:r")
	implementer := agentToken(t, "implementer")
	reviewer := seedPhaseWorker(t, pool, root.Key, claim.RoleReviewer, "")
	move(t, pool, engine, root.Key, TriggerGateOpened, record.TaskPlace{}, record.PhaseRow{})
	completesAs(t, pool, engine, claim.RoleImplementer, implementer, "implement", 1, "head")
	assertWalksAt(t, pool, engine, root.Key, phase.Reviewing, stepPlace(2))
	reviewApprovedNow(t, pool, engine, root.Key)
	completesAs(t, pool, engine, claim.RoleImplementer, implementer, "implement", 2, "head")
	assertWalksAt(t, pool, engine, root.Key, phase.Reviewing, stepPlace(4))
	rows := rolesRows(t, pool, engine, root.Key, claim.RoleReviewer)
	if rows[2].Completed() || rows[4].Completed() || rows[2].Entry >= rows[4].Entry {
		t.Fatalf("premise: the reviewer's rows are step 2 %+v and step 4 %+v, want both runs unreported, step 2's entry below step 4's", rows[2], rows[4])
	}
	second, fourth := rows[2].Entry, rows[4].Entry
	apply := applyFacts(t, pool, engine)
	if result := apply("stop", ownersStop(t, root, "a")); result.Refusal != nil {
		t.Fatalf("the owner's stop was refused: %+v", result.Refusal)
	}
	stopped := record.Hold{From: phase.Reviewing, Reason: record.HoldTaskStopped}
	if held := issueOf(t, pool, engine, root.Key); held.Phase != phase.Held || held.Hold == nil || *held.Hold != stopped {
		t.Fatalf("premise: %s after the stop stands in %s with hold %+v, want held %+v", root.Key, held.Phase, held.Hold, stopped)
	}
	if task := taskOf(t, pool, engine, root.Key); task.State != record.TaskStopped || task.Place != stepPlace(4) {
		t.Fatalf("premise: task a = %s at %+v, want stopped at step 4", task.State, task.Place)
	}

	t.Run("a: the report of step 2 is taken on step 2", func(t *testing.T) {
		issue, task := issueOf(t, pool, engine, root.Key), taskOf(t, pool, engine, root.Key)
		report := reportOf(claim.RoleReviewer, reviewer, "review-1")
		report.Entry = second
		if result := apply("reviewer-late-step-2", report); result.Refusal != nil {
			t.Fatalf("the reviewer's report of step 2 at its entry %d after the stop at step 4 = %d %s %q, want it taken on step 2",
				second, result.Refusal.Status, result.Refusal.Code, result.Refusal.Message)
		}
		rows := rolesRows(t, pool, engine, root.Key, claim.RoleReviewer)
		if rows[2].HandoffRecord != "review-1" || !rows[2].Completed() || rows[4].HandoffRecord != "" || rows[4].Completed() {
			t.Fatalf("the reviewer's rows after its report of step 2: step 2 %+v, step 4 %+v; want review-1 on step 2 and step 4 unreported", rows[2], rows[4])
		}
		if got := reportsAt(t, pool, root.Key, 2); !slices.Equal(got, []string{"a:review-1"}) {
			t.Fatalf("the reports of step 2 = %v, want the late report a:review-1", got)
		}
		if got := reportsAt(t, pool, root.Key, 4); len(got) != 0 {
			t.Fatalf("the reports of step 4 = %v, want none", got)
		}
		if after := issueOf(t, pool, engine, root.Key); after.Phase != issue.Phase || !reflect.DeepEqual(after.Hold, issue.Hold) {
			t.Fatalf("the reviewer's report of step 2 moved %s to %s with hold %+v, want it as it stood: %s with hold %+v",
				root.Key, after.Phase, after.Hold, issue.Phase, issue.Hold)
		}
		if after := taskOf(t, pool, engine, root.Key); !reflect.DeepEqual(after, task) {
			t.Fatalf("task a after the reviewer's report of step 2 = %+v, want %+v as it stood", after, task)
		}
	})

	t.Run("b: the report of step 4, the run the stop ended, is refused", func(t *testing.T) {
		before := reportedOf(t, pool, engine, root.Key, claim.RoleReviewer, true)
		report := reportOf(claim.RoleReviewer, reviewer, "review-2")
		report.Entry = fourth
		assertLateReport(t, pool, engine, claim.RoleReviewer, apply("reviewer-late-step-4", report), "TASK_STOPPED",
			"task a of LEGION-208 was stopped by its owner. Step 4 (reviewer) has ended. This report moved nothing. dispatch steps --issue LEGION-208 shows how that run ended."+usedTail,
			"review-2", before)
	})
}
