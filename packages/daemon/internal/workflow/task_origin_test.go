package workflow

// Every task the workflow queues carries its origin on the start row that carries it
// (record.SuperviseRequest.Origin), set by the constructor that wrote the row, so the value write
// it becomes is logged under the source that queued it and never under a reading of its text
// (dispatch://INFRA-2811 R7, decision 7 of its plan). The origins the record's reader falls back
// to for a row that names none (supervise's originOf) would give a phase start to every row here
// that names a phase and the operator's to every other, so each case below reads the row's own
// field rather than what the claim would record.

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// Each constructor of a start that carries a task names the origin decision 7 gives it: a
// transition's start, the held phase's retry (the fresh-session one too), an owed entry's work,
// the promotion's start of a mid-phase worker and the silence repair's and the boot's and a
// ready's send of an owed entry are phase starts; the phase's resume task a ready, the boot and
// the silence repair give a claim level with its entry is a resume; a CI settlement's wake and
// the backstop's are wakes; a production check's look is a check look; and an answered root's
// resume of its architect is a notice. Admission's architect start (internal/admit), the
// operator's task (internal/api), the daemon's controller start (internal/daemon) and the
// update round's ask (internal/supervise) are set where they are built, and tested there.
func TestEveryWorkflowTaskNamesTheOriginItsValueLogCarries(t *testing.T) {
	for _, tc := range []struct {
		name   string
		origin string
		// queue writes the case's start and answers the pool it wrote it in and its issue.
		queue func(t *testing.T) (*pgxpool.Pool, string)
	}{
		{"a transition's start", supervise.OriginPhaseStart, inLevelTester(func(ctx context.Context, tx pgx.Tx, e *Engine) error {
			return e.start(ctx, tx, resumeRuleIssue, claim.RoleTester, "Test the head.", false)
		})},
		{"the held phase's retry", supervise.OriginPhaseStart, inLevelTester(func(ctx context.Context, tx pgx.Tx, e *Engine) error {
			return e.restart(ctx, tx, resumeRuleIssue, claim.RoleTester, "Test it again: the architect retried the phase.", false)
		})},
		{"the fresh-session retry", supervise.OriginPhaseStart, inLevelTester(func(ctx context.Context, tx pgx.Tx, e *Engine) error {
			return e.restart(ctx, tx, resumeRuleIssue, claim.RoleTester, "Test it again in a new session.", true)
		})},
		{"an owed entry's work", supervise.OriginPhaseStart, inLevelTester(func(ctx context.Context, tx pgx.Tx, e *Engine) error {
			return e.SendOwedEntry(ctx, tx, resumeRuleIssue, claim.RoleTester, record.PhaseRow{Issue: entryIssue, Role: claim.RoleTester,
				Entry: seededEntry, EntryTask: seededEntryTask})
		})},
		{"a promoted mid-phase worker", supervise.OriginPhaseStart, inLevelTester(func(ctx context.Context, tx pgx.Tx, e *Engine) error {
			return e.StartPromotedWorker(ctx, tx, resumeRuleIssue, claim.RoleTester)
		})},
		{"a ready worker behind its entry", supervise.OriginPhaseStart, readyOfTester(seededEntry - 1)},
		{"a ready worker level with its entry", supervise.OriginResume, readyOfTester(seededEntry)},
		{"the boot's resume of a claim behind its entry", supervise.OriginPhaseStart, bootOfTester(seededEntry - 1)},
		{"the boot's resume of a claim level with its entry", supervise.OriginResume, bootOfTester(seededEntry)},
		{"the silence repair of a claim behind its entry", supervise.OriginPhaseStart, silenceOfTester(silenceEntry - 1)},
		{"the silence repair of a claim level with its entry", supervise.OriginResume, silenceOfTester(silenceEntry)},
		{"a CI settlement's wake", supervise.OriginWake, func(t *testing.T) (*pgxpool.Pool, string) {
			rig := newWakeRig(t)
			rig.apply("settled", settlement("head1", "build"))
			return rig.pool, "LEGION-208"
		}},
		{"the backstop's wake", supervise.OriginWake, func(t *testing.T) (*pgxpool.Pool, string) {
			rig := newWakeRig(t)
			id := armBackstop(t, rig.pool, claim.RoleImplementer, wakeAt.Add(-time.Minute))
			rig.apply("backstop", intake.PodBackstopDue{ID: id, Issue: "LEGION-208", Role: claim.RoleImplementer})
			return rig.pool, "LEGION-208"
		}},
		{"a production check's look", supervise.OriginCheckLook, func(t *testing.T) (*pgxpool.Pool, string) {
			return childCheckRig(t).pool, "LEGION-209"
		}},
		{"an answered root's resume", supervise.OriginNotice, func(t *testing.T) (*pgxpool.Pool, string) {
			pool := migratedPool(t)
			root := awaitingRoot(3)
			root.HandedOver = true
			seedIssue(t, pool, root)
			resumeAnsweredRoot(t, pool, root)
			return pool, root.Key
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool, issue := tc.queue(t)
			tasks := tasksQueued(t, pool, issue)
			if len(tasks) == 0 {
				t.Fatalf("%s queued no task on %s", tc.name, issue)
			}
			for _, task := range tasks {
				if task.Origin != tc.origin {
					t.Fatalf("%s queued %q under origin %q, want %q", tc.name, task.Task, task.Origin, tc.origin)
				}
			}
		})
	}
}

