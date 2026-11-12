package workflow

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// The issue every case of this file stands on, the role that works its phase, and the
// assignment its phase row already names. The role is the tester of an issue in testing, which
// is the shape the incident this work was filed for was read in (dispatch://INFRA-3163).
const (
	silenceIssue      = "LEGION-209"
	silenceAssignment = "A1"
	silenceBound      = 30 * time.Minute
)

// silenceRecord is the record one case stands on. aSilentTester is the record of a worker the
// rule names, and each case below changes exactly one field of it, so what the case proves is
// the field it changed.
type silenceRecord struct {
	// unrecorded leaves the issue, its phase row and its claim out of the record altogether.
	unrecorded bool
	phase      phase.Phase
	// generation is the issue's, which the fact's own generation is held against.
	generation uint64
	lingers    bool
	// assignment is the phase row's, "" for the row that names none.
	assignment string
	// completedAt is the phase row's record of a completion the workflow accepted for this
	// phase entry, zero for a row that carries none (record.PhaseRow.CompletedAt).
	completedAt time.Time
	state       supervise.ClaimState
	updateRound string
	// stepped is the moment the claim's agent last stepped as the record holds it, zero for the
	// silenceStep() every fact of these cases names: the moment the sweep read.
	stepped time.Time
	// holdsTask records a task of the issue's generation and phase on the claim, unconfirmed,
	// which is a task the claim still works (supervise.Delivery.StillWorked).
	holdsTask bool
	// queuedStart writes a start of the issue's phase into the outbox before the fact, the row
	// that is already on its way to the worker.
	queuedStart bool
}

// aSilentTester is the record of a tester nothing will prompt: an issue in testing at work, a
// claim up and between turns holding no task, in no update round, with no supervise row of its
// own, and a phase row naming the assignment that claim was last started under.
func aSilentTester() silenceRecord {
	return silenceRecord{phase: phase.Testing, generation: 1, assignment: silenceAssignment, state: supervise.StateReady}
}

// silenceToken is the claim token of silenceIssue's role under the test engine's project.
func silenceToken(t *testing.T, role claim.Role) claim.Token {
	t.Helper()
	project, err := claim.ProjectToken("LEGION")
	if err != nil {
		t.Fatal(err)
	}
	token, err := claim.NewToken(project, silenceIssue, role)
	if err != nil {
		t.Fatal(err)
	}
	return token
}

// seedSilence writes rec as the store holds it: the issue and its admission slot, the role's
// phase row with the completion it carries, the claim with the moment its agent last stepped,
// the task it holds and the start already queued for it.
func seedSilence(t *testing.T, pool *pgxpool.Pool, rec silenceRecord) record.Issue {
	t.Helper()
	issue := record.Issue{Key: silenceIssue, Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion",
		Tree: silenceIssue, Project: "LEGION", Title: "a worker that stopped taking turns", Phase: rec.phase,
		Generation: rec.generation, Status: "in_progress", Rank: "U"}
	if rec.lingers {
		until := silenceClock().Add(time.Hour)
		issue.LingerUntil = &until
	}
	if rec.unrecorded {
		return issue
	}
	seedIssue(t, pool, issue)
	seedSlot(t, pool, record.Slot{Issue: silenceIssue, Index: 0, AdmittedAt: silenceClock()})
	token := silenceToken(t, claim.RoleTester)
	seedPhase(t, pool, record.PhaseRow{Issue: silenceIssue, Role: claim.RoleTester, Claim: token,
		Assignment: rec.assignment, CompletedAt: rec.completedAt})
	stepped := rec.stepped
	if stepped.IsZero() {
		stepped = silenceStep()
	}
	if _, err := pool.Exec(t.Context(), `insert into claims (token, project, tree, issue, role, generation, session, session_file,
		state, update_round, step_at, launch_failures, prompt_failures, prompt_retires, uncertain_streak)
		values ($1, $2, $3, $3, 'tester', $4, 'ses', '/sessions/ses.jsonl', $5, $6, $7, 0, 0, 0, 0)`,
		string(token), "legion", silenceIssue, int64(rec.generation), string(rec.state), rec.updateRound, stepped); err != nil {
		t.Fatalf("seed the tester's claim of %s: %v", silenceIssue, err)
	}
	if rec.holdsTask {
		if _, err := pool.Exec(t.Context(), `insert into pending_task_deliveries (claim_token, delivery_id, task, queued_at, generation, phase)
			values ($1, 'outbox:7', 'Carry on.', $2, $3, $4)`,
			string(token), silenceClock(), int64(rec.generation), string(rec.phase)); err != nil {
			t.Fatalf("seed the task the tester of %s holds: %v", silenceIssue, err)
		}
	}
	if rec.queuedStart {
		seedRecord(t, pool, func(tx pgx.Tx) error {
			return StartOrOwe(t.Context(), tx, record.NewStore(), silenceIssue, record.SuperviseRequest{Op: "start",
				Tree: silenceIssue, Role: claim.RoleTester, Generation: rec.generation, Phase: rec.phase,
				Task: ResumePhaseTask(issue), Assignment: rec.assignment}, silenceClock())
		})
	}
	return issue
}

