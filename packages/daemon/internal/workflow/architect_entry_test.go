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
	"github.com/sjawhar/legion/daemon/internal/pods"
	"github.com/sjawhar/legion/daemon/internal/record"
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

// architectEntryToken is the claim token of entryIssue's root architect.
func architectEntryToken(t *testing.T) claim.Token {
	t.Helper()
	project, err := claim.ProjectToken("LEGION")
	if err != nil {
		t.Fatal(err)
	}
	token, err := claim.NewToken(project, entryIssue, claim.RoleArchitect)
	if err != nil {
		t.Fatal(err)
	}
	return token
}

// seedArchitectEntryRow records the architect's phases row as the start of an earlier entry left
// it: on the seeded entry, carrying that entry's work and the moment it was raised.
func seedArchitectEntryRow(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	seedPhase(t, pool, record.PhaseRow{Issue: entryIssue, Role: claim.RoleArchitect, Claim: architectEntryToken(t),
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
// raised from outside this package, and it raises the number rather than writing one: a tree
// whose architect has been handed work before counts from where it stood, which is what tells
// the work of one entry from the work of the next.
//
// The row is the architect's own: the entry of a phase worker of the same issue stands where it
// was, because the record counts the entries each role has been handed.
func TestTheRootArchitectEntryIsRaisedWithItsTaskAndTheMomentItWasRaised(t *testing.T) {
	pool := migratedPool(t)
	root := architectEntryRoot()
	seedIssue(t, pool, root)
	seedArchitectEntryRow(t, pool)
	seedEntryRow(t, pool, claim.RoleImplementer, "implement-claim")
	engine := testEngine(config.DesignGateRootIssues, nil)

	const first = "A person answered your ask ask-1 on LEGION-208."
	if entry := enterArchitect(t, pool, engine, root, first); entry != seededEntry+1 {
		t.Fatalf("the architect's entry after the first raise = %d, want %d: the number is raised, never written", entry, seededEntry+1)
	}
	row := entryRow(t, pool, claim.RoleArchitect)
	if row.Entry != seededEntry+1 || row.EntryTask != first || !row.EntryRaisedAt.Equal(engine.now()) {
		t.Fatalf("the architect's row = entry %d, task %q, raised at %s; want entry %d carrying %q, raised at the %s the engine read from its own clock",
			row.Entry, row.EntryTask, row.EntryRaisedAt, seededEntry+1, first, engine.now())
	}

	const second = "A person requested changes to your spec on LEGION-208."
	if entry := enterArchitect(t, pool, engine, root, second); entry != seededEntry+2 {
		t.Fatalf("the architect's entry after the second raise = %d, want %d: each entry counts from the one before it", entry, seededEntry+2)
	}
	if row := entryRow(t, pool, claim.RoleArchitect); row.Entry != seededEntry+2 || row.EntryTask != second {
		t.Fatalf("the architect's row after the second raise = entry %d carrying %q, want entry %d carrying the work of that entry",
			row.Entry, row.EntryTask, seededEntry+2)
	}
	assertEntryUntouched(t, pool, claim.RoleImplementer, "the architect's two entries")
}

// enterArchitect raises the architect's entry for task through the one exported door onto it, in
// a transaction of its own as a transition's own transaction does.
func enterArchitect(t *testing.T, pool *pgxpool.Pool, engine *Engine, root record.Issue, task string) int64 {
	t.Helper()
	var entry int64
	seedRecord(t, pool, func(tx pgx.Tx) error {
		var err error
		entry, err = engine.EnterArchitect(t.Context(), tx, root, task)
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
			token := architectEntryToken(t)
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
				if tc.owed && row.EntryTask != seededArchitectTask {
					t.Fatalf("the architect is owed entry %d and its row names the work %q, want %q: what it is owed is the record's",
						row.Entry, row.EntryTask, seededArchitectTask)
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
		string(architectEntryToken(t)), project, root.Tree, root.Key, int64(root.Generation), given); err != nil {
		t.Fatalf("seed the architect's claim: %v", err)
	}
}

// The resume of a root that waited on a person hands its architect work it was not on, so it
// raises the architect's entry, writes the resume task as the work of that entry and carries the
// number on the start it writes (ResumeAwaitedRoot). Both of its callers get it: the promotion of
// an answered root, which calls it as this case does (internal/admit's record.LineAnswered
// branch), and the reconcile that finds a person already acted after the hand-back, which tells
// the live architect where it stands (tellInPlace). A row lost at tellInPlace would lose a
// person's answer, which is what the record is for.
func TestResumeAwaitedRootRaisesTheArchitectEntryForBothOfItsCallers(t *testing.T) {
	answeredAt := time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)
	t.Run("the promotion of an answered root", func(t *testing.T) {
		pool := migratedPool(t)
		root := architectEntryRoot()
		seedIssue(t, pool, root)
		seedArchitectEntryRow(t, pool)
		engine := testEngine(config.DesignGateRootIssues, nil)
		waiting := record.Awaiting{Ask: "ask-1", Since: record.EventMark{Seq: 5}, AnsweredAt: &answeredAt,
			AnsweredBy: record.AnsweredByAsk}

		seedRecord(t, pool, func(tx pgx.Tx) error {
			ctx, _ := intake.WithCommitHooks(context.Background())
			return engine.ResumeAwaitedRoot(ctx, tx, root, waiting)
		})

		assertArchitectEntryRaised(t, pool, engine, seededEntry+1)
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
		assertArchitectEntryRaised(t, pool, testEngine(config.DesignGateRootIssues, nil), seededEntry+1)
	})
}

// assertArchitectEntryRaised holds the resume's two writes to each other: the architect's row
// names entry, carries the work of it and the moment the engine raised it, and the one start the
// resume wrote carries that same entry and that same task.
func assertArchitectEntryRaised(t *testing.T, pool *pgxpool.Pool, engine *Engine, entry int64) {
	t.Helper()
	row := entryRow(t, pool, claim.RoleArchitect)
	if row.Entry != entry || row.EntryTask == "" || row.EntryTask == seededArchitectTask || !row.EntryRaisedAt.Equal(engine.now()) {
		t.Fatalf("the architect's row after the resume = entry %d, task %q, raised at %s; want entry %d carrying the resume's own task, raised at %s",
			row.Entry, row.EntryTask, row.EntryRaisedAt, entry, engine.now())
	}
	start := architectStartOf(t, pool)
	if start.Entry != entry || start.Task != row.EntryTask {
		t.Fatalf("the resume's start = entry %d carrying %q, want entry %d carrying the %q its row names: the two are written in one transaction",
			start.Entry, start.Task, entry, row.EntryTask)
	}
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
	rig := newWakeRig(t)
	token := wakeToken(t, claim.RoleArchitect)
	// The architect's row as a transition left it, and a claim given the entry before it: the
	// work of the row's entry never reached this claim.
	seedPhase(t, rig.pool, record.PhaseRow{Issue: entryIssue, Role: claim.RoleArchitect, Claim: token,
		Entry: seededEntry, EntryTask: seededArchitectTask, EntryRaisedAt: seededRaisedAt})
	if _, err := rig.pool.Exec(context.Background(), `update claims set given_entry = $2, served_entry = $2 where token = $1`,
		string(token), seededEntry-1); err != nil {
		t.Fatalf("record what the architect's claim was given: %v", err)
	}

	rig.apply("role-message", intake.ListenerWake{Kind: intake.ListenerWakeRole, Role: string(token), At: wakeAt, Report: "report-1"})

	oneWake(t, rig.pool, claim.RoleArchitect, pods.WakeRoleMessage, string(token))
	start := architectStartOf(t, rig.pool)
	if start.Entry != 0 || start.Task != WakeTask(pods.WakeRoleMessage, string(token)) {
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
