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
// opened is still a root whose own architect is the only role of its tree at work — and it is no
// wider than the first-task derivation serves: a root past admitted under its own power has a
// phase role that owns its work, and the hand-over row of this generation is what says the phase
// a root stands in came with it (record.Store.HandedOverRoot).
//
// The design gate is read by none of it, which the two admitted cases below hold: one with no
// gate row at all and one whose gate stands open are both inside the rule.
func TestArchitectSweepReachReadsTheRecordAlone(t *testing.T) {
	pool := migratedPool(t)
	store := record.NewStore()
	root := func(key string, at phase.Phase, generation uint64) record.Issue {
		return record.Issue{Key: key, Tree: key, Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion",
			Project: "LEGION", Title: key, Phase: at, Generation: generation, Status: "in_progress", Rank: key,
			HandedOver: true, LastDispatchSeq: 1}
	}
	for _, tc := range []struct {
		name string
		// issue is the record the reach is asked about.
		issue record.Issue
		// gateOpen registers an approved design gate of the issue, which the reach never reads.
		gateOpen bool
		// handedOver records the issue as a root a hand-over created at that generation, zero
		// for none.
		handedOver uint64
		reaches    bool
		why        string
	}{
		{
			name: "a root at admitted whose gate has never opened", issue: root("LEGION-801", phase.Admitted, 1), reaches: true,
			why: "its architect is writing the design, which is the whole of the tree's work",
		},
		{
			name: "a root at admitted whose gate has opened", issue: root("LEGION-802", phase.Admitted, 1), gateOpen: true, reaches: true,
			why: "record.AtWork answers false here, and the architect is still the only role of the tree at work: this is the measured defect",
		},
		{
			name: "a child at admitted", issue: func() record.Issue {
				child := root("LEGION-803", phase.Admitted, 1)
				parent := "LEGION-800"
				child.Tree, child.Parent = "LEGION-800", &parent
				return child
			}(), why: "a child's own sub-architect claim is outside the rule: no daemon start creates one",
		},
		{
			name:  "a root a hand-over carried in at the phase that came with it",
			issue: root("LEGION-804", phase.Implementing, 2), handedOver: 2, reaches: true,
			why: "the hand-over row of this generation says the phase it stands in came with it, and its architect has still to take the tree up",
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
			var reaches bool
			seedRecord(t, pool, func(tx pgx.Tx) error {
				var err error
				reaches, err = ArchitectAtWork(t.Context(), tx, store, tc.issue)
				return err
			})
			if reaches != tc.reaches {
				t.Fatalf("ArchitectAtWork(%s) = %t, want %t: %s", tc.issue.Key, reaches, tc.reaches, tc.why)
			}
		})
	}
}

// What stops the sweep writing a second start for a root architect (ArchitectStartPending). It
// is owed, the daemon's own reading of a start a role has not taken, with one arm taken off: a
// tree with no architect claim at all is the tree the sweep writes the start for, so "has no
// claim yet" is not a reason to hold the row back, while a start still queued for that same tree
// is.
func TestArchitectSweepStartPendingHoldsTheRowBackWhileOneIsOnItsWay(t *testing.T) {
	queued := []record.QueuedSupervise{{ID: 7, Request: record.SuperviseRequest{Op: "start", Role: claim.RoleArchitect}}}
	for _, tc := range []struct {
		name    string
		run     record.RoleRun
		pending bool
		why     string
	}{
		{
			name: "no claim and no queued start", run: record.RoleRun{},
			why: "nothing is on its way, and this tree is exactly the one the rule writes the start for",
		},
		{
			name: "no claim and a start still queued", run: record.RoleRun{Queued: queued}, pending: true,
			why: "that row is the start, and a second would start two architects of one tree",
		},
		{
			name: "a claim still starting", run: record.RoleRun{Claim: &record.RoleClaim{State: supervise.StateLaunching}}, pending: true,
			why: "the start that created it is running now, and its delivery carries the entry's task",
		},
		{
			name:    "a claim that has not confirmed its task",
			run:     record.RoleRun{Claim: &record.RoleClaim{State: supervise.StateReady, Pending: &supervise.Delivery{Task: "take the tree up"}}},
			pending: true, why: "a task in the claim's hands is not owed again",
		},
		{
			name: "a live claim holding a task it confirmed",
			run: record.RoleRun{Claim: &record.RoleClaim{State: supervise.StateIdle,
				Pending: &supervise.Delivery{Task: "take the tree up", ConfirmedAt: seededRaisedAt}}},
			why: "it took the work of an earlier entry and holds none of this one's: the claim this rule exists to reach",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reason, pending := ArchitectStartPending(tc.run)
			if pending != tc.pending {
				t.Fatalf("ArchitectStartPending = %t (%q), want %t: %s", pending, reason, tc.pending, tc.why)
			}
			if pending && reason == "" {
				t.Fatalf("ArchitectStartPending holds the row back with no reason: the log line names it")
			}
		})
	}
}

// The start the sweep writes again is the start StartArchitect writes: the same row, at the entry
// the row already names. It raises no entry, since the work it sends is the work that entry
// already stands for, and it waits for no admission slot, since the root holds the row the cap
// counts already.
//
// What it carries turns on what the row says raised the entry (record.PhaseRow.EntryVerbatim),
// and on nothing in the task's text. A task the derivation wrote is derived again from the record
// as it stands (ArchitectStartTask) rather than read off the row, since a text composed once and
// sent again can name a kept decision another telling has already claimed. That is so of the
// derivation's every task: the one that ends in its closing sentence, the one it writes for a
// root past admitted under its own power, which is what a person decided and no closing sentence
// (its second rule), and no task at all, which the upgrade leaves on the rows it fills. A task the
// resume of a person's answer wrote names the answer and no kept decision, and the answer is the
// work that entry owes, so it is sent as written and claims nothing, whatever its text ends in.
func TestArchitectSweepSendRederivesTheTaskAtTheEntryTheRowNames(t *testing.T) {
	const resumed = "A person answered your ask ask-q on LEGION-208, or replied in its thread; read it. Continue."
	for _, tc := range []struct {
		name     string
		stored   string
		verbatim bool
		// tail is whether the stored text ends in the derivation's closing sentence, which the
		// cases cover both ways for each mark, so the send is shown to read the mark alone.
		tail bool
	}{
		{name: "an entry the derivation raised", stored: seededArchitectTask + architectStartTail, tail: true},
		{name: "an entry the derivation raised for a root past admitted, what was decided alone",
			stored: ArchitectStartTask(ArchitectStart{Root: rootAt(phase.Planning), Decided: decidedOnTheTree})},
		{name: "an entry the upgrade gave no task", stored: ""},
		{name: "an entry a resume raised", stored: resumed, verbatim: true},
		{name: "an entry a resume raised whose text ends as the derivation's does", stored: resumed + architectStartTail,
			verbatim: true, tail: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if strings.HasSuffix(tc.stored, architectStartTail) != tc.tail {
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
