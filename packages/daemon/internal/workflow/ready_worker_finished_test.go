package workflow

// R4 of dispatch://INFRA-2811: a worker that finished the last task written to it, in the session
// it still runs, is given no task when its process is relaunched or the daemon boots, unless
// something new is owed to it. The phase's resume task is queued at a ready (readyWorkerTask) and
// at the boot (booted) only for a claim never written a task, or whose session is new since it
// finished; a phase start, an owed entry's work and a wake's task are written as before.
//
// This package is handed each relaunch at the ready that ends it, read against the claim as the
// store holds it: the supervisor's relaunches — a death, a stale plugin, a displacement — are the
// supervisor's own, so each case here seeds the record that relaunch leaves (the task finished,
// the session kept) and names the relaunch. The cancelled idle release is driven through the
// store's own cancel (store.CancelIssueRelease), whose start rows are what bring the claim back.
//
// "No value write" is read here as "no start row carrying a task": a claim's value is written
// only when its machine queues a task (supervise's queue), and what reaches that queue from the
// workflow is a start row that carries one, which the outbox hands the machine as its delivery.

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"math"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/pods"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/store/migrations"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/testpg/testdb"
)

// finishedSession is the session every finished claim of this file holds and finished its last
// task in, and finishedTask the number of that task: the second the record wrote it.
const (
	finishedSession = "ses-tester"
	finishedTask    = 2
)

// finishedLine is the line a ready logs when it gives a finished claim nothing.
const finishedLine = `msg="workflow: a ready worker that finished its last task in this session is given nothing; its own reminder wakes it"`

// resumeRuleIssue is the issue every case of this file stands on: LEGION-208 in testing, its
// tester the phase's worker.
var resumeRuleIssue = record.Issue{Key: entryIssue, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets",
	Tree: entryIssue, Project: "LEGION", Title: "root", Phase: phase.Testing, Generation: 1, Status: "testing", Rank: "U", HandedOver: true}

// workerRecord is LEGION-208's tester claim as the record holds it when the ready or the boot
// reads it: its state and the source of its suspension, the launch it is at, the session it runs,
// and its task numbers — the highest the record wrote it, the last it finished and the session it
// finished that one in. mark is the rest of what the relaunch that left it wrote on the claim, as
// a SQL `set` list, "" for nothing.
type workerRecord struct {
	state       supervise.ClaimState
	suspendedBy supervise.SuspendSource
	generation  uint64
	session     string
	written     uint64
	finished    uint64
	finishedIn  string
	mark        string
}

// finishedTester is the tester ready again at launch 3 after a relaunch, in the session it
// finished its last task in.
func finishedTester() workerRecord {
	return workerRecord{state: supervise.StateReady, generation: 3, session: finishedSession,
		written: finishedTask, finished: finishedTask, finishedIn: finishedSession}
}

// seedTesterIssue records LEGION-208 as every case reads it: in testing, holding its slot, with
// its tester's phases row on the seeded entry. It answers the tester's claim token.
func seedTesterIssue(t *testing.T, pool *pgxpool.Pool) claim.Token {
	t.Helper()
	seedIssue(t, pool, resumeRuleIssue)
	seedSlot(t, pool, record.Slot{Issue: entryIssue, Index: 0})
	token := testerToken(t, entryIssue)
	seedEntryRow(t, pool, claim.RoleTester, token)
	return token
}

// testerToken is issue's tester claim token.
func testerToken(t *testing.T, issue string) claim.Token {
	t.Helper()
	project, err := claim.ProjectToken("LEGION")
	if err != nil {
		t.Fatal(err)
	}
	token, err := claim.NewToken(project, issue, claim.RoleTester)
	if err != nil {
		t.Fatal(err)
	}
	return token
}

