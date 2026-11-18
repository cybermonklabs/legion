package workflow

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/store/migrations"
)

// A rollback hands the store to an earlier release, which moves the issue and starts and ends its
// steps without counting a start or marking an end in step_starts, so the roll forward finds each
// place's row as this release left it. A run the earlier release started is none the row counted:
// its end after the roll forward is a record of its own, at the next count, with the start its
// role's entry recorded, whether the run this release counted there had ended before the rollback
// or was still open at it. The run this release counted keeps its own id, and no write after the
// roll forward names it. A run still open at the rollback that the earlier release ended gets its
// end from the roll forward itself, stopped at the moment of the rollback, when Legion stopped
// recording it, so its record reads ended whether or not its place runs again.
func TestAStepTheEarlierReleaseStartedEndsInARecordOfItsOwn(t *testing.T) {
	for _, tc := range []struct {
		name string
		// open is whether the tester's first run, which this release counted, was still running
		// at the rollback.
		open bool
	}{
		{name: "its place's run had ended at the rollback"},
		{name: "its place's run was still open at the rollback", open: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := migratedPool(t)
			stepRoot(t, pool, phase.Implementing)
			move(t, pool, stepEngine(nil), "LEGION-208", TriggerImplementationReady, "", record.PhaseRow{})
			if !tc.open {
				move(t, pool, stepEngine(nil), "LEGION-208", TriggerTesterFailed, "", record.PhaseRow{})
			}
			before := len(stepWrites(t, pool, "LEGION-208"))
			// The rollback's moment is the database's clock when the way back runs.
			rollBackFrom := databaseNow(t, pool)
			rollBackStepRecords(t, pool)
			rollBackTo := databaseNow(t, pool)
			// The earlier release runs the tester again, from its own transitions: the tester's
			// second entry, raised at secondRun, and for the run still open at the rollback, its
			// fail and the implementer's round that brought the work back to testing first, the
			// fail counting the pass every release's move back counts (recordPass).
			secondRun := stepClock.Add(time.Hour)
			if tc.open {
				engine := stepEngine(nil)
				applyInTx(t, pool, engine, func(ctx context.Context, tx pgx.Tx) error {
					return engine.recordPass(ctx, tx, "LEGION-208", phase.Testing, phase.Implementing, nil)
				})
			}
			execSQL(t, pool, `update issues set phase = 'testing' where key = 'LEGION-208'`)
			execSQL(t, pool, `update phases set entry = entry + 1, entry_raised_at = $1 where issue = 'LEGION-208' and role = 'tester'`, secondRun)
			rollForwardStepRecords(t, pool)
			// The run open at the rollback is the one the earlier release ended first: the
			// implementer's, which the tester's fail had started, where the tester's run had
			// ended, and the tester's own where it was still open. The roll forward ends it.
			counted := StepID("LEGION-208", 1, placeOf("step", 3), 1)
			abandoned, abandonedStep, abandonedRole := counted, 3, claim.RoleTester
			if !tc.open {
				abandoned, abandonedStep, abandonedRole = StepID("LEGION-208", 1, placeOf("step", 2), 1), 2, claim.RoleImplementer
			}
			ended := stepWrites(t, pool, "LEGION-208")[before:]
			if len(ended) != 1 {
				t.Fatalf("the roll forward wrote %+v, want the end of the run open at the rollback, which the earlier release ended", ended)
			}
			assertEnd(t, ended[0], "step", abandonedStep, 1, "stopped", nil)
			if at := ended[0].EndedAt; ended[0].ID != abandoned || at == nil || at.Before(rollBackFrom) || at.After(rollBackTo) ||
				ended[0].Agent != treeRoleToken(t, "LEGION-208", abandonedRole) || ended[0].StartedAt == nil || !ended[0].StartedAt.Equal(stepClock) {
				t.Fatalf("the roll forward's end = %+v, want the record %s, its %s and its start at %s, stopped at the rollback, between %s and %s",
					ended[0], abandoned, abandonedRole, stepClock, rollBackFrom, rollBackTo)
			}

			before = len(stepWrites(t, pool, "LEGION-208"))
			move(t, pool, stepEngine(nil), "LEGION-208", TriggerTesterPassed, "", record.PhaseRow{})
			after := stepWrites(t, pool, "LEGION-208")[before:]
			// The run this release counted has its end; nothing after the roll forward names it.
			for _, w := range after {
				if w.ID == counted {
					t.Fatalf("a write after the roll forward names the run this release counted (%s): %+v", counted, w)
				}
			}
			if len(after) != 2 {
				t.Fatalf("writes after the roll forward = %+v, want the end of the earlier release's tester run and the start of reviewing", after)
			}
			end := after[0]
			assertEnd(t, end, "step", 3, 1, "passed", nil)
			if want := StepID("LEGION-208", 1, placeOf("step", 3), 2); end.ID != want || end.StartedAt == nil || !end.StartedAt.Equal(secondRun) ||
				end.Agent != treeRoleToken(t, "LEGION-208", claim.RoleTester) {
				t.Fatalf("the end = %+v, want the record %s of the second count, the tester's, started at %s", end, want, secondRun)
			}
		})
	}
}

