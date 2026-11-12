package workflow

import (
	"context"
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
)

// entryIssue is the issue every case of this file stands on.
const entryIssue = "LEGION-208"

// seededEntry is the entry a fixture's phases row carries before the fact under test, raised by
// a transition of the run before it. It is above 1 so that a raise that wrote the number rather
// than raising it would read as a failure here.
const seededEntry = 4

// seededEntryTask is the work that seeded entry names, the text the transition that raised it
// sent its worker.
const seededEntryTask = "Continue root. Issue: LEGION-208. Phase: testing."

// seededRaisedAt is when that seeded entry was raised, before the clock every test engine of
// this package reads (testEngine, readyEngine), so a field left alone is told from one written
// again.
var seededRaisedAt = time.Date(2026, 9, 20, 0, 0, 0, 0, time.UTC)

// Every start that hands a role work it was not on raises that role's phase entry by one, writes
// the work of that entry and the moment it was raised on the role's phases row, and carries the
// number on the start's payload, all in the transition's own transaction. The issue's run cannot
// do this work: one run holds every phase entry, so the testing phase the tester is given after a
// red and an implementing round has to be told from the testing phase before it by something the
// run does not carry (api/handoff.go).
//
// Entering one phase twice is the case that proves the number is raised rather than set: the
// implementer is started in implementing, completes, and is started there again, which leaves its
// row at the second entry carrying the second entry's task.
func TestEveryPhaseStartRaisesTheRolesEntryAndWritesItsTask(t *testing.T) {
	pool := migratedPool(t)
	ctx := context.Background()
	seedIssue(t, pool, record.Issue{Key: entryIssue, Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion", Tree: entryIssue,
		Project: "LEGION", Title: "root", Phase: phase.Testing, Generation: 1, Status: "testing", Rank: "U"})
	seedPR(t, pool, record.PullRequest{State: record.PullRequestOpen, Issue: entryIssue, Repo: "sjawhar/legion", Number: 42,
		Branch: "legion/" + entryIssue, HeadSHA: "head", CheckedHead: "head", Failing: []string{}, Required: []string{"pytest"}})
	seedPhase(t, pool, record.PhaseRow{Issue: entryIssue, Role: claim.RoleImplementer, Claim: "implement-claim"})
	engine := testEngine(config.DesignGateRootIssues, nil)
	apply := func(eventID string, fact intake.Fact) {
		t.Helper()
		result, err := intake.ApplyFact(ctx, pool, "fact", eventID, fact, engine, admissionStub{})
		if err != nil {
			t.Fatalf("ApplyFact %s: %v", eventID, err)
		}
		if result.Refusal != nil {
			t.Fatalf("ApplyFact %s refused: %+v", eventID, *result.Refusal)
		}
	}
	red := func(eventID, head string) {
		t.Helper()
		apply(eventID, intake.PullRequestChecks{Repo: "sjawhar/legion", Number: 42, HeadSHA: head,
			CheckRuns: []record.AttemptRun{{Name: "pytest", ID: 7}}, Generation: 1, Snapshot: eventID, Failing: []string{"pytest"}})
	}

	// The red ends testing and starts the implementer, which is the first start of this test and
	// the first entry of that role's record.
	red("red-1", "head")
	assertPhase(t, pool, phase.Implementing)
	if got := entryOfStart(t, pool, claim.RoleImplementer, engine.now()); got != 1 {
		t.Fatalf("the implementer's first start is of entry %d, want 1: the role had been handed no work on this issue", got)
	}
	firstTask := entryRow(t, pool, claim.RoleImplementer).EntryTask

	// The implementer's completion sends the issue to testing and starts the tester, whose own
	// record counts from its own first entry.
	apply("implement-1", intake.HandoffComplete{Generation: 1, Issue: entryIssue, Role: claim.RoleImplementer, Claim: "implement-claim", Commit: "fix-1",
		HandoffRecord: "implement-1", HandoffVersion: 1})
	assertPhase(t, pool, phase.Testing)
	if got := entryOfStart(t, pool, claim.RoleTester, engine.now()); got != 1 {
		t.Fatalf("the tester's first start is of entry %d, want 1: each role counts the entries it is handed", got)
	}
	// The implementer's own entry is its row's still: a completion keeps it, and only its next
	// start raises it.
	if row := entryRow(t, pool, claim.RoleImplementer); row.Entry != 1 || row.EntryTask != firstTask {
		t.Fatalf("after its completion the implementer's row is at entry %d carrying %q, want entry 1 carrying the %q its start wrote",
			row.Entry, row.EntryTask, firstTask)
	}

	// The tester fails this round and the issue goes back to implementing, the second entry of
	// that role: the number is raised again and the row carries the work of this entry, not of
	// the one before it.
	apply("test-1", intake.HandoffComplete{Generation: 1, Issue: entryIssue, Role: claim.RoleTester, Claim: "test-claim", Verdict: "fail",
		HandoffRecord: "test-1", HandoffVersion: 1})
	assertPhase(t, pool, phase.Implementing)
	if got := entryOfStart(t, pool, claim.RoleImplementer, engine.now()); got != 2 {
		t.Fatalf("the implementer's second start is of entry %d, want 2: nothing else tells the two implementing phases apart", got)
	}
	secondTask := entryRow(t, pool, claim.RoleImplementer).EntryTask
	if secondTask == firstTask {
		t.Fatalf("the implementer's row carries %q after its second start, want the second entry's own task: the first was %q", secondTask, firstTask)
	}
	apply("implement-2", intake.HandoffComplete{Generation: 1, Issue: entryIssue, Role: claim.RoleImplementer, Claim: "implement-claim", Commit: "fix-2",
		HandoffRecord: "implement-2", HandoffVersion: 2})
	assertPhase(t, pool, phase.Testing)
	if got := entryOfStart(t, pool, claim.RoleTester, engine.now()); got != 2 {
		t.Fatalf("the tester's second start is of entry %d, want 2: nothing else tells the two testing phases apart", got)
	}

	// The issue never left run 1, which is why the run alone cannot tell these two testing phases
	// apart.
	var generation uint64
	if err := pool.QueryRow(ctx, "select generation from issues where key = $1", entryIssue).Scan(&generation); err != nil {
		t.Fatalf("read the issue's run: %v", err)
	}
	if generation != 1 {
		t.Fatalf("the issue is on run %d, want 1: the test only means something while the run stands still", generation)
	}
}