// seedTester records LEGION-208's tester claim as rec says, level with the entry its phases row
// names, holding no task.
func seedTester(t *testing.T, pool *pgxpool.Pool, rec workerRecord) claim.Token {
	t.Helper()
	token := seedTesterIssue(t, pool)
	project, err := claim.ProjectToken("LEGION")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(t.Context(), `insert into claims (token, project, tree, issue, role, generation, session, session_file,
		state, suspended_by, launch_failures, prompt_failures, prompt_retires, uncertain_streak, given_entry,
		task_written, task_taken, task_finished, task_finished_session)
		values ($1, $2, $3, $3, 'tester', $4, $5, '/sessions/tester.jsonl', $6, $7, 0, 0, 0, 0, $8, $9, $10, $10, $11)`,
		string(token), project, entryIssue, int64(rec.generation), rec.session, string(rec.state), string(rec.suspendedBy),
		int64(seededEntry), int64(rec.written), int64(rec.finished), rec.finishedIn); err != nil {
		t.Fatalf("seed the tester: %v", err)
	}
	if rec.mark != "" {
		if _, err := pool.Exec(t.Context(), `update claims set `+rec.mark+` where token = $1`, string(token)); err != nil {
			t.Fatalf("record what the relaunch wrote on the tester (%s): %v", rec.mark, err)
		}
	}
	return token
}

// readyTester applies the ready of the tester's launch generation, through engine.
func readyTester(t *testing.T, pool *pgxpool.Pool, engine *Engine, generation uint64) {
	t.Helper()
	if _, err := intake.ApplyFact(t.Context(), pool, "supervise", fmt.Sprintf("supervise:tester:%d:ready", generation), intake.ClaimReady{
		Issue: entryIssue, Role: claim.RoleTester, Launch: generation}, engine, admissionStub{}); err != nil {
		t.Fatalf("apply the ready of launch %d: %v", generation, err)
	}
}

// loggingEngine is the test engine writing its journal to logged.
func loggingEngine(logged *bytes.Buffer) *Engine {
	return testEngine(config.DesignGateRootIssues, slog.New(slog.NewTextHandler(logged, nil)))
}

// tasksQueued is every supervise start row of issue that carries a task, oldest first: the rows
// whose delivery queues a task on the claim, and so writes its value.
func tasksQueued(t *testing.T, pool *pgxpool.Pool, issue string) []record.SuperviseRequest {
	t.Helper()
	var tasks []record.SuperviseRequest
	for _, start := range superviseRowsOf(t, pool, issue, "start") {
		if start.Task != "" {
			tasks = append(tasks, start)
		}
	}
	return tasks
}

// wantNothingQueued fails unless issue's outbox holds no start row at all, after what.
func wantNothingQueued(t *testing.T, pool *pgxpool.Pool, issue, what string) {
	t.Helper()
	if starts := superviseRowsOf(t, pool, issue, "start"); len(starts) != 0 {
		t.Fatalf("after %s the outbox holds the starts %+v, want none: a worker that finished its last task in the session it runs is given nothing", what, starts)
	}
}

// wantResumeTask fails unless the outbox holds exactly one start, the phase's resume task for
// LEGION-208's tester, marked as the resume and under a resume's origin.
func wantResumeTask(t *testing.T, pool *pgxpool.Pool, what string) {
	t.Helper()
	starts := superviseRowsOf(t, pool, entryIssue, "start")
	if len(starts) != 1 {
		t.Fatalf("after %s the outbox holds the starts %+v, want the one resume task", what, starts)
	}
	start := starts[0]
	if start.Role != claim.RoleTester || start.Task != ResumePhaseTask(resumeRuleIssue) || !start.ResumeTask ||
		start.Phase != phase.Testing || start.Entry != 0 || start.Origin != supervise.OriginResume {
		t.Fatalf("after %s the start = %+v, want the tester's testing resume task at no entry, under origin %q", what, start, supervise.OriginResume)
	}
}

// deleteStarts finishes every start row of issue, as the outbox does once it has run them.
func deleteStarts(t *testing.T, pool *pgxpool.Pool, issue string) {
	t.Helper()
	if _, err := pool.Exec(t.Context(), `delete from outbox where issue = $1 and kind = 'supervise' and payload->>'op' = 'start'`, issue); err != nil {
		t.Fatalf("run the starts of %s: %v", issue, err)
	}
}

// relaunchClaim records token's claim brought back ready at launch generation, in the session it
// holds: the record a relaunch leaves once its new process's agent has said it is ready.
func relaunchClaim(t *testing.T, pool *pgxpool.Pool, token claim.Token, generation uint64) {
	t.Helper()
	if _, err := pool.Exec(t.Context(), `update claims set state = 'ready', suspended_by = '', generation = $2 where token = $1`,
		string(token), int64(generation)); err != nil {
		t.Fatalf("relaunch %s at launch %d: %v", token, generation, err)
	}
}

