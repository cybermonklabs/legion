package workflow

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/pods"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
)

// seededArchitectTask is the work the architect's seeded entry names: the text the transition
// that raised that entry sent it.
const seededArchitectTask = "LEGION-208 is yours. Take it up in this turn."

// architectEntryRoot is the root these cases stand on: an admitted root of a tree of its own,
// whose architect holds a phases row as every phase worker does.
func architectEntryRoot() record.Issue {
	return record.Issue{Key: entryIssue, Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion",
		Tree: entryIssue, Project: "LEGION", Title: "root", Phase: phase.Admitted, Generation: 1,
		Status: "in_progress", Rank: "U", HandedOver: true, LastDispatchSeq: 1}
}

// seedArchitectEntryRow records the architect's phases row as the start of an earlier entry left
// it: on the seeded entry, carrying that entry's work and the moment it was raised.
func seedArchitectEntryRow(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	seedPhase(t, pool, record.PhaseRow{Issue: entryIssue, Role: claim.RoleArchitect, Claim: wakeToken(t, claim.RoleArchitect),
		Entry: seededEntry, EntryTask: seededArchitectTask, EntryRaisedAt: seededRaisedAt})
}

// assertArchitectEntryUntouched is the row seedArchitectEntryRow wrote, unchanged: a delivery
// that hands the architect no work of its own leaves the number, the work and the moment alike
// where they stood.
func assertArchitectEntryUntouched(t *testing.T, pool *pgxpool.Pool, what string) {
	t.Helper()
	row := entryRow(t, pool, claim.RoleArchitect)
	if row.Entry != seededEntry || row.EntryTask != seededArchitectTask || !row.EntryRaisedAt.Equal(seededRaisedAt) {
		t.Fatalf("after %s the architect's row = entry %d, task %q, raised at %s; want the entry %d, task %q and moment %s it stood at",
			what, row.Entry, row.EntryTask, row.EntryRaisedAt, seededEntry, seededArchitectTask, seededRaisedAt)
	}
}

// architectStartOf is the one architect start the issue's outbox holds.
func architectStartOf(t *testing.T, pool *pgxpool.Pool) record.SuperviseRequest {
	t.Helper()
	var starts []record.SuperviseRequest
	for _, start := range superviseRowsOf(t, pool, entryIssue, "start") {
		if start.Role == claim.RoleArchitect {
			starts = append(starts, start)
		}
	}
	if len(starts) != 1 {
		t.Fatalf("the outbox holds %d architect starts, want the one the transition wrote: %+v", len(starts), starts)
	}
	return starts[0]
}

// A root's architect holds a row of the phases record carrying the entry its work stands at,
// that entry's task and the moment it was raised, exactly as every phase worker does
// (record.PhaseRow.Entry, EntryTask, EntryRaisedAt). EnterArchitect is the one way that entry is
// raised, and it raises the number rather than writing one: a tree whose architect has been
// handed work before counts from where it stood, which is what tells the work of one entry from
// the work of the next.
//
// Each raise writes the wait a resume's entry answers (record.PhaseRow.EntryAwaiting), so the wait
// a resume's entry kept goes with that entry: a raise that names none clears it.
//
// The row is the architect's own: the entry of a phase worker of the same issue stands where it
// was, because the record counts the entries each role has been handed.
func TestTheRootArchitectEntryIsRaisedWithItsTaskAndTheMomentItWasRaised(t *testing.T) {
	t.Parallel()
	pool := migratedPool(t)
	root := architectEntryRoot()
	seedIssue(t, pool, root)
	seedArchitectEntryRow(t, pool)
	seedEntryRow(t, pool, claim.RoleImplementer, "implement-claim")
	engine := testEngine(config.DesignGateRootIssues, nil)

	const first = "A person answered your ask ask-1 on LEGION-208."
	waiting := record.Awaiting{Ask: "ask-1", Since: record.EventMark{Seq: 5}, AnsweredBy: record.AnsweredByAsk, AnsweredDetail: "ask-1"}
	if entry := enterArchitect(t, pool, engine, root, first, &waiting); entry != seededEntry+1 {
		t.Fatalf("the architect's entry after the first raise = %d, want %d: the number is raised, never written", entry, seededEntry+1)
	}
	row := entryRow(t, pool, claim.RoleArchitect)
	if row.Entry != seededEntry+1 || row.EntryTask != first || !sameWait(row.EntryAwaiting, waiting) || !row.EntryRaisedAt.Equal(engine.now()) {
		t.Fatalf("the architect's row = entry %d, task %q, wait %+v, raised at %s; want entry %d carrying %q and the wait %+v, raised at the %s the engine read from its own clock",
			row.Entry, row.EntryTask, row.EntryAwaiting, row.EntryRaisedAt, seededEntry+1, first, waiting, engine.now())
	}

	second := ArchitectStartTask(ArchitectStart{Root: root})
	if entry := enterArchitect(t, pool, engine, root, second, nil); entry != seededEntry+2 {
		t.Fatalf("the architect's entry after the second raise = %d, want %d: each entry counts from the one before it", entry, seededEntry+2)
	}
	if row := entryRow(t, pool, claim.RoleArchitect); row.Entry != seededEntry+2 || row.EntryTask != second || row.EntryAwaiting != nil {
		t.Fatalf("the architect's row after the second raise = entry %d carrying %q, wait %+v; want entry %d carrying the work of that entry, "+
			"derived again: the first entry's wait went with it", row.Entry, row.EntryTask, row.EntryAwaiting, seededEntry+2)
	}
	assertEntryUntouched(t, pool, claim.RoleImplementer, "the architect's two entries")
}