// inLevelTester is a case that calls one of the engine's constructors directly, in a fact's
// transaction, on LEGION-208 with its tester level with the entry its phases row names.
func inLevelTester(build func(ctx context.Context, tx pgx.Tx, e *Engine) error) func(t *testing.T) (*pgxpool.Pool, string) {
	return func(t *testing.T) (*pgxpool.Pool, string) {
		pool := migratedPool(t)
		seedTester(t, pool, finishedTester())
		engine := testEngine(config.DesignGateRootIssues, nil)
		withCommitScope(t, pool, func(ctx context.Context, tx pgx.Tx) error { return build(ctx, tx, engine) })
		return pool, entryIssue
	}
}

// readyOfTester is a case whose start the ready of LEGION-208's tester queues, the tester last
// given entry given and never written a task.
func readyOfTester(given int64) func(t *testing.T) (*pgxpool.Pool, string) {
	return func(t *testing.T) (*pgxpool.Pool, string) {
		pool := migratedPool(t)
		rec := finishedTester()
		rec.written, rec.finished, rec.finishedIn = 0, 0, ""
		token := seedTester(t, pool, rec)
		seedGivenEntry(t, pool, token, given)
		readyTester(t, pool, testEngine(config.DesignGateRootIssues, nil), rec.generation)
		return pool, entryIssue
	}
}

// bootOfTester is a case whose start the boot queues for LEGION-208's tester, suspended by the
// daemon, last given entry given and never written a task.
func bootOfTester(given int64) func(t *testing.T) (*pgxpool.Pool, string) {
	return func(t *testing.T) (*pgxpool.Pool, string) {
		pool := migratedPool(t)
		rec := finishedTester()
		rec.written, rec.finished, rec.finishedIn = 0, 0, ""
		rec.state, rec.suspendedBy = supervise.StateSuspended, supervise.SuspendedByDaemon
		token := seedTester(t, pool, rec)
		seedGivenEntry(t, pool, token, given)
		if _, err := intake.ApplyFact(t.Context(), pool, "daemon", "boot:LEGION:1", intake.DaemonBooted{Boot: 1},
			testEngine(config.DesignGateRootIssues, nil), admissionStub{}); err != nil {
			t.Fatalf("apply the boot: %v", err)
		}
		return pool, entryIssue
	}
}

// silenceOfTester is a case whose start the silence repair queues for the silent tester of the
// silence tests, last given entry given.
func silenceOfTester(given int64) func(t *testing.T) (*pgxpool.Pool, string) {
	return func(t *testing.T) (*pgxpool.Pool, string) {
		pool := migratedPool(t)
		rec := aSilentTester()
		rec.given = given
		seedSilence(t, pool, rec)
		if err := applySilence(t, pool, testEngine(config.DesignGateRootIssues, nil), aSilenceOf(claim.RoleTester, 1)); err != nil {
			t.Fatalf("apply the silence: %v", err)
		}
		return pool, silenceIssue
	}
}