// queueTask records task as token's pending task under number, as the claim's machine queues a
// start row's task when the outbox hands it over (supervise's queue): the delivery and the record's
// highest written number in one write.
func queueTask(t *testing.T, pool *pgxpool.Pool, token claim.Token, number uint64, task, origin string, taskPhase phase.Phase, generation uint64) {
	t.Helper()
	if _, err := pool.Exec(t.Context(), `insert into pending_task_deliveries (claim_token, delivery_id, task, phase, generation,
		queued_at, number, origin) values ($1, 'outbox:1', $2, $3, $4, now(), $5, $6)`,
		string(token), task, string(taskPhase), int64(generation), int64(number), origin); err != nil {
		t.Fatalf("queue %s task %d: %v", token, number, err)
	}
	if _, err := pool.Exec(t.Context(), `update claims set task_written = $2 where token = $1`, string(token), int64(number)); err != nil {
		t.Fatalf("record task %d as written to %s: %v", number, token, err)
	}
}

// A worker that finished its last task in the session it runs, ready again with nothing pending,
// is given nothing at the ready its relaunch reaches, whichever relaunch that was, and the ready
// says so in its journal, naming the claim and the task it finished. Before, each such ready
// queued the phase's resume task, a turn of work the worker had already done.
//
// The negative control is this test on the merge base: the ready of every relaunch queues the
// testing phase's resume task.
func TestReadyWorkerTaskWritesNothingForAFinishedClaim(t *testing.T) {
	for _, tc := range []struct {
		name string
		// relaunch seeds the record the relaunch leaves and answers the launch its ready is of.
		relaunch func(t *testing.T, pool *pgxpool.Pool) uint64
	}{
		{name: "a death's relaunch", relaunch: func(t *testing.T, pool *pgxpool.Pool) uint64 {
			// The death is charged and its cause kept; the session is resumed on the new launch.
			rec := finishedTester()
			rec.mark = `deaths = 1, last_death_cause = 'the tester''s container exited with status 137'`
			seedTester(t, pool, rec)
			return rec.generation
		}},
		{name: "a stale-plugin relaunch", relaunch: func(t *testing.T, pool *pgxpool.Pool) uint64 {
			// The relaunch moves the claim onto the current plugin, its session resumed.
			rec := finishedTester()
			rec.mark = fmt.Sprintf("plugin_contract = %d", supervise.TaskTakeContract)
			seedTester(t, pool, rec)
			return rec.generation
		}},
		{name: "a displacement", relaunch: func(t *testing.T, pool *pgxpool.Pool) uint64 {
			// The daemon replaced the pod under the process: relaunched uncharged, session resumed.
			rec := finishedTester()
			seedTester(t, pool, rec)
			return rec.generation
		}},
		{name: "the cancelled idle release after its role stop", relaunch: func(t *testing.T, pool *pgxpool.Pool) uint64 {
			// The release's role stop suspended the tester, released, at launch 2; the release is
			// then cancelled, and its start row brings the claim back in the pod it was in.
			rec := finishedTester()
			rec.state, rec.suspendedBy, rec.generation = supervise.StateSuspended, supervise.SuspendedByReleased, 2
			token := seedTester(t, pool, rec)
			cancelTheRelease(t, pool, claim.RoleTester)
			relaunchClaim(t, pool, token, 3)
			return 3
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := migratedPool(t)
			var logged bytes.Buffer
			launch := tc.relaunch(t, pool)

			readyTester(t, pool, loggingEngine(&logged), launch)

			wantNothingQueued(t, pool, entryIssue, "the ready of "+tc.name)
			if lines := strings.Count(logged.String(), finishedLine); lines != 1 {
				t.Fatalf("the journal logged %d lines saying the finished worker is given nothing, want one: %s", lines, logged.String())
			}
			for _, attribute := range []string{"claim=" + string(testerToken(t, entryIssue)), "issue=" + entryIssue, "role=tester",
				"task=2", "session=" + finishedSession} {
				if !strings.Contains(logged.String(), attribute) {
					t.Fatalf("the line saying the finished worker is given nothing does not name %s: %s", attribute, logged.String())
				}
			}
		})
	}
}