// silenceClock is the moment the test engine runs at (testEngine), and silenceStep the moment
// the silent claim last stepped: two hours before it, which is past the bound of every case.
func silenceClock() time.Time { return time.Date(2026, 9, 23, 0, 0, 0, 0, time.UTC) }

func silenceStep() time.Time { return silenceClock().Add(-2 * time.Hour) }

// applySilence applies one silence of role, as the sweep writes it: the claim token and the
// moment of the last step are the fact's identity.
func applySilence(t *testing.T, pool *pgxpool.Pool, engine *Engine, fact intake.WorkerSilent) error {
	t.Helper()
	_, err := intake.ApplyFact(t.Context(), pool, "supervise", silenceEventID(t, fact), fact, engine)
	return err
}

func silenceEventID(t *testing.T, fact intake.WorkerSilent) string {
	t.Helper()
	return fmt.Sprintf("worker-silent:%s:%s", silenceToken(t, fact.Role), fact.LastStep.UTC().Format(time.RFC3339Nano))
}

// aSilenceOf is the fact the sweep writes for role, with the bound it judged the claim by.
func aSilenceOf(role claim.Role, generation uint64) intake.WorkerSilent {
	return intake.WorkerSilent{Issue: silenceIssue, Role: role, Generation: generation, LastStep: silenceStep(), Bound: silenceBound}
}

// silenceSuperviseRow is one supervise row of the issue as the outbox holds it.
type silenceSuperviseRow struct {
	op         string
	role       claim.Role
	task       string
	stands     phase.Phase
	assignment string
}

// silenceSuperviseRows is every supervise row of silenceIssue, oldest first. It reads every
// operation and not starts alone, so a case that asserts the handler wrote no row is held to
// the whole table rather than to one kind of row in it.
func silenceSuperviseRows(t *testing.T, pool *pgxpool.Pool) []silenceSuperviseRow {
	t.Helper()
	rows, err := pool.Query(t.Context(), `select coalesce(payload->>'op', ''), coalesce(payload->>'role', ''),
		coalesce(payload->>'task', ''), coalesce(payload->>'phase', ''), coalesce(payload->>'assignment', '')
		from outbox where kind = 'supervise' and issue = $1 order by id`, silenceIssue)
	if err != nil {
		t.Fatalf("read the supervise rows of %s: %v", silenceIssue, err)
	}
	defer rows.Close()
	var read []silenceSuperviseRow
	for rows.Next() {
		var row silenceSuperviseRow
		if err := rows.Scan(&row.op, &row.role, &row.task, &row.stands, &row.assignment); err != nil {
			t.Fatalf("scan a supervise row of %s: %v", silenceIssue, err)
		}
		read = append(read, row)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate the supervise rows of %s: %v", silenceIssue, err)
	}
	return read
}

// namesTheSilence is every part of the record R2 asks the notice to name and that the case's
// own clause adds: the issue, the role, the phase, the moment of the last step and the length
// of the silence, then what the daemon wrote about it (workerSilentDid).
func namesTheSilence(t *testing.T, reason string, outcome string, absent string) {
	t.Helper()
	for _, part := range []string{silenceIssue, string(claim.RoleTester), string(phase.Testing),
		silenceStep().Format(time.RFC3339), "2h0m0s", outcome} {
		if !strings.Contains(reason, part) {
			t.Errorf("the notice's reason %q names no %q, which the architect needs to act on it", reason, part)
		}
	}
	if absent != "" && strings.Contains(reason, absent) {
		t.Errorf("the notice's reason %q says %q, which is not what the daemon did", reason, absent)
	}
}

