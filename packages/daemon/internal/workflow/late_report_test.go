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

// reportOf is role's completion of its step of LEGION-208 at generation 1 under token, naming the
// handoff record handoff ("" names none).
func reportOf(role claim.Role, token claim.Token, handoff string) intake.HandoffComplete {
	return intake.HandoffComplete{Generation: 1, Issue: "LEGION-208", Role: role, Claim: token, Summary: "the late report", HandoffRecord: handoff, HandoffVersion: 1}
}

// reported is what a refused report must leave as it found, or with its one write: the issue, its
// task where it has plan rows, the reporting role's phases row and the number of outbox rows.
type reported struct {
	issue  record.Issue
	task   *record.PlanTask
	row    record.PhaseRow
	outbox int
}

func reportedOf(t *testing.T, pool *pgxpool.Pool, engine *Engine, key string, role claim.Role, planned bool) reported {
	t.Helper()
	now := reported{issue: issueOf(t, pool, engine, key), row: roleRowOf(t, pool, engine, key, role), outbox: outboxRows(t, pool)}
	if planned {
		task := taskOf(t, pool, engine, key)
		now.task = &task
	}
	return now
}

// assertLateReport fails unless result is the 409 code whose message is want, and the issue, its
// task and the outbox stand as before, and the role's row as before but for LastHandoff, which
// holds handoff where the report named one, so the next run of the step cannot complete on it.
func assertLateReport(t *testing.T, pool *pgxpool.Pool, engine *Engine, role claim.Role, result intake.Result, code, want, handoff string, before reported) {
	t.Helper()
	if result.Refusal == nil || result.Refusal.Status != 409 || result.Refusal.Code != code || result.Refusal.Message != want {
		t.Fatalf("the late report = %+v, want 409 %s %q", result.Refusal, code, want)
	}
	expected := before
	if handoff != "" {
		expected.row.LastHandoff = handoff
	}
	if after := reportedOf(t, pool, engine, before.issue.Key, role, before.task != nil); !reflect.DeepEqual(after, expected) {
		t.Fatalf("the refused late report changed %s:\n want %+v\n got  %+v", before.issue.Key, expected, after)
	}
}

// The tail every refusal of a late report ends with where the report names a handoff record.
const usedTail = " Its handoff record counts as used, so the next run of this step writes a handoff of its own."