// cancelTheRelease marks LEGION-208's release as having suspended role, cancels it through the
// store as the daemon's outbox does (store.CancelIssueRelease), checks the one start row the cancel
// queued — the resume a wake writes, carrying no task — and runs it, as the outbox does.
func cancelTheRelease(t *testing.T, pool *pgxpool.Pool, role claim.Role) {
	t.Helper()
	ctx := t.Context()
	if _, err := pool.Exec(ctx, `insert into issue_releases (project, issue, generation, first_row, boundary_row, last_row,
		started_row, role, suspended, marked_at) values ('legion', $1, 1, 30, 31, 32, 0, 'architect', $2, now())`,
		entryIssue, []string{string(role)}); err != nil {
		t.Fatalf("mark the release of %s: %v", entryIssue, err)
	}
	st, err := store.Open(ctx, pool.Config().ConnString())
	if err != nil {
		t.Fatalf("open the store: %v", err)
	}
	defer st.Close()
	// The row the daemon's outbox builds for each claim a cancelled release still holds
	// (releaseResume): a start at the issue's generation that names no phase and carries no task.
	resume := func(role claim.Role, generation uint64) (store.IdleReleaseRow, error) {
		row, err := record.NewOutboxRow(entryIssue, record.SuperviseRequest{Op: "start", Tree: entryIssue, Role: role,
			Generation: generation}, seededRaisedAt)
		return store.IdleReleaseRow{Kind: string(row.Kind), Payload: row.Payload}, err
	}
	resumed, _, err := st.CancelIssueRelease(ctx, "legion", entryIssue, resume, record.OutOfWorkflow)
	if err != nil || len(resumed) != 1 || resumed[0] != role {
		t.Fatalf("the cancel resumed %v, %v; want the %s it suspended", resumed, err, role)
	}
	starts := superviseRowsOf(t, pool, entryIssue, "start")
	if len(starts) != 1 || starts[0].Role != role || starts[0].Task != "" || starts[0].Phase != "" {
		t.Fatalf("the cancel queued %+v, want the one resume of the %s, carrying no task", starts, role)
	}
	deleteStarts(t, pool, entryIssue)
}

// A worker that finished its last task in an earlier session, and now runs a fresh one — the
// operator's fresh-session retry, a relaunch that began one of its own — is given its phase's
// resume task at its ready: a new session has no reminder state to wake it.
func TestReadyWorkerTaskGivesAFreshSessionItsResumeTask(t *testing.T) {
	pool := migratedPool(t)
	rec := finishedTester()
	rec.session = "ses-fresh"
	seedTester(t, pool, rec)

	readyTester(t, pool, testEngine(config.DesignGateRootIssues, nil), rec.generation)

	wantResumeTask(t, pool, "the ready of a fresh session")
}

// A worker whose session was abandoned — the session's file lost, the claim's session given up —
// and replaced by one its relaunch began, is given its phase's resume task at its ready: the
// replacement has read nothing of the task the abandoned session finished.
func TestReadyWorkerTaskGivesAnAbandonedSessionReplacedItsResumeTask(t *testing.T) {
	pool := migratedPool(t)
	rec := finishedTester()
	rec.session = "ses-replacement"
	seedTester(t, pool, rec)

	readyTester(t, pool, testEngine(config.DesignGateRootIssues, nil), rec.generation)

	wantResumeTask(t, pool, "the ready of a session that replaced an abandoned one")
}

// A worker to which the record never wrote a task is given its phase's resume task at its ready,
// as before: it has finished nothing, and the resume is the work of the phase it holds.
func TestReadyWorkerTaskGivesAClaimNeverWrittenATaskItsResumeTask(t *testing.T) {
	pool := migratedPool(t)
	rec := finishedTester()
	rec.written, rec.finished, rec.finishedIn = 0, 0, ""
	seedTester(t, pool, rec)

	readyTester(t, pool, testEngine(config.DesignGateRootIssues, nil), rec.generation)

	wantResumeTask(t, pool, "the ready of a claim never written a task")
}