// A silent phase worker whose phase row names an assignment is sent its phase's task again
// under that same assignment, and its architect is told what the daemon read and what it did.
// The task is ResumePhaseTask: the phase the worker is in is already under way, and no handoff
// begins it again.
func TestASilentWorkerIsSentItsPhasesTaskAgainUnderTheAssignmentItsRowNames(t *testing.T) {
	pool := migratedPool(t)
	issue := seedSilence(t, pool, aSilentTester())
	engine := testEngine(config.DesignGateRootIssues, nil)

	if err := applySilence(t, pool, engine, aSilenceOf(claim.RoleTester, 1)); err != nil {
		t.Fatalf("apply the silence: %v", err)
	}

	want := []silenceSuperviseRow{{op: "start", role: claim.RoleTester, task: ResumePhaseTask(issue),
		stands: phase.Testing, assignment: silenceAssignment}}
	if got := silenceSuperviseRows(t, pool); !reflect.DeepEqual(got, want) {
		t.Fatalf("the supervise rows of %s = %+v, want %+v: the worker comes back on one start row carrying the phase's resume task under the assignment its row already names",
			silenceIssue, got, want)
	}
	notices := noticesOfKind(t, pool, silenceIssue, workerSilentNotice)
	if len(notices) != 1 {
		t.Fatalf("the %s notices of %s = %d, want the one its architect is owed", workerSilentNotice, silenceIssue, len(notices))
	}
	if notices[0].Role != claim.RoleTester || notices[0].Phase != phase.Testing {
		t.Errorf("the notice names role %q and phase %q, want the tester of testing", notices[0].Role, notices[0].Phase)
	}
	namesTheSilence(t, notices[0].Reason, "wrote the start row that sends the phase's task again, under the assignment "+silenceAssignment, "")
	if rows := noticeRows(t, pool); len(rows) != 1 {
		t.Fatalf("the notice rows = %d, want the architect's one: the project's controller is told this fact on its own periodic wake", len(rows))
	}
}

// A silent phase worker whose phase row names no assignment is resumed just the same, as
// admission's own repair of a slotted worker resumes one: the completion fence compares only
// when both sides are set (api/handoff.go), and a re-admitted tree's members all have an empty
// one (record.ClearGeneration). The start carries no assignment and the notice names none,
// claiming nothing about an assignment the row does not hold.
func TestASilentWorkerWhosePhaseRowNamesNoAssignmentIsStartedUnderNone(t *testing.T) {
	pool := migratedPool(t)
	rec := aSilentTester()
	rec.assignment = ""
	issue := seedSilence(t, pool, rec)
	engine := testEngine(config.DesignGateRootIssues, nil)

	if err := applySilence(t, pool, engine, aSilenceOf(claim.RoleTester, 1)); err != nil {
		t.Fatalf("apply the silence: %v", err)
	}

	want := []silenceSuperviseRow{{op: "start", role: claim.RoleTester, task: ResumePhaseTask(issue), stands: phase.Testing}}
	if got := silenceSuperviseRows(t, pool); !reflect.DeepEqual(got, want) {
		t.Fatalf("the supervise rows of %s = %+v, want %+v: a row naming no assignment is resumed under none, as admission resumes one",
			silenceIssue, got, want)
	}
	notices := noticesOfKind(t, pool, silenceIssue, workerSilentNotice)
	if len(notices) != 1 {
		t.Fatalf("the %s notices of %s = %d, want the one its architect is owed", workerSilentNotice, silenceIssue, len(notices))
	}
	namesTheSilence(t, notices[0].Reason, "wrote the start row that sends the phase's task again", "under the assignment")
}