// enterArchitect raises the architect's entry for task, keeping the wait awaiting where a resume's
// raise carries one, through the one exported door onto it, in a transaction of its own as a
// transition's own transaction does.
func enterArchitect(t *testing.T, pool *pgxpool.Pool, engine *Engine, root record.Issue, task string, awaiting *record.Awaiting) int64 {
	t.Helper()
	var entry int64
	seedRecord(t, pool, func(tx pgx.Tx) error {
		var err error
		entry, err = engine.EnterArchitect(t.Context(), tx, root, task, awaiting)
		return err
	})
	return entry
}

// What a root's architect is owed is read off the record exactly as a phase worker's is: the
// entry its phases row names against the entry its claim was last queued a task of
// (record.RoleClaim.GivenEntry), with a queued start that already carries the entry counting as
// that entry's work on its way (OwedEntry). A role the daemon holds no claim row of was given
// nothing, which is the architect of a tree whose first start has not run.
func TestOwedEntryReadsARootArchitectEntryAsItReadsAPhaseWorkers(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		// claimed is whether the daemon holds an architect claim at all, and given the entry it
		// was last queued a task of.
		claimed bool
		given   int64
		// queued is whether the entry's own start is in the outbox, carrying that entry.
		queued bool
		owed   bool
	}{
		{name: "a claim given the entry below it", claimed: true, given: seededEntry - 1, owed: true},
		{name: "a claim given that entry's own work", claimed: true, given: seededEntry},
		{name: "no claim at all, which was given nothing", owed: true},
		{name: "the entry's own start queued, which carries that work", claimed: true, given: seededEntry - 1, queued: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := migratedPool(t)
			root := architectEntryRoot()
			seedIssue(t, pool, root)
			seedArchitectEntryRow(t, pool)
			token := wakeToken(t, claim.RoleArchitect)
			if tc.claimed {
				seedArchitectClaimGiven(t, pool, root, tc.given)
			}
			if tc.queued {
				seedRecord(t, pool, func(tx pgx.Tx) error {
					row, err := record.NewOutboxRow(root.Key, record.SuperviseRequest{Op: "start", Tree: root.Tree,
						Role: claim.RoleArchitect, Generation: root.Generation, Task: seededArchitectTask, Entry: seededEntry},
						time.Now().UTC())
					if err != nil {
						return err
					}
					return record.NewStore().Enqueue(t.Context(), tx, row)
				})
			}

			seedRecord(t, pool, func(tx pgx.Tx) error {
				runs, err := record.NewStore().RoleRuns(t.Context(), tx, []record.RoleRunKey{
					{Token: token, Issue: root.Key, Role: claim.RoleArchitect, Generation: root.Generation}})
				if err != nil {
					return err
				}
				run := runs[token]
				row := entryRow(t, pool, claim.RoleArchitect)
				if owed := OwedEntry(run, root, row.Entry); owed != tc.owed {
					t.Fatalf("the architect is owed = %t on the entry %d its row names, with claim %+v and %d queued rows; want owed = %t",
						owed, row.Entry, run.Claim, len(run.Queued), tc.owed)
				}
				return nil
			})
		})
	}
}

// seedArchitectClaimGiven records root's architect claim, ready, as the entry given was last
// queued a task of (supervise.Claim.GivenEntry, and ServedEntry for the turn that took it).
func seedArchitectClaimGiven(t *testing.T, pool *pgxpool.Pool, root record.Issue, given int64) {
	t.Helper()
	project, err := claim.ProjectToken(root.Project)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(context.Background(), `insert into claims (token, project, tree, issue, role, generation, session, session_file,
		state, launch_failures, prompt_failures, prompt_retires, uncertain_streak, given_entry, served_entry, serving_generation)
		values ($1, $2, $3, $4, 'architect', $5, 'session-architect', '', 'ready', 0, 0, 0, 0, $6, $6, $5)`,
		string(wakeToken(t, claim.RoleArchitect)), project, root.Tree, root.Key, int64(root.Generation), given); err != nil {
		t.Fatalf("seed the architect's claim: %v", err)
	}
}