// A resume hands no new task: it is the task the worker already holds, said again for a session
// that may have forgotten it (ResumePhaseTask). It raises no entry, so the row keeps the number,
// the work and the moment its phase's own start wrote, and the resumed worker's in-flight
// completion is never refused.
func TestTheBootsResumeRaisesNoEntry(t *testing.T) {
	pool := migratedPool(t)
	ctx := t.Context()
	project, err := claim.ProjectToken("LEGION")
	if err != nil {
		t.Fatal(err)
	}
	seedIssue(t, pool, record.Issue{Key: entryIssue, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Tree: entryIssue,
		Project: "LEGION", Title: "root", Phase: phase.Implementing, Generation: 1, Status: "in_progress", Rank: "U", HandedOver: true})
	seedSlot(t, pool, record.Slot{Issue: entryIssue, Index: 0})
	seedEntryRow(t, pool, claim.RoleImplementer, "implement-claim")
	token, err := claim.NewToken(project, entryIssue, claim.RoleImplementer)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `insert into claims (token, project, tree, issue, role, generation, session, session_file,
		state, launch_failures, prompt_failures, prompt_retires, uncertain_streak, suspended_by)
		values ($1, $2, 'LEGION-208', 'LEGION-208', 'implementer', 1, 'ses', '/sessions/ses.jsonl', 'suspended', 0, 0, 0, 0, 'daemon')`,
		string(token), project); err != nil {
		t.Fatalf("seed the suspended implementer: %v", err)
	}

	if _, err := intake.ApplyFact(ctx, pool, "daemon", "boot:LEGION:1", intake.DaemonBooted{Boot: 1},
		testEngine(config.DesignGateRootIssues, nil), admissionStub{}); err != nil {
		t.Fatalf("apply the boot: %v", err)
	}

	starts := superviseRowsOf(t, pool, entryIssue, "start")
	if len(starts) != 1 || !starts[0].ResumeTask {
		t.Fatalf("the boot queued %+v, want the one resume start", starts)
	}
	if starts[0].Entry != 0 {
		t.Fatalf("the resume start is of entry %d, want none: it hands the worker no new task", starts[0].Entry)
	}
	assertEntryUntouched(t, pool, claim.RoleImplementer, "the resume")
}

