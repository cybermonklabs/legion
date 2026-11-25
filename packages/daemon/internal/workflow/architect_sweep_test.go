package workflow

import (
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// The reach of the entry sweep over a root's own architect, read from the record alone
// (ArchitectAtWork). It is wider than record.AtWork at admitted — a root whose design gate has
// opened is still a root whose own architect is the only role at work on the root itself — and it
// is no wider than the work the rule has to send: a root past admitted under its own power has a
// phase role that owns its work, the hand-over row of this generation is what says a root stands
// in the run a hand-over carried it into (record.Store.HandedOverRoot), and the one entry such a
// root's architect is owed past admitted is a resume's its claim was not given, which the
// admission pass that resumed the root can have promoted past admitted.
//
// The design gate is read by none of it, which the two admitted cases below hold: one with no
// gate row at all and one whose gate stands open are both inside the rule.
func TestArchitectSweepReachReadsTheRecordAlone(t *testing.T) {
	t.Parallel()
	pool := migratedPool(t)
	store := record.NewStore()
	root := func(key string, at phase.Phase, generation uint64) record.Issue {
		return record.Issue{Key: key, Tree: key, Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion",
			Project: "LEGION", Title: key, Phase: at, Generation: generation, Status: "in_progress", Rank: key,
			HandedOver: true, LastDispatchSeq: 1}
	}
	waiting := func(key string) record.Issue {
		issue := root(key, phase.AwaitingPerson, 1)
		issue.Awaiting = &record.Awaiting{Ask: "ask-q", Since: record.EventMark{Seq: 10}}
		return issue
	}
	resume := record.PhaseEntry{Entry: 3, Verbatim: true}
	derived := record.PhaseEntry{Entry: 3}
	for _, tc := range []struct {
		name string
		// issue is the record the reach is asked about.
		issue record.Issue
		// gateOpen registers an approved design gate of the issue, which the reach never reads.
		gateOpen bool
		// handedOver records the issue as a root a hand-over created at that generation, zero
		// for none.
		handedOver uint64
		// entry is what the architect's phases row names, and claimed gives the architect a claim
		// given the entry given; without it the tree's architect claim does not exist.
		entry   record.PhaseEntry
		claimed bool
		given   int64
		reaches bool
		why     string
	}{
		{
			name: "a root at admitted whose gate has never opened", issue: root("LEGION-801", phase.Admitted, 1), reaches: true,
			why: "its architect is writing the design, which is the whole of the tree's work",
		},
		{
			name: "a root at admitted whose gate has opened", issue: root("LEGION-802", phase.Admitted, 1), gateOpen: true, reaches: true,
			why: "record.AtWork answers false here, and the architect is still the only role at work on the root itself: this is the measured defect",
		},
		{
			name: "a child at admitted", issue: func() record.Issue {
				child := root("LEGION-803", phase.Admitted, 1)
				parent := "LEGION-800"
				child.Tree, child.Parent = "LEGION-800", &parent
				return child
			}(), why: "a child's own sub-architect claim is outside the rule: its start carries no task and raises no entry",
		},
		{
			name:  "a root a hand-over carried in, in the phase that came with it",
			issue: root("LEGION-804", phase.Implementing, 2), handedOver: 2, reaches: true,
			why: "the hand-over row of this generation says it stands in the run that hand-over carried it into, and its architect has still to take the tree up",
		},
		{
			name: "a root a hand-over carried into accepting", issue: root("LEGION-805", phase.Accepting, 2), handedOver: 2, reaches: true,
			why: "the carry is what put it in accepting, so no notice of a move told its architect anything",
		},
		{
			name: "a root past admitted under its own power", issue: root("LEGION-806", phase.Implementing, 1),
			why: "its implementer owns the work where it stands, and no hand-over carried it there",
		},
		{
			name: "a root whose hand-over belongs to an earlier generation", issue: root("LEGION-807", phase.Reviewing, 3), handedOver: 2,
			why: "that carry belongs to a run that has ended, so it says nothing of the run standing now",
		},
		{
			name: "a root a hand-over carried in that is done", issue: root("LEGION-808", phase.Done, 2), handedOver: 2,
			why: "a phase that carries no run is no phase to take up (record.Issue.CarriesRun)",
		},
		{
			name:  "a root past admitted whose architect's claim was not given the entry a resume raised",
			issue: root("LEGION-809", phase.Planning, 1), entry: resume, claimed: true, given: 2, reaches: true,
			why: "the answer that resumed it promoted it past admitted, and the resume's work has still to reach the architect",
		},
		{
			name:  "a root past admitted owed the entry a resume raised, with no architect claim at all",
			issue: root("LEGION-810", phase.Planning, 1), entry: resume, reaches: true,
			why: "no claim was given that entry, and the start the rule writes is what creates the claim",
		},
		{
			name:  "a root past admitted whose architect's claim was given the entry a resume raised",
			issue: root("LEGION-811", phase.Planning, 1), entry: resume, claimed: true, given: 3,
			why: "the resume's work reached the claim, so the planner owns the work where the root stands",
		},
		{
			name:  "a root past admitted whose architect's row names an entry the derivation raised",
			issue: root("LEGION-812", phase.Planning, 1), entry: derived, claimed: true, given: 2,
			why: "the derivation serves no root past admitted under its own power, so nothing of that entry is owed here",
		},
		{
			name:  "a root waiting on a person whose architect's claim was not given the entry a resume raised",
			issue: waiting("LEGION-813"), entry: resume, claimed: true, given: 2,
			why: "its architect handed the work back since that resume, and the answer to the new wait queues the next start",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			seedIssue(t, pool, tc.issue)
			if tc.gateOpen {
				version := 1
				seedGate(t, pool, record.DesignGate{Issue: tc.issue.Key, ArtifactID: "artifact-" + tc.issue.Key,
					LatestVersion: 1, ApprovedVersion: &version, EverOpened: true})
			}
			if tc.handedOver != 0 {
				seedRecord(t, pool, func(tx pgx.Tx) error {
					return store.PutHandedOverRoot(t.Context(), tx, record.HandedOverRoot{Issue: tc.issue.Key, Generation: tc.handedOver})
				})
			}
			var run record.RoleRun
			if tc.claimed {
				run.Claim = &record.RoleClaim{GivenEntry: tc.given}
			}
			var reaches bool
			seedRecord(t, pool, func(tx pgx.Tx) error {
				var err error
				reaches, err = ArchitectAtWork(t.Context(), tx, store, tc.issue, tc.entry, run)
				return err
			})
			if reaches != tc.reaches {
				t.Fatalf("ArchitectAtWork(%s) = %t, want %t: %s", tc.issue.Key, reaches, tc.reaches, tc.why)
			}
		})
	}
}