// The rule covers the resume task alone. A phase start queued for the finished worker, and a
// wake's task, are each written once: the start row that carries each is queued, and the ready
// of the relaunch that follows — before that row runs and after its task is pending on the claim
// — queues no second task beside it.
func TestReadyWorkerTaskStillWritesAPhaseStartAndAWakeTaskOnce(t *testing.T) {
	t.Run("a phase start", func(t *testing.T) {
		pool := migratedPool(t)
		token := seedTester(t, pool, finishedTester())
		engine := testEngine(config.DesignGateRootIssues, nil)
		const task = "Test the head the implementer pushed."
		withCommitScope(t, pool, func(ctx context.Context, tx pgx.Tx) error {
			return engine.start(ctx, tx, resumeRuleIssue, claim.RoleTester, task, false)
		})
		wantOnlyTask(t, pool, entryIssue, task, supervise.OriginPhaseStart, "the phase start")

		relaunchClaim(t, pool, token, 4)
		readyTester(t, pool, engine, 4)
		wantOnlyTask(t, pool, entryIssue, task, supervise.OriginPhaseStart, "the ready while the phase start waits")

		deleteStarts(t, pool, entryIssue)
		queueTask(t, pool, token, finishedTask+1, task, supervise.OriginPhaseStart, phase.Testing, resumeRuleIssue.Generation)
		relaunchClaim(t, pool, token, 5)
		readyTester(t, pool, engine, 5)
		wantNothingQueued(t, pool, entryIssue, "the ready of a worker holding its phase start's task")
	})

	t.Run("a wake's task", func(t *testing.T) {
		rig := newWakeRig(t)
		implementer := wakeToken(t, claim.RoleImplementer)
		if _, err := rig.pool.Exec(t.Context(), `update claims set task_written = $2, task_taken = $2, task_finished = $2,
			task_finished_session = session where token = $1`, string(implementer), int64(finishedTask)); err != nil {
			t.Fatalf("record the implementer's last task as finished in its session: %v", err)
		}
		rig.apply("settled", settlement("head1", "build"))
		oneWake(t, rig.pool, claim.RoleImplementer, pods.WakeChecks, "head1", "build")
		woken := tasksQueued(t, rig.pool, "LEGION-208")
		if len(woken) != 1 || woken[0].Origin != supervise.OriginWake {
			t.Fatalf("the wake queued %+v, want its one task under origin %q", woken, supervise.OriginWake)
		}

		deleteStarts(t, rig.pool, "LEGION-208")
		queueTask(t, rig.pool, implementer, finishedTask+1, woken[0].Task, supervise.OriginWake, "", wakeIssueGeneration)
		relaunchClaim(t, rig.pool, implementer, 2)
		rig.apply("ready", intake.ClaimReady{Issue: "LEGION-208", Role: claim.RoleImplementer, Launch: 2})
		wantNothingQueued(t, rig.pool, "LEGION-208", "the ready of a woken worker holding its wake's task")
	})
}

// withCommitScope runs fn in one transaction on pool under a commit scope, as a fact's handler
// runs (intake.WithCommitHooks), so a constructor the test calls directly may log on commit.
func withCommitScope(t *testing.T, pool *pgxpool.Pool, fn func(ctx context.Context, tx pgx.Tx) error) {
	t.Helper()
	seedRecord(t, pool, func(tx pgx.Tx) error {
		ctx, _ := intake.WithCommitHooks(t.Context())
		return fn(ctx, tx)
	})
}

// wantOnlyTask fails unless issue's outbox holds exactly one start carrying a task, task, under
// origin, after what.
func wantOnlyTask(t *testing.T, pool *pgxpool.Pool, issue, task, origin, what string) {
	t.Helper()
	tasks := tasksQueued(t, pool, issue)
	if len(tasks) != 1 || tasks[0].Task != task || tasks[0].Origin != origin {
		t.Fatalf("after %s the outbox holds the tasks %+v, want the one %q under origin %q", what, tasks, task, origin)
	}
}