// For an issue with plan rows, the first report of a run an event ended is taken: it moves
// nothing, and the next run of the step cannot complete on its handoff record (HANDOFF_NOT_NEW).
// A report of a step whose task was stopped there is refused, and the answer says why and what to
// do (TASK_STOPPED, by its owner or because a failed step had no step to return to). The refusal
// moves nothing and writes only the report's handoff record as the role's last one, so the next
// run of the step cannot complete on it either. Its text does not name how the run ended: the
// step record Dispatch holds does (dispatch steps). A report of a task whose issue left the
// workflow is refused HANDOFF_NOT_CURRENT_PHASE, and a report of another run
// HANDOFF_STALE_GENERATION. An issue with no plan rows answers the same: a late reviewer's report
// is taken and moves nothing, and a held issue's tester is told it runs no phase.
func TestALateReportIsTakenAfterAnEventAndRefusedAfterAStop(t *testing.T) {
	t.Run("a review that requested changes ended the reviewer's step", func(t *testing.T) {
		pool := migratedPool(t)
		engine := returnEngine()
		root := stepRoot(t, pool, phase.Admitted)
		seedStepsPlan(t, pool, root.Key, root.Generation, "implementer", "tester", "reviewer")
		move(t, pool, engine, root.Key, TriggerGateOpened, record.TaskPlace{}, record.PhaseRow{})
		move(t, pool, engine, root.Key, TriggerImplementationReady, record.TaskPlace{}, record.PhaseRow{})
		move(t, pool, engine, root.Key, TriggerTesterPassed, record.TaskPlace{}, record.PhaseRow{})
		reviewer := seedPhaseWorker(t, pool, root.Key, claim.RoleReviewer, "")
		reviewRequestsChanges(t, pool, engine, root.Key)
		implementing := record.TaskPlace{Kind: record.PlaceStep, Position: 1}
		assertWalksAt(t, pool, engine, root.Key, phase.Implementing, implementing)
		apply := applyFacts(t, pool, engine)

		if result := apply("late", reportOf(claim.RoleReviewer, reviewer, "review-1")); result.Refusal != nil {
			t.Fatalf("the reviewer's first report after the changes request = %+v, want it taken", result.Refusal)
		}
		assertWalksAt(t, pool, engine, root.Key, phase.Implementing, implementing)
		if row := roleRowOf(t, pool, engine, root.Key, claim.RoleReviewer); row.HandoffRecord != "review-1" || row.LastHandoff != "review-1" {
			t.Fatalf("the reviewer's row = %+v, want the taken report recorded on it", row)
		}

		// Round 2: the reviewer's completion naming the record of the taken report is refused as
		// a handoff not written anew.
		move(t, pool, engine, root.Key, TriggerImplementationReady, record.TaskPlace{}, record.PhaseRow{})
		move(t, pool, engine, root.Key, TriggerTesterPassed, record.TaskPlace{}, record.PhaseRow{})
		assertWalksAt(t, pool, engine, root.Key, phase.Reviewing, record.TaskPlace{Kind: record.PlaceStep, Position: 3})
		result := apply("round-2", reportOf(claim.RoleReviewer, reviewer, "review-1"))
		want := "the reviewer named handoff record review-1 for its previous phase of LEGION-208; write this phase's handoff with `dispatch handoff-write --issue LEGION-208 --phase review --body-file <file>` before completing"
		if result.Refusal == nil || result.Refusal.Code != "HANDOFF_NOT_NEW" || result.Refusal.Message != want {
			t.Fatalf("round 2's completion naming the taken report's record = %+v, want HANDOFF_NOT_NEW %q", result.Refusal, want)
		}
	})

	// The owner stops the task at the step a review's request for changes sent it back to, before
	// the reviewer's report of the run that request ended arrives. That run is one an event ended
	// before the stop, so its report is taken as the row above takes it: recorded on the reviewer's
	// row, moving nothing, the issue still held for its stopped task. The report of the step the
	// stop ended, the implementer's, is still refused as late.
	t.Run("a review that requested changes, then the owner's stop at the step it went back to", func(t *testing.T) {
		pool := migratedPool(t)
		engine := returnEngine()
		root := stepRoot(t, pool, phase.Admitted)
		seedStepsPlan(t, pool, root.Key, root.Generation, "implementer", "tester", "reviewer")
		move(t, pool, engine, root.Key, TriggerGateOpened, record.TaskPlace{}, record.PhaseRow{})
		move(t, pool, engine, root.Key, TriggerImplementationReady, record.TaskPlace{}, record.PhaseRow{})
		move(t, pool, engine, root.Key, TriggerTesterPassed, record.TaskPlace{}, record.PhaseRow{})
		reviewer := seedPhaseWorker(t, pool, root.Key, claim.RoleReviewer, "")
		reviewRequestsChanges(t, pool, engine, root.Key)
		assertWalksAt(t, pool, engine, root.Key, phase.Implementing, record.TaskPlace{Kind: record.PlaceStep, Position: 1})
		implementer := seedPhaseWorker(t, pool, root.Key, claim.RoleImplementer, "")
		apply := applyFacts(t, pool, engine)
		if result := apply("stop", ownersStop(t, root, "a")); result.Refusal != nil {
			t.Fatalf("the owner's stop was refused: %+v", result.Refusal)
		}
		stopped := record.Hold{From: phase.Implementing, Reason: record.HoldTaskStopped}
		if held := issueOf(t, pool, engine, root.Key); held.Phase != phase.Held || held.Hold == nil || *held.Hold != stopped {
			t.Fatalf("%s after the stop stands in %s with hold %+v, want held %+v", root.Key, held.Phase, held.Hold, stopped)
		}

		if result := apply("late", reportOf(claim.RoleReviewer, reviewer, "review-1")); result.Refusal != nil {
			t.Fatalf("the reviewer's report of the run the changes request ended, after the stop = %+v, want it taken", result.Refusal)
		}
		if held := issueOf(t, pool, engine, root.Key); held.Phase != phase.Held || held.Hold == nil || *held.Hold != stopped {
			t.Fatalf("the taken report moved %s to %s with hold %+v, want it held %+v still", root.Key, held.Phase, held.Hold, stopped)
		}
		if row := roleRowOf(t, pool, engine, root.Key, claim.RoleReviewer); row.HandoffRecord != "review-1" || row.LastHandoff != "review-1" {
			t.Fatalf("the reviewer's row = %+v, want the taken report recorded on it", row)
		}

		before := reportedOf(t, pool, engine, root.Key, claim.RoleImplementer, true)
		assertLateReport(t, pool, engine, claim.RoleImplementer, apply("late-implementer", reportOf(claim.RoleImplementer, implementer, "implement-2")), "TASK_STOPPED",
			"task a of LEGION-208 was stopped by its owner. Step 1 (implementer) has ended. This report moved nothing. dispatch steps --issue LEGION-208 shows how that run ended."+usedTail,
			"implement-2", before)
	})

	t.Run("the owner's stop ended the tester's step", func(t *testing.T) {
		pool := migratedPool(t)
		engine := returnEngine()
		tester := seedPhaseWorker(t, pool, "LEGION-208", claim.RoleTester, "")
		root := stoppedAtTesting(t, pool, engine)
		apply := applyFacts(t, pool, engine)
		const stopped = "task a of LEGION-208 was stopped by its owner. Step 2 (tester) has ended. This report moved nothing. dispatch steps --issue LEGION-208 shows how that run ended."

		// A late report that names no handoff record has none to count as used, and says so by
		// saying nothing of one.
		before := reportedOf(t, pool, engine, root.Key, claim.RoleTester, true)
		assertLateReport(t, pool, engine, claim.RoleTester, apply("late-bare", reportOf(claim.RoleTester, tester, "")), "TASK_STOPPED", stopped, "", before)

		before = reportedOf(t, pool, engine, root.Key, claim.RoleTester, true)
		assertLateReport(t, pool, engine, claim.RoleTester, apply("late", reportOf(claim.RoleTester, tester, "test-1")), "TASK_STOPPED", stopped+usedTail, "test-1", before)
	})

	// The way back of a rollback past the release that writes plan rows drops every plan row and
	// leaves the issue's hold as it stood (the header of 20261117034837_issue_plans.up.sql), so an
	// issue still held for its stopped task at the roll forward has that hold and no plan rows. A
	// late report of the stopped step then has no task to read a stop reason from, and is answered
	// as a report of a phase no role works there, writing nothing.
	t.Run("a stopped task whose plan rows a rollback dropped is not current", func(t *testing.T) {
		pool := migratedPool(t)
		engine := returnEngine()
		tester := seedPhaseWorker(t, pool, "LEGION-208", claim.RoleTester, "")
		root := stoppedAtTesting(t, pool, engine)
		for _, table := range []string{"plan_steps", "plan_tasks", "issue_plans"} {
			if _, err := pool.Exec(t.Context(), "delete from "+table+" where issue = $1", root.Key); err != nil {
				t.Fatalf("delete %s's rows of %s: %v", root.Key, table, err)
			}
		}
		if held := issueOf(t, pool, engine, root.Key); held.Hold == nil || *held.Hold != (record.Hold{From: phase.Testing, Reason: record.HoldTaskStopped}) {
			t.Fatalf("%s stands in %s with hold %+v, want held from testing for the stopped task", root.Key, held.Phase, held.Hold)
		}
		before := reportedOf(t, pool, engine, root.Key, claim.RoleTester, false)
		assertLateReport(t, pool, engine, claim.RoleTester, applyFacts(t, pool, engine)("late", reportOf(claim.RoleTester, tester, "test-1")),
			"HANDOFF_NOT_CURRENT_PHASE", "the tester does not run phase held of LEGION-208; this completion changed nothing", "", before)
	})

	t.Run("a failed step with no step to return to stopped the task", func(t *testing.T) {
		pool := migratedPool(t)
		engine := returnEngine()
		root := stepRoot(t, pool, phase.Admitted)
		seedStepsPlan(t, pool, root.Key, root.Generation, "implementer", "tester", "reviewer")
		move(t, pool, engine, root.Key, TriggerGateOpened, record.TaskPlace{}, record.PhaseRow{})
		move(t, pool, engine, root.Key, TriggerImplementationReady, record.TaskPlace{}, record.PhaseRow{})
		move(t, pool, engine, root.Key, TriggerTesterPassed, record.TaskPlace{}, record.PhaseRow{})
		// The type is forged: no plan of this release writes it. Each failed step a plan can hold
		// comes after its implementer (plan.Supported), so only a stored type that no plan gives
		// stops the task here (returnTarget).
		if _, err := pool.Exec(t.Context(), "update plan_steps set type = 'review' where issue = $1 and task = 'a' and position = 1", root.Key); err != nil {
			t.Fatalf("store the implementer step's type as review: %v", err)
		}
		reviewer := seedPhaseWorker(t, pool, root.Key, claim.RoleReviewer, "")
		reviewRequestsChanges(t, pool, engine, root.Key)
		if held := issueOf(t, pool, engine, root.Key); held.Hold == nil || *held.Hold != (record.Hold{From: phase.Reviewing, Reason: record.HoldTaskStopped}) {
			t.Fatalf("%s stands in %s with hold %+v, want held from reviewing for the stopped task", root.Key, held.Phase, held.Hold)
		}
		before := reportedOf(t, pool, engine, root.Key, claim.RoleReviewer, true)

		result := applyFacts(t, pool, engine)("late", reportOf(claim.RoleReviewer, reviewer, "review-1"))

		assertLateReport(t, pool, engine, claim.RoleReviewer, result, "TASK_STOPPED",
			"task a of LEGION-208 was stopped: step 3 (reviewer) failed and no earlier step of the type implementation can take the work. Step 3 (reviewer) has ended. This report moved nothing. dispatch steps --issue LEGION-208 shows how that run ended."+usedTail,
			"review-1", before)
	})

	// An issue with plan rows refuses a report of a task whose issue left the workflow as a report
	// of a phase no role works (HANDOFF_NOT_CURRENT_PHASE), and a report of another run as stale
	// (HANDOFF_STALE_GENERATION).
	t.Run("a task whose issue left the workflow is not current, and another run is stale", func(t *testing.T) {
		pool := migratedPool(t)
		engine := returnEngine()
		root := stepRoot(t, pool, phase.Admitted)
		seedStepsPlan(t, pool, root.Key, root.Generation, "implementer", "tester")
		move(t, pool, engine, root.Key, TriggerGateOpened, record.TaskPlace{}, record.PhaseRow{})
		move(t, pool, engine, root.Key, TriggerImplementationReady, record.TaskPlace{}, record.PhaseRow{})
		tester := seedPhaseWorker(t, pool, root.Key, claim.RoleTester, "")
		apply := applyFacts(t, pool, engine)

		stale := reportOf(claim.RoleTester, tester, "test-1")
		stale.Generation = 2
		before := reportedOf(t, pool, engine, root.Key, claim.RoleTester, true)
		assertLateReport(t, pool, engine, claim.RoleTester, apply("another-run", stale), "HANDOFF_STALE_GENERATION",
			"LEGION-208 is on generation 1 and this completion is generation 2's; the run it reports is over", "", before)

		applyInTx(t, pool, engine, func(ctx context.Context, tx pgx.Tx) error {
			issue, err := engine.store.Issue(ctx, tx, root.Key)
			if err != nil {
				return err
			}
			return engine.leave(ctx, tx, *issue, "backlog")
		})

		if task := taskOf(t, pool, engine, root.Key); task.State != record.TaskLeft {
			t.Fatalf("task a after its issue left is %s, want left", task.State)
		}
		before = reportedOf(t, pool, engine, root.Key, claim.RoleTester, true)
		assertLateReport(t, pool, engine, claim.RoleTester, apply("left", reportOf(claim.RoleTester, tester, "test-1")), "HANDOFF_NOT_CURRENT_PHASE",
			"the tester does not run phase done of LEGION-208; this completion changed nothing", "", before)
	})

	t.Run("the twins with no plan rows answer the same", func(t *testing.T) {
		pool := migratedPool(t)
		engine := returnEngine()
		root := stepRoot(t, pool, phase.Reviewing)
		reviewer := seedPhaseWorker(t, pool, root.Key, claim.RoleReviewer, "")
		reviewRequestsChanges(t, pool, engine, root.Key)
		assertPhase(t, pool, phase.Implementing)

		if result := applyFacts(t, pool, engine)("late", reportOf(claim.RoleReviewer, reviewer, "review-1")); result.Refusal != nil {
			t.Fatalf("the late reviewer's report on an issue with no plan rows = %+v, want it taken", result.Refusal)
		}
		assertPhase(t, pool, phase.Implementing)
		if row := roleRowOf(t, pool, engine, root.Key, claim.RoleReviewer); row.HandoffRecord != "review-1" || row.LastHandoff != "review-1" {
			t.Fatalf("the reviewer's row = %+v, want the late report recorded on it", row)
		}

		held := migratedPool(t)
		bare := stepRoot(t, held, phase.Held)
		bare.Hold = &record.Hold{From: phase.Testing}
		seedIssue(t, held, bare)
		tester := seedPhaseWorker(t, held, bare.Key, claim.RoleTester, "")
		before := reportedOf(t, held, engine, bare.Key, claim.RoleTester, false)
		assertLateReport(t, held, engine, claim.RoleTester, applyFacts(t, held, engine)("held", reportOf(claim.RoleTester, tester, "test-1")),
			"HANDOFF_NOT_CURRENT_PHASE", "the tester does not run phase held of LEGION-208; this completion changed nothing", "", before)
	})
}