// A held phase's retry puts the issue back in the phase its worker's claim failed in, with the
// work that phase entry already gave it. It raises no entry and writes no task, so the record
// still names the entry the worker is on and the start carries that number: a completion still in
// flight is never refused for an entry nothing ever delivered.
func TestAHeldPhasesRetryRaisesNoEntry(t *testing.T) {
	pool := migratedPool(t)
	seedIssue(t, pool, record.Issue{Key: entryIssue, Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion", Tree: entryIssue,
		Project: "LEGION", Title: "root", Phase: phase.Held, Hold: &record.Hold{From: phase.Testing}, Generation: 1,
		Status: "testing", Rank: "U", HandedOver: true})
	seedTreeLifecycle(t, pool, entryIssue)
	seedEntryRow(t, pool, claim.RoleTester, "test-claim")
	engine := testEngine(config.DesignGateRootIssues, nil)

	applyRefusingNothing(t, pool, engine, intake.RetryOrEscalate{Issue: entryIssue, Decision: intake.RetryDecision})

	assertPhase(t, pool, phase.Testing)
	starts := superviseRowsOf(t, pool, entryIssue, "start")
	if len(starts) != 1 {
		t.Fatalf("the retry queued %+v, want the one start it writes", starts)
	}
	if starts[0].Entry != seededEntry {
		t.Fatalf("the retry's start is of entry %d, want the %d its worker is on: the retry hands back the work it has", starts[0].Entry, seededEntry)
	}
	assertEntryUntouched(t, pool, claim.RoleTester, "the retry")
}

// A production check's look starts the role that works it after the close, on work that is no
// phase: the issue has no phase to hand out, the report is held to the check and its look
// instead, and the start raises no entry. So that role's record goes on naming whatever entry its
// last phase gave it, and nothing of the check is weighed against a phases row.
func TestAProductionChecksLookRaisesNoEntry(t *testing.T) {
	rig := childCheckRig(t)
	starts := superviseRowsOf(t, rig.pool, "LEGION-209", "start")
	if len(starts) == 0 {
		t.Fatal("no start for the check's first look")
	}
	look := starts[len(starts)-1]
	if look.CheckLook != 1 {
		t.Fatalf("the newest start is %+v, want the check's look 1", look)
	}
	if look.Entry != 0 {
		t.Fatalf("the look's start is of entry %d, want none: a look is no phase", look.Entry)
	}
}