// The boot's resume by rule brings back a suspended claim that finished its last task in the
// session it holds with no task: one start row that names the phase, carries none, resumes by
// rule and waits for no slot. The claim is still resumed, since nothing else would bring it back,
// and the ready of that relaunch gives it nothing either. A claim whose session is new since it
// finished is given the phase's resume task, as before.
//
// The negative control is this test on the merge base: the boot gives the finished claim the
// testing phase's resume task.
func TestBootedResumesAFinishedClaimWithNoTask(t *testing.T) {
	t.Run("in the session it finished its task in", func(t *testing.T) {
		pool := migratedPool(t)
		rec := finishedTester()
		rec.state, rec.suspendedBy, rec.generation = supervise.StateSuspended, supervise.SuspendedByDaemon, 2
		token := seedTester(t, pool, rec)
		var logged bytes.Buffer
		engine := loggingEngine(&logged)

		if _, err := intake.ApplyFact(t.Context(), pool, "daemon", "boot:LEGION:1", intake.DaemonBooted{Boot: 1}, engine, admissionStub{}); err != nil {
			t.Fatalf("apply the boot: %v", err)
		}

		starts := superviseRowsOf(t, pool, entryIssue, "start")
		if len(starts) != 1 {
			t.Fatalf("the boot queued %+v, want the one start that resumes the suspended claim", starts)
		}
		start := starts[0]
		if start.Role != claim.RoleTester || start.Task != "" || start.Origin != "" || start.Phase != phase.Testing ||
			!start.RuleResume || start.ResumeTask || start.NeedsSlot || start.Entry != 0 || start.Generation != resumeRuleIssue.Generation {
			t.Fatalf("the boot's start = %+v, want the tester's resume by rule in testing, carrying no task and waiting for no slot", start)
		}
		const bootLine = `msg="workflow: the boot resumes a suspended claim that finished its last task in this session with no task; its own reminder wakes it"`
		if !strings.Contains(logged.String(), bootLine) || !strings.Contains(logged.String(), "task=2") {
			t.Fatalf("the boot's journal does not say the finished claim is resumed with no task: %s", logged.String())
		}
		assertEntryUntouched(t, pool, claim.RoleTester, "the boot")

		deleteStarts(t, pool, entryIssue)
		relaunchClaim(t, pool, token, 3)
		readyTester(t, pool, engine, 3)
		wantNothingQueued(t, pool, entryIssue, "the ready of the claim the boot resumed")
	})

	t.Run("in a session new since it finished", func(t *testing.T) {
		pool := migratedPool(t)
		rec := finishedTester()
		rec.state, rec.suspendedBy, rec.generation, rec.session = supervise.StateSuspended, supervise.SuspendedByDaemon, 2, "ses-fresh"
		seedTester(t, pool, rec)

		if _, err := intake.ApplyFact(t.Context(), pool, "daemon", "boot:LEGION:1", intake.DaemonBooted{Boot: 1},
			testEngine(config.DesignGateRootIssues, nil), admissionStub{}); err != nil {
			t.Fatalf("apply the boot: %v", err)
		}

		wantResumeTask(t, pool, "the boot of a claim in a session new since it finished")
		if start := superviseRowsOf(t, pool, entryIssue, "start")[0]; !start.RuleResume || !start.NeedsSlot {
			t.Fatalf("the boot's resume task = %+v, want it resuming by rule and waiting for the slot", start)
		}
	})
}

// taskNumbersMigration is the migration that numbers each claim's first task
// (20261113175649_task_numbers): this file migrates a store through everything before it, writes
// a claim of the release before, and lets the store's own migration fill it.
const taskNumbersMigration = 20261113175649