// Every reading that says there is nothing to do leaves the record exactly as it stood: no
// supervise row of the handler's and no notice at all. Each row changes one term of the told
// case above, and together they are R6 of dispatch://INFRA-3163, the workers the daemon must
// not prompt.
func TestASilenceTheRecordAnswersForChangesNothing(t *testing.T) {
	for _, tc := range []struct {
		name string
		rec  silenceRecord
		fact func(silenceRecord) intake.WorkerSilent
	}{
		{name: "the record holds no such issue", rec: func() silenceRecord {
			rec := aSilentTester()
			rec.unrecorded = true
			return rec
		}()},
		{name: "the issue's generation has moved past the fact's", rec: func() silenceRecord {
			rec := aSilentTester()
			rec.generation = 2
			return rec
		}(), fact: func(silenceRecord) intake.WorkerSilent { return aSilenceOf(claim.RoleTester, 1) }},
		{name: "the claim is working", rec: func() silenceRecord {
			rec := aSilentTester()
			rec.state = supervise.StateWorking
			return rec
		}()},
		{name: "the claim holds a task it still works", rec: func() silenceRecord {
			rec := aSilentTester()
			rec.holdsTask = true
			return rec
		}()},
		// The claim took a turn between the sweep's read and this apply — an Envoy message, a
		// person's steer, a background result — and is back to ready with a step of its own.
		// The step is still older than the bound, so the rule answers true and the handler's
		// own reading of the fact's moment against the record's is what ends it.
		{name: "the claim stepped after the moment the fact names", rec: func() silenceRecord {
			rec := aSilentTester()
			rec.stepped = silenceClock().Add(-time.Hour)
			return rec
		}()},
		// A completion the workflow accepted that moved nothing: an implementing completion
		// with no pull request, or a reviewing round no review has decided. That worker
		// finished its phase entry and is owed no turn.
		{name: "the role's completion of this phase entry was accepted", rec: func() silenceRecord {
			rec := aSilentTester()
			rec.completedAt = silenceClock().Add(-time.Hour)
			return rec
		}()},
		{name: "a start for the phase is already queued", rec: func() silenceRecord {
			rec := aSilentTester()
			rec.queuedStart = true
			return rec
		}()},
		{name: "the claim is in an update round", rec: func() silenceRecord {
			rec := aSilentTester()
			rec.updateRound = "round-1"
			return rec
		}()},
		{name: "the issue is accepting", rec: func() silenceRecord {
			rec := aSilentTester()
			rec.phase = phase.Accepting
			return rec
		}()},
		{name: "the issue awaits its merge", rec: func() silenceRecord {
			rec := aSilentTester()
			rec.phase = phase.AwaitingMerge
			return rec
		}()},
		{name: "the role does not work the issue's phase", rec: func() silenceRecord {
			rec := aSilentTester()
			rec.phase = phase.Implementing
			return rec
		}()},
		{name: "the claim is suspended", rec: func() silenceRecord {
			rec := aSilentTester()
			rec.state = supervise.StateSuspended
			return rec
		}()},
		{name: "the claim has failed", rec: func() silenceRecord {
			rec := aSilentTester()
			rec.state = supervise.StateFailed
			return rec
		}()},
		{name: "the claim is retired", rec: func() silenceRecord {
			rec := aSilentTester()
			rec.state = supervise.StateRetired
			return rec
		}()},
		{name: "the tree lingers", rec: func() silenceRecord {
			rec := aSilentTester()
			rec.lingers = true
			return rec
		}()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := migratedPool(t)
			seedSilence(t, pool, tc.rec)
			engine := testEngine(config.DesignGateRootIssues, nil)
			before := silenceSuperviseRows(t, pool)
			fact := aSilenceOf(claim.RoleTester, tc.rec.generation)
			if tc.fact != nil {
				fact = tc.fact(tc.rec)
			}

			if err := applySilence(t, pool, engine, fact); err != nil {
				t.Fatalf("apply the silence: %v", err)
			}

			if got := silenceSuperviseRows(t, pool); !reflect.DeepEqual(got, before) {
				t.Errorf("the supervise rows of %s = %+v, want the %+v the fixture left: this reading says there is nothing to do",
					silenceIssue, got, before)
			}
			if rows := noticeRows(t, pool); len(rows) != 0 {
				t.Errorf("the notice rows = %+v, want none: a reading that shows the worker needs nothing tells nobody", rows)
			}
		})
	}
}

// refusedStart is the error the staged store answers the repair's own write with.
var refusedStart = errors.New("the store refused the start row")

// refusingStore is the record store with one write refused: the enqueue of the supervise start
// row the repair writes. Every other call is the real store's, so what the handler reads is the
// record the fixture seeded.
type refusingStore struct {
	record.Store
}

func (s refusingStore) Enqueue(ctx context.Context, tx pgx.Tx, row record.OutboxRow) error {
	if row.Kind == record.OutboxKindSupervise {
		return refusedStart
	}
	return s.Store.Enqueue(ctx, tx, row)
}

// A repair the store refuses writes nothing at all, and the fact is not recorded as applied.
// The notice, the start row and the mark that the fact was applied are one transaction
// (intake.ApplyFact), so the handler returns the error rather than writing a notice over it:
// nothing commits, and the next sweep's identical fact runs the handler again, which is the
// retry. A notice written in its place would be the last word on this silence for ever, the
// fact's identity being the claim token and a last step that does not move.
func TestAStartRowTheStoreRefusesWritesNoNoticeAndLeavesTheFactUnapplied(t *testing.T) {
	pool := migratedPool(t)
	seedSilence(t, pool, aSilentTester())
	engine := testEngine(config.DesignGateRootIssues, nil)
	engine.store = refusingStore{Store: record.NewStore()}
	fact := aSilenceOf(claim.RoleTester, 1)

	err := applySilence(t, pool, engine, fact)

	if !errors.Is(err, refusedStart) {
		t.Fatalf("the apply of a silence whose start row the store refuses = %v, want the store's own error", err)
	}
	if rows := noticeRows(t, pool); len(rows) != 0 {
		t.Errorf("the notice rows = %+v, want none: a notice must never commit while the repair it reports failed", rows)
	}
	if got := silenceSuperviseRows(t, pool); len(got) != 0 {
		t.Errorf("the supervise rows of %s = %+v, want none", silenceIssue, got)
	}
	applied, err := intake.Applied(context.Background(), pool, "supervise", []string{silenceEventID(t, fact)})
	if err != nil {
		t.Fatalf("read which facts are applied: %v", err)
	}
	if applied[silenceEventID(t, fact)] {
		t.Errorf("the fact is recorded as applied, so the next sweep's identical fact would be skipped and this silence would never be repaired")
	}
}