// A member's merge runs on through the linger of its tree and the re-admission of its root, and
// only the promotion that starts its merger again ends it (admit's startMidPhaseMembers). So an
// earlier release that re-admits the root after a rollback, and has not promoted the tree by the
// roll forward, ended no run of the member: the roll forward writes nothing of its merge, and the
// merge that lands then ends the record this release counted, passed, under its own id.
func TestAMembersMergeRunsOnThroughARootReadmittedAcrossARollback(t *testing.T) {
	pool := migratedPool(t)
	root := stepRoot(t, pool, phase.Reviewing)
	stepChild(t, pool, "LEGION-209", phase.Retro, nil)
	seedPR(t, pool, record.PullRequest{State: record.PullRequestOpen, Issue: "LEGION-209", Repo: "sjawhar/legion", Number: 43,
		Branch: "legion/LEGION-209", HeadSHA: "head-43", Failing: []string{}})
	engine := stepEngine(nil)
	move(t, pool, engine, "LEGION-209", TriggerRetroCompleted, "", record.PhaseRow{})
	apply := applyFacts(t, pool, engine)
	apply("park-root", intake.DispatchIssue{Key: root.Key, ResolvedRepo: "sjawhar/legion", Seq: 2, Type: "issue.updated",
		Status: "backlog", Title: "root", Rank: "U"})
	writes := stepWrites(t, pool, "LEGION-209")
	if len(writes) != 2 || !isStart(writes[1]) || writes[1].Kind != "merge" {
		t.Fatalf("the member's writes before the rollback = %+v, want the retro's end and the merge's start, open", writes)
	}
	merge := writes[1].ID
	rollBackStepRecords(t, pool)
	// The earlier release re-admits the root (admit's readmit): a new generation, its linger
	// cleared, and no promotion yet.
	execSQL(t, pool, `update issues set generation = generation + 1, linger_until = null, phase = 'admitted', status = 'todo' where key = $1`, root.Key)
	rollForwardStepRecords(t, pool)
	if after := stepWrites(t, pool, "LEGION-209")[len(writes):]; len(after) != 0 {
		t.Fatalf("the roll forward wrote %+v for LEGION-209, whose merge still runs", after)
	}
	apply("merge", intake.PullRequestMerged{Repo: "sjawhar/legion", Number: 43, HeadSHA: "head-43", MergeSHA: "merge-43"})
	after := stepWrites(t, pool, "LEGION-209")[len(writes):]
	if len(after) != 1 || after[0].ID != merge {
		t.Fatalf("the merge's writes after the roll forward = %+v, want the one end of the record %s this release counted", after, merge)
	}
	assertEnd(t, after[0], "merge", 0, 3, "passed", nil)
}