// The rollout's two waiting workers, as the migration numbers them, are given nothing at their
// relaunch. A tester whose task was confirmed and retired before the upgrade — it served its run
// and holds no pending task — is recorded as having finished task 1 in the session it holds, so
// the ready of its relaunch gives it nothing; one holding a task nobody has confirmed is recorded
// as written task 1 and taken nothing, so it keeps that task, untaken, and its ready gives it no
// second.
//
// The negative control is this test on the merge base, where the record holds no task numbers:
// the ready of the first tester's relaunch queues the testing phase's resume task.
func TestWaitingWorkerAfterMigrationIsGivenNothingAtItsRelaunch(t *testing.T) {
	ctx := t.Context()
	dsn := testdb.Database(t, "legion_workflow_migration")
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect to the store: %v", err)
	}
	t.Cleanup(pool.Close)
	migrateBefore(t, pool, taskNumbersMigration)

	const retired, holding = "LEGION-208", "LEGION-210"
	project, err := claim.ProjectToken("LEGION")
	if err != nil {
		t.Fatal(err)
	}
	for _, seed := range []struct {
		issue, session, state string
		serving               int64
	}{
		{retired, "ses-retired", "idle", 1},
		{holding, "ses-holding", "ready", 0},
	} {
		if _, err := pool.Exec(ctx, `insert into claims (token, project, tree, issue, role, generation, session, session_file, state,
			launch_failures, prompt_failures, prompt_retires, uncertain_streak, serving_generation, served_entry, given_entry)
			values ($1, $2, $3, $3, 'tester', 1, $4, '', $5, 0, 0, 0, 0, $6, $7, $7)`,
			string(testerToken(t, seed.issue)), project, seed.issue, seed.session, seed.state, seed.serving, int64(seededEntry)); err != nil {
			t.Fatalf("record the tester of %s before the migration: %v", seed.issue, err)
		}
	}
	// The push path's record of a task acknowledged and never confirmed by a turn.
	if _, err := pool.Exec(ctx, `insert into pending_task_deliveries (claim_token, delivery_id, task, phase, generation, queued_at,
		delivered_at, marked_by, prompted) values ($1, 'outbox:31', 'Test the change.', 'testing', 1, now(), now(), 'outbox:31', true)`,
		string(testerToken(t, holding))); err != nil {
		t.Fatalf("record the held task before the migration: %v", err)
	}

	st, err := store.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("open the store: %v", err)
	}
	t.Cleanup(st.Close)
	if _, err := st.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	for issue, want := range map[string]string{
		retired: "written 1 taken 1 finished 1 in ses-retired",
		holding: "written 1 taken 0 finished 0 in ",
	} {
		if got := taskNumbersOf(t, pool, testerToken(t, issue)); got != want {
			t.Fatalf("the migration recorded the tester of %s as %q, want %q", issue, got, want)
		}
	}

	engine := testEngine(config.DesignGateRootIssues, nil)
	for index, key := range []string{retired, holding} {
		seedIssue(t, pool, record.Issue{Key: key, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Tree: key,
			Project: "LEGION", Title: "root", Phase: phase.Testing, Generation: 1, Status: "testing", Rank: "U", HandedOver: true})
		seedSlot(t, pool, record.Slot{Issue: key, Index: index})
		seedPhase(t, pool, record.PhaseRow{Issue: key, Role: claim.RoleTester, Claim: testerToken(t, key), Entry: seededEntry,
			EntryTask: seededEntryTask, EntryRaisedAt: seededRaisedAt})
		relaunchClaim(t, pool, testerToken(t, key), 2)
		if _, err := intake.ApplyFact(ctx, pool, "supervise", "supervise:"+key+":tester:2:ready",
			intake.ClaimReady{Issue: key, Role: claim.RoleTester, Launch: 2}, engine, admissionStub{}); err != nil {
			t.Fatalf("apply the ready of %s's relaunched tester: %v", key, err)
		}
		wantNothingQueued(t, pool, key, "the ready of "+key+"'s relaunched tester")
	}
	var number int64
	var confirmed bool
	if err := pool.QueryRow(ctx, `select number, confirmed_at is not null from pending_task_deliveries where claim_token = $1`,
		string(testerToken(t, holding))).Scan(&number, &confirmed); err != nil || number != 1 || confirmed {
		t.Fatalf("the held task after the relaunch = number %d, confirmed %t, %v; want task 1, still untaken", number, confirmed, err)
	}
}