// What stops the sweep writing a second start for a root architect (ArchitectStartPending). It
// is owed, the daemon's own reading of a start a role has not taken, with one arm taken off and
// one read narrower: a tree with no architect claim at all is the tree the sweep writes the start
// for, so "has no claim yet" is not a reason to hold the row back, while a start still queued for
// that same tree is; and a task the claim holds unconfirmed holds the row back only while the
// root's own record has not superseded it (supervise's Delivery.SupersededBy), which it never has
// for a task of no run or of no entry, a wake's among them. The root of every case stands at
// generation 2, and its architect's row names entry 3 (rowEntry) where the case does not say it
// names none.
func TestArchitectSweepStartPendingHoldsTheRowBackWhileOneIsOnItsWay(t *testing.T) {
	t.Parallel()
	const rowEntry = 3
	queued := []record.QueuedSupervise{{ID: 7, Request: record.SuperviseRequest{Op: "start", Role: claim.RoleArchitect}}}
	holding := func(state supervise.ClaimState, task supervise.Delivery) *record.RoleClaim {
		return &record.RoleClaim{State: state, Pending: &task}
	}
	root := record.Issue{Key: "LEGION-208", Tree: "LEGION-208", Phase: phase.Admitted, Generation: 2}
	for _, tc := range []struct {
		name    string
		run     record.RoleRun
		entry   int64
		pending bool
		// reason is the reason a case that names one must be held back for, which is owed's.
		reason string
		why    string
	}{
		{
			name: "no claim and no queued start", run: record.RoleRun{}, entry: rowEntry,
			why: "nothing is on its way, and this tree is exactly the one the rule writes the start for",
		},
		{
			name: "no claim and a start still queued", run: record.RoleRun{Queued: queued}, entry: rowEntry, pending: true,
			why: "that row is the start, and a second would start the root's one architect claim again",
		},
		{
			name: "a claim still starting", run: record.RoleRun{Claim: &record.RoleClaim{State: supervise.StateLaunching}}, entry: rowEntry,
			pending: true, why: "the start that created it is running now, and its delivery carries the entry's task",
		},
		{
			name: "a claim that has not confirmed the task of the row's entry", entry: rowEntry,
			run:     record.RoleRun{Claim: holding(supervise.StateReady, supervise.Delivery{Task: "take the tree up", Generation: 2, Entry: rowEntry})},
			pending: true, why: "a task in the claim's hands is not owed again",
		},
		{
			name: "a claim that has not confirmed a task of no run", entry: rowEntry,
			run:     record.RoleRun{Claim: holding(supervise.StateReady, supervise.Delivery{Task: "look at the failing check"})},
			pending: true, why: "a task of no run, an operator's own, is one no start of the root's run takes the place of",
		},
		{
			name: "a claim that has not confirmed a wake's task of the root's run", entry: rowEntry,
			run: record.RoleRun{Claim: holding(supervise.StateReady,
				supervise.Delivery{Task: "Your pod was released and has come back.", Generation: 2})},
			pending: true, reason: "has not taken its task yet",
			why: "a wake's task carries no entry (wakeRow), so no start of an entry takes its place: the claim sends it first",
		},
		{
			name: "a claim that has not confirmed a wake's task of the run before", entry: rowEntry,
			run: record.RoleRun{Claim: holding(supervise.StateIdle,
				supervise.Delivery{Task: "Your pod was released and has come back.", Generation: 1})},
			pending: true, reason: "has not taken its task yet",
			why: "a task of no entry is superseded by nothing, whatever run it is of",
		},
		{
			name: "the row names no entry, and the claim has not confirmed a task of the root's run", entry: 0,
			run:     record.RoleRun{Claim: holding(supervise.StateReady, supervise.Delivery{Task: "take the tree up", Generation: 2})},
			pending: true, why: "a task of the root's own run at no entry is below no entry the row names",
		},
		{
			name: "a claim that has not confirmed the task of the run before", entry: rowEntry,
			run: record.RoleRun{Claim: holding(supervise.StateSuspended, supervise.Delivery{Task: "take the tree up", Generation: 1, Entry: 1})},
			why: "re-admission ended that run, so its task is no start on its way: the start of the row's entry takes its place in the claim's hands",
		},
		{
			name: "a claim that has not confirmed the task of an entry below the row's", entry: rowEntry,
			run: record.RoleRun{Claim: holding(supervise.StateIdle, supervise.Delivery{Task: "take the tree up", Generation: 2, Entry: rowEntry - 1})},
			why: "the row has moved past that entry, so its task is no start of the row's entry on its way",
		},
		{
			name: "the row names no entry, and the claim has not confirmed the task of the run before", entry: 0,
			run: record.RoleRun{Claim: holding(supervise.StateIdle, supervise.Delivery{Task: "take the tree up", Generation: 1, Entry: 4})},
			why: "a task of an earlier run is superseded whatever entry it carries, and the raise's start carries the root's run",
		},
		{
			name: "a claim still starting that holds the task of the run before", entry: rowEntry,
			run:     record.RoleRun{Claim: holding(supervise.StateLaunching, supervise.Delivery{Task: "take the tree up", Generation: 1, Entry: 1})},
			pending: true, why: "the claim is still starting, which holds the row back whatever task it holds",
		},
		{
			name: "a start still queued beside the claim's task of the run before", entry: rowEntry,
			run: record.RoleRun{Queued: queued,
				Claim: holding(supervise.StateSuspended, supervise.Delivery{Task: "take the tree up", Generation: 1, Entry: 1})},
			pending: true, why: "that row is the start of the row's entry, already on its way",
		},
		{
			name: "a live claim holding a task it confirmed", entry: rowEntry,
			run: record.RoleRun{Claim: holding(supervise.StateIdle,
				supervise.Delivery{Task: "take the tree up", Generation: 2, Entry: rowEntry, ConfirmedAt: seededRaisedAt})},
			why: "it took that work and holds nothing it has not taken: the claim this rule exists to reach once its row moves on",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var task *supervise.Delivery
			if tc.run.Claim != nil {
				task = tc.run.Claim.Pending
			}
			reason, pending := ArchitectStartPending(root, tc.entry, tc.run)
			if pending != tc.pending {
				t.Fatalf("ArchitectStartPending = %t (%q), want %t: %s", pending, reason, tc.pending, tc.why)
			}
			if pending && reason == "" {
				t.Fatalf("ArchitectStartPending holds the row back with no reason: the log line names it")
			}
			if tc.reason != "" && reason != tc.reason {
				t.Fatalf("ArchitectStartPending holds the row back as %q, want %q: %s", reason, tc.reason, tc.why)
			}
			if tc.run.Claim != nil && tc.run.Claim.Pending != task {
				t.Fatalf("ArchitectStartPending changed the task of the claim it was handed: the caller reads that claim again for the stall arms")
			}
		})
	}
}