// A review the review event approves before the reviewer completes ends passed with no report, and
// the reviewer's completion names its handoff on that ended run's record when it lands
// (reportEndedStep). A rollback between the two, under which the earlier release signs the issue
// off into the retro, starts no review: the review's place is none that release may have run
// again, so the roll forward leaves its counted run ended, and the late completion's report lands
// on the record this release counted.
func TestALateReviewersReportLandsOnItsCountedRecordAcrossARollback(t *testing.T) {
	const report = "6f1d2c3b-4a5e-4f60-8a7b-9c0d1e2f3a4b"
	pool := migratedPool(t)
	seedReview(t, pool, "green")
	engine := testEngine(config.DesignGateRootIssues, nil)
	// The start this release counted is in the store before the daemon boots, so it is written
	// with no boot pass before it (factTx); the boot then finds the run open and opens nothing.
	factTx(t, pool, engine, func(ctx context.Context, tx pgx.Tx) error {
		issue, err := engine.store.Issue(ctx, tx, "LEGION-208")
		if err != nil {
			return err
		}
		return engine.StartStepRecord(ctx, tx, *issue)
	})
	apply := applyFacts(t, pool, engine)
	apply("review", intake.PullRequestReview{Repo: "sjawhar/legion", Number: 42, State: "approved", CommitID: headOne, Author: testReviewApp,
		Body: reviewBody("the review", headOne)})
	if got := issueAt(t, pool, "LEGION-208"); got != phase.Accepting {
		t.Fatalf("LEGION-208 stands at %s after the approval, want accepting", got)
	}
	rollBackStepRecords(t, pool)
	// The earlier release signs the issue off: accepting moves to the retro, whose start raises
	// the implementer's entry.
	execSQL(t, pool, `update issues set phase = 'retro' where key = 'LEGION-208'`)
	execSQL(t, pool, `update phases set entry = entry + 1 where issue = 'LEGION-208' and role = 'implementer'`)
	rollForwardStepRecords(t, pool)
	before := len(stepWrites(t, pool, "LEGION-208"))
	if result := apply("reviewer-handoff", intake.HandoffComplete{Generation: 1, Issue: "LEGION-208", Role: claim.RoleReviewer,
		Claim: reviewClaim, Summary: "reviewed", HandoffRecord: report, HandoffVersion: 1}); result.Refusal != nil {
		t.Fatalf("the reviewer's completion was refused: %+v", result.Refusal)
	}
	after := stepWrites(t, pool, "LEGION-208")[before:]
	counted := StepID("LEGION-208", 1, placeOf("step", 4), 1)
	if len(after) != 1 || after[0].ID != counted || after[0].Report == nil || *after[0].Report != report {
		t.Fatalf("the late completion wrote %+v, want one write of the counted review's record %s naming %s", after, counted, report)
	}
}

// execSQL runs one statement on pool, with args.
func execSQL(t *testing.T, pool *pgxpool.Pool, sql string, args ...any) {
	t.Helper()
	if _, err := pool.Exec(t.Context(), sql, args...); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
}

// databaseNow is the database's clock, which the way back stamps the rollback's moment with.
func databaseNow(t *testing.T, pool *pgxpool.Pool) time.Time {
	t.Helper()
	var now time.Time
	if err := pool.QueryRow(t.Context(), `select clock_timestamp()`).Scan(&now); err != nil {
		t.Fatalf("read the database's clock: %v", err)
	}
	return now
}

// rollBackStepRecords runs the way back the roll forward's migration carries in its header
// (migrations.WayBack), as an operator runs it with the daemon stopped (docs/kubernetes.md,
// "Rolling back the release that records each step", carries the same block; internal/textcheck
// holds the two to one text).
func rollBackStepRecords(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	block, err := migrations.WayBack(migrations.StepRecordsRollForward)
	if err != nil {
		t.Fatalf("read the way back of the roll forward: %v", err)
	}
	execSQL(t, pool, block)
}

// rollForwardStepRecords is the newer image's boot over the store the earlier release left: its
// Migrate, which applies the roll forward the way back re-armed, and that one migration alone, and
// the boot that follows it (rebooted).
func rollForwardStepRecords(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	st, err := store.Open(t.Context(), pool.Config().ConnString())
	if err != nil {
		t.Fatalf("open the store: %v", err)
	}
	defer st.Close()
	if applied, err := st.Migrate(t.Context()); err != nil || applied != 1 {
		t.Fatalf("the roll forward's Migrate applied %d migrations, %v; want the one the way back re-armed", applied, err)
	}
	rebooted(t, pool)
}
