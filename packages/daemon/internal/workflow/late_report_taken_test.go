package workflow

// A role's first completion of its run after an event ended that run, on a run with plan rows. The
// reviewer's normal order is its handoff, its review, then its completion, so its own review event
// almost always ends its step first. The completion is taken as a run with no plan rows takes it
// (TestTheReviewEventDecidesAndTheCompletionRecordsItsFindings): it moves nothing, the role's row
// records it, and its report is written onto the ended step (reportEndedStep). The subtests ask
// that of the default rows a new issue with no owner's plan is given at its first step (firstStep,
// planFirstStep's defaultRows), and of an owner's plan `implementer reviewer acceptance:owner`.
// The controls still refuse: a stopped task, the report of a superseded entry, and a duplicate.

import (
	"context"
	"reflect"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// reviewApprovedNow ends key's review round, in reviewing, with an approval, as the review's
// settlement moves it (settleRound raises TriggerReviewApproved with the reviewer's row as it
// stands): the reviewer has not completed.
func reviewApprovedNow(t *testing.T, pool *pgxpool.Pool, engine *Engine, key string) {
	t.Helper()
	applyInTx(t, pool, engine, func(ctx context.Context, tx pgx.Tx) error {
		issue, err := engine.store.Issue(ctx, tx, key)
		if err != nil {
			return err
		}
		pr, err := engine.store.PullRequest(ctx, tx, key)
		if err != nil {
			return err
		}
		reviewer, err := engine.phaseRow(ctx, tx, key, claim.RoleReviewer)
		if err != nil {
			return err
		}
		return engine.transition(ctx, tx, *issue, TriggerReviewApproved, "", reviewer, pr, moveWhy{})
	})

}

// defaultAtReviewing is LEGION-208 whose owner gave no plan: its first step stores the project's
// default steps as a new issue's does (firstStep, StartFirstPhase), and it walks planning,
// implementing and testing to reviewing, step 4 of task default, with its reviewer's claim.
func defaultAtReviewing(t *testing.T, pool *pgxpool.Pool, engine *Engine) (record.Issue, claim.Token) {
	t.Helper()
	root := stepRoot(t, pool, phase.Admitted)
	firstStep(t, pool, engine, root.Key)
	if task := taskOf(t, pool, engine, root.Key); task.Task != defaultTask {
		t.Fatalf("the first step of %s with no owner's plan stored task %q, want the default rows", root.Key, task.Task)
	}
	move(t, pool, engine, root.Key, TriggerPlannerCompleted, "", record.PhaseRow{})
	move(t, pool, engine, root.Key, TriggerImplementationReady, "", record.PhaseRow{})
	move(t, pool, engine, root.Key, TriggerTesterPassed, "", record.PhaseRow{})
	reviewer := seedPhaseWorker(t, pool, root.Key, claim.RoleReviewer, "")
	assertWalksAt(t, pool, engine, root.Key, phase.Reviewing, record.TaskPlace{Kind: record.PlaceStep, Position: 4})
	return root, reviewer
}

// ownerAtReviewing is LEGION-208 walking the owner's plan `implementer reviewer acceptance:owner`,
// standing in reviewing at step 2 of task a, with its reviewer's claim.
func ownerAtReviewing(t *testing.T, pool *pgxpool.Pool, engine *Engine) (record.Issue, claim.Token) {
	t.Helper()
	root := stepRoot(t, pool, phase.Admitted)
	seedStepsPlan(t, pool, root.Key, root.Generation, "implementer", "reviewer", "acceptance:owner")
	move(t, pool, engine, root.Key, TriggerGateOpened, "", record.PhaseRow{})
	move(t, pool, engine, root.Key, TriggerImplementationReady, "", record.PhaseRow{})
	reviewer := seedPhaseWorker(t, pool, root.Key, claim.RoleReviewer, "")
	assertWalksAt(t, pool, engine, root.Key, phase.Reviewing, record.TaskPlace{Kind: record.PlaceStep, Position: 2})
	return root, reviewer
}

// startCounts is how many starts of each phase worker key's outbox holds.
func startCounts(t *testing.T, pool *pgxpool.Pool, key string) map[claim.Role]int {
	t.Helper()
	counts := map[claim.Role]int{}
	for _, role := range []claim.Role{claim.RolePlanner, claim.RoleImplementer, claim.RoleTester, claim.RoleReviewer, claim.RoleMerger} {
		counts[role] = len(roleStartsOf(t, pool, key, role))
	}
	return counts
}

// reportsAt is the reports the writes of step place of key name, in their order.
func reportsAt(t *testing.T, pool *pgxpool.Pool, key string, place int) []string {
	t.Helper()
	var reports []string
	for _, w := range writesAt(stepWrites(t, pool, key), record.PlaceStep, place) {
		if w.Report != nil {
			reports = append(reports, w.Task+":"+*w.Report)
		}
	}
	return reports
}

// assertFirstReportTaken sends the reviewer's first completion, naming review-1, after its review
// event ended its step, and holds it to the answer a run with no plan rows gets: no refusal, the
// issue and its task where the event left them, no start of any role, the reviewer's row holding
// review-1, and one write of the ended review step, place reviewed of task, naming review-1.
func assertFirstReportTaken(t *testing.T, pool *pgxpool.Pool, engine *Engine, key string, reviewer claim.Token,
	at phase.Phase, place record.TaskPlace, task string, reviewed int) {
	t.Helper()
	starts := startCounts(t, pool, key)
	result := applyFacts(t, pool, engine)("first-report", reportOf(claim.RoleReviewer, reviewer, "review-1"))
	if result.Refusal != nil {
		t.Fatalf("the reviewer's first completion after its review event ended its step = %d %s %q; want it taken, moving nothing, "+
			"with its report on the ended review step", result.Refusal.Status, result.Refusal.Code, result.Refusal.Message)
	}
	assertWalksAt(t, pool, engine, key, at, place)
	if after := startCounts(t, pool, key); !reflect.DeepEqual(after, starts) {
		t.Fatalf("the starts after the completion = %v, before %v; want no start: the completion moves nothing", after, starts)
	}
	if row := roleRowOf(t, pool, engine, key, claim.RoleReviewer); row.HandoffRecord != "review-1" || row.LastHandoff != "review-1" {
		t.Fatalf("the reviewer's row = %+v, want its completion's handoff review-1 recorded on it", row)
	}
	if got := reportsAt(t, pool, key, reviewed); len(got) != 1 || got[0] != task+":review-1" {
		t.Fatalf("the reports of the ended review step %d = %v, want one, %s:review-1", reviewed, got, task)
	}
}

func TestAFirstReportAfterItsRunsEventIsTakenOnTheEndedStep(t *testing.T) {
	t.Run("a: no owner's plan, the approval before the reviewer's first completion", func(t *testing.T) {
		pool := migratedPool(t)
		engine := returnEngine()
		root, reviewer := defaultAtReviewing(t, pool, engine)
		reviewApprovedNow(t, pool, engine, root.Key)
		accepting := record.TaskPlace{Kind: record.PlaceStep, Position: 5}
		assertWalksAt(t, pool, engine, root.Key, phase.Accepting, accepting)
		assertFirstReportTaken(t, pool, engine, root.Key, reviewer, phase.Accepting, accepting, defaultTask, 4)
	})

	t.Run("b: no owner's plan, the changes request before the reviewer's first completion", func(t *testing.T) {
		pool := migratedPool(t)
		engine := returnEngine()
		root, reviewer := defaultAtReviewing(t, pool, engine)
		reviewRequestsChanges(t, pool, engine, root.Key)
		implementing := record.TaskPlace{Kind: record.PlaceStep, Position: 2}
		assertWalksAt(t, pool, engine, root.Key, phase.Implementing, implementing)
		assertFirstReportTaken(t, pool, engine, root.Key, reviewer, phase.Implementing, implementing, defaultTask, 4)
	})

	t.Run("c: the plan implementer reviewer acceptance:owner, the approval before the reviewer's first completion", func(t *testing.T) {
		pool := migratedPool(t)
		engine := returnEngine()
		root, reviewer := ownerAtReviewing(t, pool, engine)
		reviewApprovedNow(t, pool, engine, root.Key)
		accepting := record.TaskPlace{Kind: record.PlaceStep, Position: 3}
		assertWalksAt(t, pool, engine, root.Key, phase.Accepting, accepting)
		assertFirstReportTaken(t, pool, engine, root.Key, reviewer, phase.Accepting, accepting, "a", 2)
	})

	// The twin, which shows the assertions above can pass: the same approval and first completion
	// on a run with no plan rows, whose review's start is recorded (StartStepRecord), is taken,
	// moves nothing and writes its report onto the ended review step.
	t.Run("twin: no plan rows, the approval before the reviewer's first completion is taken", func(t *testing.T) {
		pool := migratedPool(t)
		engine := returnEngine()
		root := stepRoot(t, pool, phase.Reviewing)
		reviewer := seedPhaseWorker(t, pool, root.Key, claim.RoleReviewer, "")
		onStored(t, pool, engine, root.Key, engine.StartStepRecord)
		reviewApprovedNow(t, pool, engine, root.Key)
		assertPhase(t, pool, phase.Accepting)
		if result := applyFacts(t, pool, engine)("first-report", reportOf(claim.RoleReviewer, reviewer, "review-1")); result.Refusal != nil {
			t.Fatalf("the reviewer's first completion on a run with no plan rows = %+v, want it taken", result.Refusal)
		}
		assertPhase(t, pool, phase.Accepting)
		if row := roleRowOf(t, pool, engine, root.Key, claim.RoleReviewer); row.HandoffRecord != "review-1" {
			t.Fatalf("the reviewer's row = %+v, want review-1 recorded on it", row)
		}
		if got := reportsAt(t, pool, root.Key, 4); len(got) != 1 || got[0] != defaultTask+":review-1" {
			t.Fatalf("the reports of the ended review step 4 = %v, want one, %s:review-1", got, defaultTask)
		}
	})

	t.Run("control: a stopped task still refuses TASK_STOPPED", func(t *testing.T) {
		pool := migratedPool(t)
		engine := returnEngine()
		root, reviewer := ownerAtReviewing(t, pool, engine)
		if result := applyFacts(t, pool, engine)("stop", ownersStop(t, root, "a")); result.Refusal != nil {
			t.Fatalf("the owner's stop was refused: %+v", result.Refusal)
		}
		before := reportedOf(t, pool, engine, root.Key, claim.RoleReviewer, true)
		result := applyFacts(t, pool, engine)("after-stop", reportOf(claim.RoleReviewer, reviewer, "review-1"))
		if result.Refusal == nil || result.Refusal.Code != "TASK_STOPPED" {
			t.Fatalf("the reviewer's completion of a stopped task = %+v, want TASK_STOPPED", result.Refusal)
		}
		if after := reportedOf(t, pool, engine, root.Key, claim.RoleReviewer, true); !reflect.DeepEqual(after.issue, before.issue) || !reflect.DeepEqual(after.task, before.task) {
			t.Fatalf("the refused completion moved %s: before %+v, after %+v", root.Key, before, after)
		}
	})

	t.Run("control: the report of a superseded entry does not complete the next entry", func(t *testing.T) {
		pool := migratedPool(t)
		engine := returnEngine()
		root, reviewer := defaultAtReviewing(t, pool, engine)
		reviewRequestsChanges(t, pool, engine, root.Key)
		// Round 1's first report, which case b holds to its answer.
		applyFacts(t, pool, engine)("round-1", reportOf(claim.RoleReviewer, reviewer, "review-1"))
		move(t, pool, engine, root.Key, TriggerImplementationReady, "", record.PhaseRow{})
		move(t, pool, engine, root.Key, TriggerTesterPassed, "", record.PhaseRow{})
		assertWalksAt(t, pool, engine, root.Key, phase.Reviewing, record.TaskPlace{Kind: record.PlaceStep, Position: 4})
		again := applyFacts(t, pool, engine)("round-1-again", reportOf(claim.RoleReviewer, reviewer, "review-1"))
		if again.Refusal == nil || again.Refusal.Code != "HANDOFF_NOT_NEW" {
			t.Fatalf("round 1's report sent in round 2 = %+v, want HANDOFF_NOT_NEW: a superseded entry's report does not complete the next entry", again.Refusal)
		}
		assertWalksAt(t, pool, engine, root.Key, phase.Reviewing, record.TaskPlace{Kind: record.PlaceStep, Position: 4})
	})

	t.Run("control: a duplicate of a taken completion is refused and attaches no second report", func(t *testing.T) {
		pool := migratedPool(t)
		engine := returnEngine()
		root, reviewer := defaultAtReviewing(t, pool, engine)
		if result := applyFacts(t, pool, engine)("first", reportOf(claim.RoleReviewer, reviewer, "review-1")); result.Refusal != nil {
			t.Fatalf("the reviewer's completion in reviewing = %+v, want it taken", result.Refusal)
		}
		reviewApprovedNow(t, pool, engine, root.Key)
		accepting := record.TaskPlace{Kind: record.PlaceStep, Position: 5}
		assertWalksAt(t, pool, engine, root.Key, phase.Accepting, accepting)
		reports := reportsAt(t, pool, root.Key, 4)
		dup := applyFacts(t, pool, engine)("duplicate", reportOf(claim.RoleReviewer, reviewer, "review-1"))
		if dup.Refusal == nil || dup.Refusal.Code != "HANDOFF_NOT_CURRENT_PHASE" {
			t.Fatalf("the duplicate of the reviewer's taken completion = %+v, want HANDOFF_NOT_CURRENT_PHASE", dup.Refusal)
		}
		assertWalksAt(t, pool, engine, root.Key, phase.Accepting, accepting)
		if got := reportsAt(t, pool, root.Key, 4); !reflect.DeepEqual(got, reports) {
			t.Fatalf("the reports of the review step after the duplicate = %v, before %v; want no second report", got, reports)
		}
	})
}