// A re-admitted tree's promotion hands its mid-phase member's role the work of its phase again,
// so it raises that role's entry and writes the resume task as the work of the new entry. The
// re-admission cleared the task the earlier entry named with the rest of the generation
// (record.Store.ClearTreeGeneration), so the record would otherwise name an entry whose work
// nothing could send again.
func TestThePromotionsStartRaisesTheEntryAndWritesItsTask(t *testing.T) {
	pool := migratedPool(t)
	issue := record.Issue{Key: entryIssue, Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion", Tree: entryIssue,
		Project: "LEGION", Title: "root", Phase: phase.Implementing, Generation: 2, Status: "in_progress", Rank: "U"}
	seedIssue(t, pool, issue)
	seedTreeLifecycle(t, pool, entryIssue)
	// The row a re-admission leaves: the entry where it stood, with no task of its own.
	seedPhase(t, pool, record.PhaseRow{Issue: entryIssue, Role: claim.RoleImplementer, Claim: "implement-claim",
		Entry: seededEntry, EntryRaisedAt: seededRaisedAt})
	engine := testEngine(config.DesignGateRootIssues, nil)

	seedRecord(t, pool, func(tx pgx.Tx) error {
		return engine.StartPromotedWorker(t.Context(), tx, issue, claim.RoleImplementer)
	})

	starts := superviseRowsOf(t, pool, entryIssue, "start")
	if len(starts) != 1 {
		t.Fatalf("the promotion queued %+v, want the one start it writes", starts)
	}
	start := starts[0]
	want := ResumePhaseTask(issue)
	if start.Entry != seededEntry+1 || start.Task != want || !start.ResumeTask {
		t.Fatalf("the promotion's start = %+v, want entry %d carrying %q, marked as the phase's resume task: the promotion hands the role its work again",
			start, seededEntry+1, want)
	}
	row := entryRow(t, pool, claim.RoleImplementer)
	if row.Entry != seededEntry+1 || row.EntryTask != want || !row.EntryRaisedAt.Equal(engine.now()) {
		t.Fatalf("the implementer's row after the promotion = entry %d, task %q, raised at %s; want entry %d, the task the start carries, raised at %s",
			row.Entry, row.EntryTask, row.EntryRaisedAt, seededEntry+1, engine.now())
	}
}

// A claim behind its role's entry is sent the work that entry names, at that entry and with no
// entry raised: the task the record holds for it where it holds one, and the phase's resume task
// where it holds none, which is the row the upgrade to the entry left and the row a re-admission
// reset. The merger reads both halves, because its task carries the head its review round
// approved and that head may be named exactly once: the recorded task already carries it, since
// the transition wrote the text it sent, and the resume task appends it itself.
func TestTheWorkAnEntryNamesIsSentOnce(t *testing.T) {
	const head = "approved-head"
	issue := record.Issue{Key: entryIssue, Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion", Tree: entryIssue,
		Project: "LEGION", Title: "root", Phase: phase.Merging, Generation: 1, Status: "in_progress", Rank: "U"}
	recorded := ResumePhaseTask(issue) + " " + approvedHeadLabel + ": " + head + "."
	for _, tc := range []struct {
		name string
		// held is the work the merger's phases row names, "" for a row that names none.
		held string
		want string
	}{
		{name: "a row that names the work of its entry", held: recorded, want: recorded},
		{name: "a row an upgrade or a re-admission left with none", want: recorded},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := migratedPool(t)
			seedIssue(t, pool, issue)
			seedTreeLifecycle(t, pool, entryIssue)
			seedPhase(t, pool, record.PhaseRow{Issue: entryIssue, Role: claim.RoleReviewer, Claim: "review-claim",
				Decision: &record.ReviewDecision{State: "approved", Head: head}})
			merger := record.PhaseRow{Issue: entryIssue, Role: claim.RoleMerger, Claim: "merge-claim",
				Entry: seededEntry, EntryTask: tc.held, EntryRaisedAt: seededRaisedAt}
			seedPhase(t, pool, merger)
			engine := testEngine(config.DesignGateRootIssues, nil)

			seedRecord(t, pool, func(tx pgx.Tx) error {
				return engine.SendOwedEntry(t.Context(), tx, issue, claim.RoleMerger, merger)
			})

			starts := superviseRowsOf(t, pool, entryIssue, "start")
			if len(starts) != 1 {
				t.Fatalf("the send queued %+v, want the one start it writes", starts)
			}
			start := starts[0]
			if start.Entry != seededEntry || start.Task != tc.want {
				t.Fatalf("the start sent = entry %d carrying %q, want entry %d carrying %q: a claim behind the entry is owed that entry's own work",
					start.Entry, start.Task, seededEntry, tc.want)
			}
			if got := strings.Count(start.Task, approvedHeadLabel); got != 1 {
				t.Fatalf("the task names %q %d times, want once: the recorded task already carries the head, and the resume task appends it itself",
					approvedHeadLabel, got)
			}
			row := entryRow(t, pool, claim.RoleMerger)
			if row.Entry != seededEntry || row.EntryTask != tc.held || !row.EntryRaisedAt.Equal(seededRaisedAt) {
				t.Fatalf("after the send the merger's row = entry %d, task %q, raised at %s; want the entry %d, task %q and moment %s it stood at: the send raises nothing",
					row.Entry, row.EntryTask, row.EntryRaisedAt, seededEntry, tc.held, seededRaisedAt)
			}
		})
	}
}