// A task stopped in its retro or in its merge stands at no step of its plan: the retro and the
// merge are the acts of its line after its steps (record.PlaceRetro, record.PlaceMerge). The late
// report of the role that worked it, the implementer's of the retro and the merger's of the merge,
// is refused HANDOFF_NOT_CURRENT_PHASE, as a report of a phase no role works while the issue is
// held, and never TASK_STOPPED, which answers only the report of a step the stop ended
// (lateReport). The two get there by two roads. The merger's report is of merging, the phase the
// hold is from, so it reports the run the stop ended (pastReportOf), and lateReport answers it
// nothing because the merge is no step. The implementer works implementing and the retro, and
// while the issue is held its report is of implementing (phase.Of, phase.LeftBy: held stands
// outside the line), which the hold is not from, so it reports no run at all. The implementer's
// completion of implementing is the fact the completion route applies, so its row keeps that
// completion's handoff record as its last one, as a real run leaves it. Each late report has the
// fields the role's `legion` tool sends while the issue is held (packages/pi-shared/src/
// handoff-record.ts, completedPhase and findHandoffReport) and the completion route requires
// (api's handoffComplete: a commit from the implementer and from the merger). The implementer's is
// of implementing, so it names its newest implementing handoff record, at that record's version
// and with the head that record names as its commit. The merger's names no record and no version,
// since merging ends with no record and the route refuses a merge report that names one
// (HANDOFF_RECORD_MISMATCH), and its commit is the one its workspace stands on. Either refusal
// writes nothing: the issue, its task, the role's row and the outbox stand as they were.
func TestALateReportOfTheRetroOrTheMergeAfterAStopIsNotCurrent(t *testing.T) {
	const head = "head"
	for _, tc := range []struct {
		name string
		role claim.Role
		// after takes the task on from the implementer's completion of its step to where the owner
		// stops it, at in place. handoff and version are the record the role's report names, ""
		// and 0 for none.
		after   []TriggerKind
		at      phase.Phase
		place   record.TaskPlace
		handoff string
		version int
	}{
		{name: "the implementer's report of the retro", role: claim.RoleImplementer,
			at: phase.Retro, place: record.TaskPlace{Kind: record.PlaceRetro}, handoff: "implement-1", version: 1},
		{name: "the merger's report of the merge", role: claim.RoleMerger, after: []TriggerKind{TriggerRetroCompleted},
			at: phase.Merging, place: record.TaskPlace{Kind: record.PlaceMerge}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := migratedPool(t)
			engine := returnEngine()
			apply := applyFacts(t, pool, engine)
			root := stepRoot(t, pool, phase.Admitted)
			seedStepsPlan(t, pool, root.Key, root.Generation, "implementer")
			move(t, pool, engine, root.Key, TriggerGateOpened, record.TaskPlace{}, record.PhaseRow{})
			implementer := seedPhaseWorker(t, pool, root.Key, claim.RoleImplementer, "")
			if result := apply("implemented", intake.HandoffComplete{Generation: 1, Issue: root.Key, Role: claim.RoleImplementer, Claim: implementer,
				Summary: "implemented", Commit: head, HandoffRecord: "implement-1", HandoffVersion: 1}); result.Refusal != nil {
				t.Fatalf("the implementer's completion of implementing was refused: %+v", result.Refusal)
			}
			for _, trigger := range tc.after {
				move(t, pool, engine, root.Key, trigger, "", record.PhaseRow{})
			}
			assertWalksAt(t, pool, engine, root.Key, tc.at, tc.place)
			worker := seedPhaseWorker(t, pool, root.Key, tc.role, "")
			if result := apply("stop", ownersStop(t, root, "a")); result.Refusal != nil {
				t.Fatalf("the owner's stop was refused: %+v", result.Refusal)
			}
			stopped := record.Hold{From: tc.at, Reason: record.HoldTaskStopped}
			if held := issueOf(t, pool, engine, root.Key); held.Phase != phase.Held || held.Hold == nil || *held.Hold != stopped {
				t.Fatalf("%s after the stop stands in %s with hold %+v, want held %+v", root.Key, held.Phase, held.Hold, stopped)
			}
			if task := taskOf(t, pool, engine, root.Key); task.State != record.TaskStopped || task.Place != tc.place {
				t.Fatalf("task a after the stop is %s at %+v, want stopped at %+v", task.State, task.Place, tc.place)
			}
			if row := roleRowOf(t, pool, engine, root.Key, claim.RoleImplementer); row.LastHandoff != "implement-1" {
				t.Fatalf("the implementer's row after its completion = %+v, want implement-1 kept as its last handoff", row)
			}
			before := reportedOf(t, pool, engine, root.Key, tc.role, true)

			result := apply("late", intake.HandoffComplete{Generation: 1, Issue: root.Key, Role: tc.role, Claim: worker, Summary: "the late report",
				Commit: head, HandoffRecord: tc.handoff, HandoffVersion: tc.version})

			if result.Refusal == nil || result.Refusal.Status != 409 || result.Refusal.Code != "HANDOFF_NOT_CURRENT_PHASE" {
				t.Fatalf("the %s's late report after the stop at %s = %+v, want 409 HANDOFF_NOT_CURRENT_PHASE", tc.role, tc.at, result.Refusal)
			}
			if after := reportedOf(t, pool, engine, root.Key, tc.role, true); !reflect.DeepEqual(after, before) {
				t.Fatalf("the refused late report changed %s:\n want %+v\n got  %+v", root.Key, before, after)
			}
		})
	}
}