// The silence repair writes origin `silence` in both of its branches, whatever task each writes
// (repairSilence, decision 7 of INFRA-2811's plan): the work of the entry a silent claim stands
// behind, and the phase's resume task for a claim level with its row. Neither names `resume`,
// the origin R9 reads for writes to a worker that finished its last task, nor `phase-start`: the
// repair is the backstop of a worker whose own report does not explain its quiet, and its writes
// are counted apart. Each silent tester here is a waiting one, having finished its last task in
// the session it runs, because that is the worker the two origins are told apart for; the level
// start keeps its resume mark, which is the outbox's rule and no word on the source.
//
// The negative control is this test before the change: the level start names `resume` and the
// behind start `phase-start`.
func TestWaitingWorkerSilenceRepairWritesOriginSilenceInBothBranches(t *testing.T) {
	for _, tc := range []struct {
		name string
		// given is the entry the silent claim was last given work of (record.RoleClaim.GivenEntry).
		given int64
		// task is the task the repair's start must carry, entry its entry, and resume whether it
		// is marked the phase's resume task.
		task   func(record.Issue) string
		entry  int64
		resume bool
	}{
		{name: "behind the entry its row names", given: silenceEntry - 1,
			task: func(record.Issue) string { return silenceEntryTask }, entry: silenceEntry},
		{name: "level with the entry its row names", given: silenceEntry,
			task: ResumePhaseTask, entry: 0, resume: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := migratedPool(t)
			rec := aSilentTester()
			rec.given = tc.given
			issue := seedSilence(t, pool, rec)
			// seedSilence's claim runs session 'ses': it finished its last task, the second the
			// record wrote it, in that same session.
			if _, err := pool.Exec(t.Context(), `update claims set task_written = $2, task_taken = $2, task_finished = $2,
				task_finished_session = session where token = $1`, string(silenceToken(t, claim.RoleTester)), int64(finishedTask)); err != nil {
				t.Fatalf("record the silent tester's finished task: %v", err)
			}

			if err := applySilence(t, pool, testEngine(config.DesignGateRootIssues, nil), aSilenceOf(claim.RoleTester, 1)); err != nil {
				t.Fatalf("apply the silence: %v", err)
			}

			starts := superviseRowsOf(t, pool, silenceIssue, "start")
			if len(starts) != 1 {
				t.Fatalf("the silence queued %+v, want the one start it writes for the silent claim", starts)
			}
			start := starts[0]
			if start.Task != tc.task(issue) || start.Entry != tc.entry || start.ResumeTask != tc.resume {
				t.Fatalf("the silence of a claim given entry %d sent %q at entry %d, resume task %t; want %q at entry %d, resume task %t",
					tc.given, start.Task, start.Entry, start.ResumeTask, tc.task(issue), tc.entry, tc.resume)
			}
			if start.Origin != supervise.OriginSilence {
				t.Fatalf("the silence repair's start names origin %q, want %q: neither %q nor %q, so its writes are counted apart",
					start.Origin, supervise.OriginSilence, supervise.OriginResume, supervise.OriginPhaseStart)
			}
		})
	}
}

// taskNumbersOf is token's task numbers as the store holds them, in one line a failure prints
// whole.
func taskNumbersOf(t *testing.T, pool *pgxpool.Pool, token claim.Token) string {
	t.Helper()
	var written, taken, finished int64
	var session string
	if err := pool.QueryRow(t.Context(), `select task_written, task_taken, task_finished, task_finished_session
		from claims where token = $1`, string(token)).Scan(&written, &taken, &finished, &session); err != nil {
		t.Fatalf("read the task numbers of %s: %v", token, err)
	}
	return fmt.Sprintf("written %d taken %d finished %d in %s", written, taken, finished, session)
}

// migrateBefore applies every embedded migration below version to pool's empty database, each in
// a transaction of its own that records its version, as the store's own migration does
// (store.Store.Migrate, whose step is not exported): a store the release before the migration
// under test left, which the store then takes through that migration itself.
func migrateBefore(t *testing.T, pool *pgxpool.Pool, version int) {
	t.Helper()
	ctx := t.Context()
	all, err := migrations.All()
	if err != nil {
		t.Fatalf("read the embedded migrations: %v", err)
	}
	widened := false
	for _, migration := range all {
		if migration.Version >= version {
			return
		}
		if err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, migration.SQL); err != nil {
				return err
			}
			// A timestamp version is past int4, so the first one widens the column, as the store's
			// own migration does.
			if migration.Version > math.MaxInt32 && !widened {
				if _, err := tx.Exec(ctx, "alter table schema_version alter column version type bigint"); err != nil {
					return err
				}
			}
			_, err := tx.Exec(ctx, "insert into schema_version (version) values ($1::bigint)", migration.Version)
			return err
		}); err != nil {
			t.Fatalf("apply %s: %v", migration.Name, err)
		}
		widened = widened || migration.Version > math.MaxInt32
	}
}