// seedEntryRow records role's phases row as a transition of the run before the fact under test
// left it: on the seeded entry, carrying that entry's work and the moment it was raised.
func seedEntryRow(t *testing.T, pool *pgxpool.Pool, role claim.Role, held claim.Token) {
	t.Helper()
	seedPhase(t, pool, record.PhaseRow{Issue: entryIssue, Role: role, Claim: held, Entry: seededEntry,
		EntryTask: seededEntryTask, EntryRaisedAt: seededRaisedAt})
}

// assertEntryUntouched is the phases row seedEntryRow wrote, unchanged: a start that hands the
// role no work it was not on leaves the number, the work and the moment alike where they stood.
func assertEntryUntouched(t *testing.T, pool *pgxpool.Pool, role claim.Role, what string) {
	t.Helper()
	row := entryRow(t, pool, role)
	if row.Entry != seededEntry || row.EntryTask != seededEntryTask || !row.EntryRaisedAt.Equal(seededRaisedAt) {
		t.Fatalf("after %s the %s's row = entry %d, task %q, raised at %s; want the entry %d, task %q and moment %s it stood at",
			what, role, row.Entry, row.EntryTask, row.EntryRaisedAt, seededEntry, seededEntryTask, seededRaisedAt)
	}
}

// entryOfStart is the entry the start the issue owes its worker is of, which must be role's and
// must be the entry its phases row names, with that row carrying the task the start carries and
// the moment the engine raised it. One pending start exists at a time: a transition writes its
// start over the one the issue already owed (Store.EnqueueStart), so what this reads is always
// the newest one, and a phase the issue has left leaves none of its own behind.
func entryOfStart(t *testing.T, pool *pgxpool.Pool, role claim.Role, raised time.Time) int64 {
	t.Helper()
	starts := superviseRowsOf(t, pool, entryIssue, "start")
	if len(starts) != 1 {
		t.Fatalf("the outbox holds %d pending starts, want the one the issue owes its worker: %+v", len(starts), starts)
	}
	start := starts[0]
	if start.Role != role {
		t.Fatalf("the pending start is the %s's, want the %s's: %+v", start.Role, role, start)
	}
	if start.Entry == 0 {
		t.Fatalf("the %s's start is of no entry: %+v", role, start)
	}
	row := entryRow(t, pool, role)
	if row.Entry != start.Entry || row.EntryTask != start.Task {
		t.Fatalf("the %s's start is of entry %d carrying %q while its row is at entry %d carrying %q; the two are written in one transaction and must agree",
			role, start.Entry, start.Task, row.Entry, row.EntryTask)
	}
	if !row.EntryRaisedAt.Equal(raised) {
		t.Fatalf("the %s's row says its entry was raised at %s, want the %s the transition read from its own clock", role, row.EntryRaisedAt, raised)
	}
	return start.Entry
}

// entryRow is role's phases row on the issue.
func entryRow(t *testing.T, pool *pgxpool.Pool, role claim.Role) record.PhaseRow {
	t.Helper()
	var row record.PhaseRow
	seedRecord(t, pool, func(tx pgx.Tx) error {
		rows, err := record.NewStore().Phases(t.Context(), tx, entryIssue)
		if err != nil {
			return err
		}
		for _, held := range rows {
			if held.Role == role {
				row = held
				return nil
			}
		}
		t.Fatalf("the issue holds no %s phases row", role)
		return nil
	})
	return row
}