// The resume of a root that waited on a person hands its architect work it was not on, so it
// raises the architect's entry, writes the resume task as the work of that entry and carries the
// number on the start it writes (ResumeAwaitedRoot). The start carries the wait it answers and no
// text, because its task is rendered at its send from the record as it stands then; the row keeps
// that same wait beside the text rendered at the resume, because the sweep's send of a lost start
// builds the resume's start again from it once the root's own wait is gone (SendArchitectStart).
// Both of its callers get it: the promotion of an answered root, which calls it as this case does
// (internal/admit's record.LineAnswered branch), and the reconcile that finds a person already
// acted after the hand-back, which tells the live architect where it stands (tellInPlace). A row
// lost at tellInPlace would lose a person's answer, which is what the record is for.
func TestResumeAwaitedRootRaisesTheArchitectEntryForBothOfItsCallers(t *testing.T) {
	t.Parallel()
	answeredAt := time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)
	t.Run("the promotion of an answered root", func(t *testing.T) {
		pool := migratedPool(t)
		root := architectEntryRoot()
		seedIssue(t, pool, root)
		seedArchitectEntryRow(t, pool)
		engine := testEngine(config.DesignGateRootIssues, nil)
		waiting := record.Awaiting{Ask: "ask-1", Since: record.EventMark{Seq: 5}, AnsweredAt: &answeredAt,
			AnsweredBy: record.AnsweredByAsk}

		if err := store.Transact(context.Background(), pool, func(ctx context.Context, tx pgx.Tx) error {
			return engine.ResumeAwaitedRoot(ctx, tx, root, waiting)
		}); err != nil {
			t.Fatalf("resume the answered root: %v", err)
		}

		assertArchitectEntryRaised(t, pool, engine, seededEntry+1, waiting)
	})

	t.Run("the reconcile that finds a person already acted", func(t *testing.T) {
		pool := migratedPool(t)
		root := architectEntryRoot()
		seedIssue(t, pool, root)
		seedArchitectEntryRow(t, pool)
		seedArchitectWorking(t, pool, root)
		seedSlot(t, pool, record.Slot{Issue: root.Key, Index: 0, AdmittedAt: answeredAt})
		// The question the architect handed to a person, the hand-back that starts the wait, and
		// the person's own act after it: the act decideAwait reads for actTellInPlace.
		seedRecord(t, pool, func(tx pgx.Tx) error {
			store := record.NewStore()
			if err := store.PutRootAsk(t.Context(), tx, record.RootAsk{Issue: root.Key, Ask: "ask-1",
				Kind: record.AskQuestion, Open: true, Seq: 4, WaitingOn: "human", WaitingOnSeq: 4, ByTree: true, PersonSeq: 7}); err != nil {
				return err
			}
			return store.Declare(t.Context(), tx, root.Key, record.EventMark{Seq: 5, At: answeredAt})
		})

		if entered := reconcileRoots(t, pool, root.Key); len(entered) != 0 {
			t.Fatalf("the roots that entered the wait = %v, want none: a person had already acted, so its architect is told where it stands", entered)
		}
		if got := phaseOf(t, pool, root.Key); got != phase.Admitted {
			t.Fatalf("the root told in place stands in %s, want it left admitted, holding its slot", got)
		}
		// The wait the architect is told of is the one its hand-back began, on the question still
		// with the person, answered by the person's act on it as the reconcile read it.
		engine := testEngine(config.DesignGateRootIssues, nil)
		told := engine.now()
		assertArchitectEntryRaised(t, pool, engine, seededEntry+1, record.Awaiting{Ask: "ask-1", Since: record.EventMark{Seq: 5, At: answeredAt},
			AnsweredAt: &told, AnsweredBy: record.AnsweredByAsk, AnsweredDetail: "ask-1"})
	})
}