// The start the sweep writes again is the start StartArchitect writes: the same row, at the entry
// the row already names. It raises no entry, since the work it sends is the work that entry
// already stands for, and it waits for no admission slot, since a root architect's start takes
// none, whether its root holds its row or not.
//
// What it carries turns on what the row says raised the entry (record.PhaseRow.EntryVerbatim),
// and on nothing in the task's text. A task the derivation wrote is derived again from the record
// as it stands (ArchitectStartTask) rather than read off the row, since a text composed once and
// sent again can name a kept decision another telling has already claimed. That is so of the
// derivation's every task: the one that ends in a shape's closing sentence, the one it writes for
// a root past admitted under its own power, which is what a person decided and no closing sentence
// (its second rule), and no task at all, which the upgrade leaves on the rows it fills. A task the
// resume of a person's answer wrote names the answer and no kept decision, and the answer is the
// work that entry owes, so it is sent as written and claims nothing, whatever its text ends in.
func TestArchitectSweepSendRederivesTheTaskAtTheEntryTheRowNames(t *testing.T) {
	t.Parallel()
	const resumed = "A person answered your ask ask-q on LEGION-208, or replied in its thread; read it. Continue."
	for _, tc := range []struct {
		name     string
		stored   string
		verbatim bool
		// tail is whether the stored text ends in the closing sentence of the shape the derivation
		// writes for this root (freshClosing), which the cases cover both ways for each mark, so
		// the send is shown to read the mark alone.
		tail bool
	}{
		{name: "an entry the derivation raised", stored: seededArchitectTask + freshClosing, tail: true},
		{name: "an entry the derivation raised for a root past admitted, what was decided alone",
			stored: ArchitectStartTask(ArchitectStart{Root: rootAt(phase.Planning), Decided: decidedOnTheTree})},
		{name: "an entry the upgrade gave no task", stored: ""},
		{name: "an entry a resume raised", stored: resumed, verbatim: true},
		{name: "an entry a resume raised whose text ends as the derivation's does", stored: resumed + freshClosing,
			verbatim: true, tail: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if strings.HasSuffix(tc.stored, freshClosing) != tc.tail {
				t.Fatalf("the stored task %q ends in the closing sentence = %t, want %t: the case's premise", tc.stored, !tc.tail, tc.tail)
			}
			pool := migratedPool(t)
			root := architectEntryRoot()
			seedIssue(t, pool, root)
			row := record.PhaseRow{Issue: entryIssue, Role: claim.RoleArchitect, Claim: wakeToken(t, claim.RoleArchitect),
				Entry: seededEntry, EntryTask: tc.stored, EntryVerbatim: tc.verbatim, EntryRaisedAt: seededRaisedAt}
			seedPhase(t, pool, row)
			engine := testEngine(config.DesignGateRootIssues, nil)

			seedRecord(t, pool, func(tx pgx.Tx) error {
				return engine.SendArchitectStart(t.Context(), tx, root, row)
			})

			start := architectStartOf(t, pool)
			want := ArchitectStartTask(ArchitectStart{Root: root, Gate: record.TreeGate{Steps: engine.GatedSteps()}})
			if tc.verbatim {
				want = tc.stored
			}
			if start.Task != want {
				t.Fatalf("the task the sweep sends for a row whose task is %q, sent as written %t, = %q; want %q",
					tc.stored, tc.verbatim, start.Task, want)
			}
			if tc.verbatim && start.Kept != nil {
				t.Fatalf("the resume's task sent again claims %+v, want no kept decision: it names none", start.Kept)
			}
			if start.Entry != seededEntry || start.Phase != "" || start.NeedsSlot {
				t.Fatalf("the start = entry %d, phase %q, needs slot %t; want entry %d, no phase and no wait for a slot: a root architect's start is none of the cap's",
					start.Entry, start.Phase, start.NeedsSlot, seededEntry)
			}
			got := entryRow(t, pool, claim.RoleArchitect)
			if got.Entry != seededEntry || got.EntryTask != tc.stored || got.EntryVerbatim != tc.verbatim {
				t.Fatalf("after the sweep's re-send the architect's row = entry %d, task %q, sent as written %t; want the entry %d, task %q and mark %t it stood at",
					got.Entry, got.EntryTask, got.EntryVerbatim, seededEntry, tc.stored, tc.verbatim)
			}
		})
	}
}