// assertArchitectEntryRaised holds the resume's two writes to each other: the architect's row
// names entry, carries the work of it, keeps waiting, the wait it answers, and the moment the
// engine raised it, and the one start the resume wrote carries that same entry and that same wait,
// claims no kept decision, and carries no text of its own. The daemon renders that start's task at
// its send (its rootTask, through ResumeAwaitedRootTask), so it is rendered here the same way,
// from the record as it stands, and it must be the very task the row keeps.
func assertArchitectEntryRaised(t *testing.T, pool *pgxpool.Pool, engine *Engine, entry int64, waiting record.Awaiting) {
	t.Helper()
	row := entryRow(t, pool, claim.RoleArchitect)
	if row.Entry != entry || row.EntryTask == "" || row.EntryTask == seededArchitectTask || !row.EntryRaisedAt.Equal(engine.now()) {
		t.Fatalf("the architect's row after the resume = entry %d, task %q, raised at %s; want entry %d carrying the resume's own task, raised at %s",
			row.Entry, row.EntryTask, row.EntryRaisedAt, entry, engine.now())
	}
	if !sameWait(row.EntryAwaiting, waiting) {
		t.Fatalf("the architect's row after the resume keeps the wait %+v, want %+v: a lost start is built again from it and rendered at its send "+
			"(SendArchitectStart)", row.EntryAwaiting, waiting)
	}
	start := architectStartOf(t, pool)
	if start.Entry != entry || start.Task != "" || start.Kept != nil || !sameWait(start.Awaiting, waiting) {
		t.Fatalf("the resume's start = entry %d carrying %q naming %+v and answering %+v, want entry %d carrying no text and no kept decision, "+
			"answering %+v: its task is rendered at its send from the wait it answers", start.Entry, start.Task, start.Kept, start.Awaiting, entry, waiting)
	}
	if sent := renderedResume(t, pool, entryIssue, *start.Awaiting); sent != row.EntryTask {
		t.Fatalf("the resume's start rendered as its send renders it = %q, want the %q its row keeps: the two are written in one transaction "+
			"from one record", sent, row.EntryTask)
	}
}

// sameWait says whether got is the wait want, each moment of it read as an instant: a start
// carries its wait as JSON, which keeps the instant of a moment read back from the record but
// not always the location it was read in.
func sameWait(got *record.Awaiting, want record.Awaiting) bool {
	if got == nil || (got.AnsweredAt == nil) != (want.AnsweredAt == nil) || !got.Since.At.Equal(want.Since.At) ||
		got.AnsweredAt != nil && !got.AnsweredAt.Equal(*want.AnsweredAt) {
		return false
	}
	g, w := *got, want
	g.AnsweredAt, g.Since.At, w.AnsweredAt, w.Since.At = nil, time.Time{}, nil, time.Time{}
	return reflect.DeepEqual(g, w)
}

// A wake brings a released pod back and tells the role why; it hands the architect no phase
// entry's work, so its start carries no entry (wakeRow) and the architect's row is left where it
// stood. The two numbers on the claim stand with it, because a delivery that carries no entry
// moves neither (supervise's TestADeliveryOfNoEntryLeavesTheGivenEntryAndTheServedEntryAlone),
// and the wake's queued start carries none either, so it is not read as the work of the entry the
// row names. So an architect behind its entry is still behind it after the wake, and the pass
// that converges entries would still send the work that entry names.
//
// The resume of a cancelled idle release is the other delivery of this shape (the daemon's
// releaseResume), which carries no task at all.
func TestAWakeStartRaisesNoArchitectEntryAndLeavesTheArchitectOwedItsWork(t *testing.T) {
	t.Parallel()
	rig := newWakeRig(t)
	token := wakeToken(t, claim.RoleArchitect)
	// The architect's row as a transition left it, and a claim given the entry before it: the
	// work of the row's entry never reached this claim.
	seedArchitectEntryRow(t, rig.pool)
	if _, err := rig.pool.Exec(context.Background(), `update claims set given_entry = $2, served_entry = $2 where token = $1`,
		string(token), seededEntry-1); err != nil {
		t.Fatalf("record what the architect's claim was given: %v", err)
	}

	rig.apply("role-message", intake.ListenerWake{Kind: intake.ListenerWakeRole, Role: string(token), At: wakeAt, Report: "report-1"})

	oneWake(t, rig.pool, claim.RoleArchitect, pods.WakeRoleMessage, string(token))
	start := architectStartOf(t, rig.pool)
	if start.Entry != 0 || start.Task != WakeTask(pods.WakeRoleMessage, string(token), StoppedByRelease) {
		t.Fatalf("the wake's start = entry %d carrying %q, want no entry and the wake's own task: a wake hands no phase entry's work",
			start.Entry, start.Task)
	}
	assertArchitectEntryUntouched(t, rig.pool, "the wake")

	seedRecord(t, rig.pool, func(tx pgx.Tx) error {
		store := record.NewStore()
		root, err := store.Issue(t.Context(), tx, entryIssue)
		if err != nil {
			return err
		}
		runs, err := store.RoleRuns(t.Context(), tx, []record.RoleRunKey{
			{Token: token, Issue: entryIssue, Role: claim.RoleArchitect, Generation: root.Generation}})
		if err != nil {
			return err
		}
		run := runs[token]
		if run.Claim == nil || run.Claim.GivenEntry != seededEntry-1 {
			t.Fatalf("the architect's claim after the wake = %+v, want it still given entry %d", run.Claim, seededEntry-1)
		}
		if !OwedEntry(run, *root, seededEntry) {
			t.Fatalf("the architect reads as holding entry %d's work after the wake, want it still owed: the wake's start carries no entry", seededEntry)
		}
		return nil
	})
}
