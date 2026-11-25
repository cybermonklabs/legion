package replay

import (
	"context"
	"maps"
	"slices"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// The holds on a start that are no word of record.HeldReasons, each with a window of its own
// below: the phase held, which a failed claim writes; a project's pause, under which the pass
// promotes nothing (admit's promoteHolds); and the two holds of a boot's catch-up, the summaries
// the boot listing found ahead of the Dispatch consumer (admit.Admission's pending) and the roots
// whose catch-up has not finished (its catchingUp).
const (
	holdHeldPhase  = "the phase held"
	holdPause      = "a project's pause"
	holdPending    = "a boot's pending summaries"
	holdCatchingUp = "a boot's roots catching up"
)

// handedToLegion is the labels of an issue a person hands to Legion.
var handedToLegion = []string{dispatch.LegionLabel}

// holdCase is one hold on a start, the window that records it and what its replay must show: that
// issue's next start is at starts, the recorded end of the hold, and none of it comes between the
// hold's recorded start (held) and then. worksOn is a hold that stops no work (at_work): the
// issue's work goes on from the hold's start, and its next start is the end of that work's
// segment. unplaced is a hold no word says the reason for (unclassified), at the window's start:
// the replay names the issue in its report of the issues its start state could not place. pieces
// is a hold whose replay also starts every issue's work segments where the record started its
// pieces of work (TestHoldsApplyAtTheirRecordedTimes, checkRecordedStarts), and timelines, where
// set, what the replay's two timelines must show beside agreeing (TestSnapshotAndLineTimelinesAgree,
// checkTimelines).
type holdCase struct {
	issue     string
	held      time.Time
	starts    time.Time
	worksOn   bool
	unplaced  bool
	pieces    bool
	timelines func(t *testing.T, window Window, result RunResult)
	window    func(t *testing.T) Window
}

// holdCases is every hold this test replays, by the word or the name of the hold.
func holdCases() map[string]holdCase {
	return map[string]holdCase{
		string(record.HeldTreeClosed): {issue: "ACME-2", held: clock(9, 30), starts: clock(11, 30), pieces: true, window: treeClosedWindow},
		string(record.HeldTreeParked): {issue: "ACME-2", held: clock(11, 30), starts: clock(12, 30), window: treeParkedWindow},
		string(record.HeldLabelOff):   {issue: "ACME-2", held: clock(9, 40), starts: clock(10, 40), window: labelOffWindow},
		string(record.HeldAncestry): {issue: "ACME-2", held: clock(9, 10), starts: clock(10, 10), pieces: true, window: ancestryWindow,
			timelines: func(t *testing.T, window Window, result RunResult) {
				// ACME-2, a queued root, stands under ACME-1, a live root, from 09:10 to 10:10.
				assertWaitsForNoSlot(t, window, result, "ACME-2", clock(9, 40))
			}},
		string(record.HeldRootAwaitingPerson): {issue: "ACME-2", held: clock(9, 20), starts: clock(10, 20), window: rootAwaitingPersonWindow},
		string(record.HeldDesignGate):         {issue: "ACME-2", held: clock(9, 10), starts: clock(10, 20), window: designGateWindow},
		string(record.HeldDispatchBlocker):    {issue: "ACME-2", held: clock(9, 30), starts: clock(10, 30), window: dispatchBlockerWindow},
		string(record.HeldChildrenFirst): {issue: "ACME-2", held: clock(9, 20), starts: clock(10, 20), window: childrenFirstWindow,
			timelines: func(t *testing.T, window Window, result RunResult) {
				// ACME-1, the root, waits on ACME-2 from 09:20 to the window's end at 15:00: one wait, open,
				// of 5 hours 40 minutes. ACME-2 has not started while its child holds it (09:20 to 10:20).
				assertTimelineWaits(t, result, map[WaitEnd]WaitHours{WaitOpen: {Count: 1, Hours: 17.0 / 3}})
				assertWaitsForNoSlot(t, window, result, "ACME-2", clock(9, 40))
			}},
		string(record.HeldAcceptedAncestor): {issue: "ACME-2", held: clock(11, 30), starts: clock(12, 30), pieces: true, window: acceptedAncestorWindow,
			timelines: func(t *testing.T, window Window, result RunResult) {
				// ACME-2 stands beneath ACME-1, accepted in its retro, from 11:30 to 12:30.
				assertWaitsForNoSlot(t, window, result, "ACME-2", clock(12, 0))
			}},
		string(record.HeldAtWork):       {issue: "ACME-1", held: clock(11, 0), starts: clock(11, 30), worksOn: true, window: atWorkWindow},
		string(record.HeldUnclassified): {issue: "ACME-1", unplaced: true, window: unclassifiedWindow},
		holdHeldPhase:                   {issue: "ACME-1", held: clock(10, 20), starts: clock(11, 20), pieces: true, window: heldPhaseWindow},
		holdPause:                       {issue: "ACME-1", held: clock(9, 40), starts: clock(10, 40), window: pauseWindow},
		holdPending:                     {issue: "ACME-1", held: clock(10, 0), starts: clock(10, 10), window: pendingWindow},
		holdCatchingUp:                  {issue: "ACME-1", held: clock(10, 0), starts: clock(10, 2), window: catchingUpWindow},
	}
}

// noRecordedWindowHolds is each word of record.HeldReasons that has no window below, with the
// reason it has none in place of a source: the hold comes only from an API act no recorded window
// holds, so a window built here could record the act but no release this tool replays ever wrote
// the hold. A window recorded after the release that added the act does write it, and then the
// word takes a case of its own above and leaves this map.
var noRecordedWindowHolds = map[string]string{
	string(record.HeldChildrenBeforeRetro): "the hold comes only from the API act register_plan, of a plan whose task's last step is not " +
		"the owner's acceptance (cybermonklabs/legion#4611, workflow's holdBeforeRetro), and every window this tool has " +
		"recorded was released before that act existed, so no recorded release holds a parent before its retro",
	string(record.HeldTaskStopped): "the hold comes only from the API act stop_task (cybermonklabs/legion#4611, workflow's " +
		"taskStopped), and every window this tool has recorded was released before that act existed, so no recorded " +
		"release stops a task",
}

// Every hold that can keep an issue from starting applies in a replay at the times the window
// records it (spec R2, assumption 7): each word of record.HeldReasons, read from the code so a
// word added there fails here until it has a window below or a reason why no recorded window holds
// it (noRecordedWindowHolds), the phase held, a project's pause and
// the two holds of a boot's catch-up. For each, a small window built in Go (windowBuilder) records
// the hold beginning and ending at known times while nothing else stops the issue, the window is
// replayed under the rule main with every segment traced, and the issue's next start must come
// exactly at the hold's recorded end and none before it from the hold's recorded start. at_work
// holds no work: the issue's work goes on from the hold's start and its next start is the end of
// that segment. unclassified is no hold any word explains: an issue that stands in it at the
// window's start is one the start state cannot place, and the replay names it in that report. A
// hold the trace or the driver gives no source fails here by its word. The replays of the windows
// of ancestry, accepted_ancestor, tree_closed and the phase held also start every piece of every
// issue's work where the record started it (TestHoldsApplyAtTheirRecordedTimes), and those of
// ancestry, children_first and accepted_ancestor keep their two timelines agreeing, with the waits
// and the first starts their holds keep (TestSnapshotAndLineTimelinesAgree).
func TestEveryHoldHasASourceInTheReplay(t *testing.T) {
	t.Parallel()
	cases := holdCases()
	for _, word := range record.HeldReasons {
		_, covered := cases[string(word)]
		why, named := noRecordedWindowHolds[string(word)]
		switch {
		case covered && named:
			t.Errorf("record.HeldReasons' word %q has a window below and a reason why no recorded window holds it (%s): keep the window and take the reason away",
				word, why)
		case !covered && !named:
			t.Errorf("record.HeldReasons holds the word %q, and this test replays no window for it: write one in which that hold begins and ends at recorded times, "+
				"or name in noRecordedWindowHolds why no recorded window holds it", word)
		}
	}
	for name, why := range noRecordedWindowHolds {
		if !slices.Contains(record.HeldReasons, record.HeldReason(name)) {
			t.Errorf("noRecordedWindowHolds names %q (%s), which is no word of record.HeldReasons", name, why)
		}
	}
	others := []string{holdHeldPhase, holdPause, holdPending, holdCatchingUp}
	for name := range cases {
		if !slices.Contains(record.HeldReasons, record.HeldReason(name)) && !slices.Contains(others, name) {
			t.Errorf("the case %q is no word of record.HeldReasons and none of the other holds %q", name, others)
		}
	}

	dsn := replayDSN(t)
	rule := mainRule(t)
	for _, name := range slices.Sorted(maps.Keys(cases)) {
		hold := cases[name]
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			window := hold.window(t)
			result, err := Run(context.Background(), RunConfig{Window: window, Rule: rule, Branch: MainBranch, Seed: 1, Slowdown: 1,
				PGDSN: dsn, Out: t.TempDir(), TraceSegments: true})
			if err != nil {
				t.Fatalf("replay the window of the hold %q under the rule main: %v", name, err)
			}
			hold.check(t, result)
			if hold.pieces {
				checkRecordedStarts(t, window, result)
			}
			if hold.timelines != nil {
				checkTimelines(t, result)
				hold.timelines(t, window, result)
			}
		})
	}
}

// check holds a replay of the case's window to what the case says it must show.
func (c holdCase) check(t *testing.T, result RunResult) {
	t.Helper()
	if c.unplaced {
		for _, unplaced := range result.Unplaced {
			if unplaced.Issue == c.issue {
				if unplaced.Reason == "" {
					t.Errorf("the replay could not place %s at the window's start and says no reason", c.issue)
				}
				return
			}
		}
		t.Errorf("the replay's report of the issues its start state could not place is %v: it must name %s, which stands in a hold no held word is true of at the window's start",
			result.Unplaced, c.issue)
		return
	}
	var starts []time.Time
	for _, segment := range result.Segments {
		if segment.Issue == c.issue {
			starts = append(starts, segment.Start)
		}
	}
	slices.SortFunc(starts, time.Time.Compare)
	after := func(at time.Time) bool { return !at.Before(c.held) }
	if c.worksOn {
		if !slices.ContainsFunc(starts, func(at time.Time) bool { return at.Equal(c.held) }) {
			t.Errorf("%s's segments start at %v, and none at %s: the hold takes its slot and stops none of its work, so its work goes on from then",
				c.issue, starts, c.held.Format(time.RFC3339))
		}
		after = func(at time.Time) bool { return at.After(c.held) }
	}
	next := slices.IndexFunc(starts, after)
	if next < 0 {
		t.Errorf("%s's segments start at %v, and none from %s on: its next start is recorded at %s, the end of the hold",
			c.issue, starts, c.held.Format(time.RFC3339), c.starts.Format(time.RFC3339))
		return
	}
	if !starts[next].Equal(c.starts) {
		t.Errorf("%s's next start from %s is at %s, want %s: the hold begins at %s and ends at %s in the record, and its start waits exactly that long",
			c.issue, c.held.Format(time.RFC3339), starts[next].Format(time.RFC3339), c.starts.Format(time.RFC3339),
			c.held.Format(time.RFC3339), c.starts.Format(time.RFC3339))
	}
}

// treeClosedWindow holds tree_closed. ACME-1, a root, is promoted at 09:05 and its architect
// writes the design; ACME-2, its child, is filed todo at 09:10 and waits for the gate. At 09:30 a
// person closes ACME-1: it leaves the workflow and lingers for two hours, and from then ACME-2
// stands in a closed tree. At 11:30 the linger closes the tree and hands ACME-2, still todo, to
// Legion (a done root hands over every member left in the workflow); the daemon's write of it
// todo with the label makes it a tree of its own, which the pass promotes at once.
func treeClosedWindow(t *testing.T) Window {
	b := newWindowBuilder(t, clock(9, 0), clock(15, 0))
	b.boot(clock(9, 0))
	b.file(clock(9, 5), "ACME-1", filing{Title: "Export the widget list", Status: "todo", Rank: "a0", Labels: handedToLegion})
	b.rootPromoted(clock(9, 5), "ACME-1")
	b.file(clock(9, 10), "ACME-2", filing{Title: "Write the export's header row", Parent: "ACME-1", Tree: "ACME-1", Status: "todo", Rank: "a1"})
	b.edit(clock(9, 30), "ACME-1", setStatus(record.DoneStatus))
	b.leaves(clock(9, 30), "ACME-1", record.DoneStatus)
	b.lingerCloses(clock(11, 30), "ACME-1", 2)
	b.info(clock(11, 30), "workflow: a finished root hands its unfinished child to Legion", "issue", "ACME-2", "tree", "ACME-1",
		"phase", string(phase.Admitted), "status", "todo")
	b.handsOver(clock(11, 30), "ACME-2")
	b.info(clock(11, 30), "admission hand-over", "issue", "ACME-2", "tree", "ACME-1", "linger", 1)
	b.ownTree("ACME-2")
	b.rootPromoted(clock(11, 30), "ACME-2")
	return b.build()
}

// treeParkedWindow holds tree_parked. As treeClosedWindow to 09:30, where a person moves ACME-1 to
// backlog instead: the root lingers for two hours, its tree closed, and at 11:30 the linger closes
// a tree whose root is parked, which hands over no member at admitted, so ACME-2 stands in a
// parked tree from then. At 12:30 a person puts the legion label on ACME-2, todo under a tree that
// is not live, which makes it an orphan the pass takes as a tree of its own and promotes.
func treeParkedWindow(t *testing.T) Window {
	b := newWindowBuilder(t, clock(9, 0), clock(15, 0))
	b.boot(clock(9, 0))
	b.file(clock(9, 5), "ACME-1", filing{Title: "Export the widget list", Status: "todo", Rank: "a0", Labels: handedToLegion})
	b.rootPromoted(clock(9, 5), "ACME-1")
	b.file(clock(9, 10), "ACME-2", filing{Title: "Write the export's header row", Parent: "ACME-1", Tree: "ACME-1", Status: "todo", Rank: "a1"})
	b.edit(clock(9, 30), "ACME-1", setStatus("backlog"))
	b.leaves(clock(9, 30), "ACME-1", "backlog")
	b.lingerCloses(clock(11, 30), "ACME-1", 2)
	b.edit(clock(12, 30), "ACME-2", labelled)
	b.info(clock(12, 30), "admission orphan", "issue", "ACME-2", "parent", "ACME-1")
	b.ownTree("ACME-2")
	b.rootPromoted(clock(12, 30), "ACME-2")
	return b.build()
}

// labelOffWindow holds label_off, under a cap of one. ACME-1 takes the one slot at 09:05; ACME-2,
// a root handed to Legion at 09:10, waits for it. At 09:20 a person takes ACME-2's label off, and
// at 09:40 closes ACME-1, whose slot comes free: from then nothing but the label keeps ACME-2 from
// it. At 10:40 the person puts the label back, and the pass promotes ACME-2 at once.
func labelOffWindow(t *testing.T) Window {
	b := newWindowBuilder(t, clock(9, 0), clock(15, 0))
	b.capped(1)
	b.boot(clock(9, 0))
	b.file(clock(9, 5), "ACME-1", filing{Title: "Export the widget list", Status: "todo", Rank: "a0", Labels: handedToLegion})
	b.rootPromoted(clock(9, 5), "ACME-1")
	b.file(clock(9, 10), "ACME-2", filing{Title: "Count the widgets per shelf", Status: "todo", Rank: "a1", Labels: handedToLegion})
	b.edit(clock(9, 20), "ACME-2", unlabelled)
	b.edit(clock(9, 40), "ACME-1", setStatus(record.DoneStatus))
	b.leaves(clock(9, 40), "ACME-1", record.DoneStatus)
	b.edit(clock(10, 40), "ACME-2", labelled)
	b.rootPromoted(clock(10, 40), "ACME-2")
	b.lingerCloses(clock(11, 40), "ACME-1", 1)
	return b.build()
}

// ancestryWindow holds ancestry. ACME-1, a root, is promoted at 09:05; its architect files ACME-3
// under it in backlog at 09:06, unreleased. At 09:10 a person hands ACME-2 to Legion, todo under
// ACME-3: no recorded parent takes it as a child, so the pass records it as a root of its own,
// whose Dispatch chain names ACME-1, a live root, and it stands as that tree's unreleased work. At
// 10:10 a person closes ACME-1: its tree lingers, no chain names a live root any longer, and the
// pass promotes ACME-2 in the same pass.
func ancestryWindow(t *testing.T) Window {
	b := newWindowBuilder(t, clock(9, 0), clock(15, 0))
	b.boot(clock(9, 0))
	b.file(clock(9, 5), "ACME-1", filing{Title: "Export the widget list", Status: "todo", Rank: "a0", Labels: handedToLegion})
	b.rootPromoted(clock(9, 5), "ACME-1")
	b.file(clock(9, 6), "ACME-3", filing{Title: "Agree the export's columns", Parent: "ACME-1", Tree: "ACME-1", Status: "backlog", Rank: "a2"})
	b.file(clock(9, 10), "ACME-2", filing{Title: "Export the archived widgets", Parent: "ACME-3", Status: "todo", Rank: "a1", Labels: handedToLegion})
	b.info(clock(9, 10), "admission orphan", "issue", "ACME-2", "parent", "ACME-3")
	b.edit(clock(10, 10), "ACME-1", setStatus(record.DoneStatus))
	b.leaves(clock(10, 10), "ACME-1", record.DoneStatus)
	b.rootPromoted(clock(10, 10), "ACME-2")
	b.lingerCloses(clock(12, 10), "ACME-1", 1)
	return b.build()
}

// approvalAsk is the id of the approval request rootAwaitingPersonWindow's architect opens.
const approvalAsk = "00000000-0000-4000-8000-0000000000b1"

// rootAwaitingPersonWindow holds root_awaiting_person, in a deployment that gates the spec.
// ACME-1 is promoted at 09:05; ACME-2, its child, is filed todo at 09:10 and waits for the gate.
// ACME-1's architect writes version 1 of the spec at 09:17, asks a person to approve it at 09:18
// and registers it at 09:20: the registration hands the document to the person, and the root
// waits on them, keeping its slot, from then. At 10:20 the person approves it, which opens the
// gate and answers the wait in one fact: the pass resumes the root's architect and starts ACME-2.
func rootAwaitingPersonWindow(t *testing.T) Window {
	b := newWindowBuilder(t, clock(9, 0), clock(15, 0))
	rootAwaitingPerson(b, b.session("ACME-1", claim.RoleArchitect), false, nil)
	return b.build()
}

// rootAwaitingPerson writes rootAwaitingPersonWindow's record into b, its root architect's session
// named architect. Where claimed, that session claims ACME-1 in Dispatch at 09:06; and waiting,
// where set, writes what comes while the root waits, after the wait's start at 09:20 and before the
// person's approval at 10:20.
func rootAwaitingPerson(b *windowBuilder, architect eventActor, claimed bool, waiting func()) {
	b.t.Helper()
	b.gated(record.DesignStepSpec)
	b.boot(clock(9, 0))
	b.file(clock(9, 5), "ACME-1", filing{Title: "Export the widget list", Status: "todo", Rank: "a0", Labels: handedToLegion})
	b.rootPromoted(clock(9, 5), "ACME-1")
	if claimed {
		b.claimedBy(clock(9, 6), "ACME-1", architect)
	}
	b.file(clock(9, 10), "ACME-2", filing{Title: "Write the export's header row", Parent: "ACME-1", Tree: "ACME-1", Status: "todo", Rank: "a1"})
	b.specWrittenBy(clock(9, 17), "ACME-1", architect)
	b.asksApprovalBy(clock(9, 18), "ACME-1", approvalAsk, architect)
	b.turnEnds(clock(9, 20), "ACME-1", claim.RoleArchitect)
	b.registered(clock(9, 20), "ACME-1", false)
	since := b.seq("ACME-1")
	b.info(clock(9, 20), "workflow: a registration hands a design document to the person", "issue", "ACME-1",
		"step", string(record.DesignStepSpec), "version", 1, "since_seq", since)
	b.info(clock(9, 20), "workflow: root waits on a person and keeps its admission slot", "issue", "ACME-1", "tree", "ACME-1",
		"reason", "spec version 1 is awaiting a person's review", "since_seq", since)
	if waiting != nil {
		waiting()
	}
	b.approves(clock(10, 20), "ACME-1", approvalAsk)
	b.gateOpened(clock(10, 20), "ACME-1", intake.DispatchArtifactApproved, 1)
	b.info(clock(10, 20), "workflow: a person's review or a newer spec version answered a root's wait", "issue", "ACME-1",
		"version", 1, "event", "approved", "ask", "", "waited_version", 1)
	b.turnStarts(clock(10, 20), "ACME-1", claim.RoleArchitect)
	b.childStarts(clock(10, 20), "ACME-2")
	b.turnEnds(clock(10, 35), "ACME-1", claim.RoleArchitect)
}

// designGateWindow holds design_gate, in a deployment that gates the spec. ACME-1 is promoted at
// 09:05; ACME-2, its child, is filed todo at 09:10, and its tree's gate holds no approved spec.
// ACME-1's architect writes version 1 at 09:17 and registers it at 09:20 with no approval request,
// so nothing is handed to a person and the root waits on no one. At 10:20 a person approves the
// version: the gate opens, ACME-1 waits for its open child (children first) and gives its slot
// back, ACME-2 starts, and the architect is told of the approval.
func designGateWindow(t *testing.T) Window {
	b := newWindowBuilder(t, clock(9, 0), clock(15, 0))
	b.gated(record.DesignStepSpec)
	b.boot(clock(9, 0))
	b.file(clock(9, 5), "ACME-1", filing{Title: "Export the widget list", Status: "todo", Rank: "a0", Labels: handedToLegion})
	b.rootPromoted(clock(9, 5), "ACME-1")
	b.file(clock(9, 10), "ACME-2", filing{Title: "Write the export's header row", Parent: "ACME-1", Tree: "ACME-1", Status: "todo", Rank: "a1"})
	b.specWritten(clock(9, 17), "ACME-1")
	b.turnEnds(clock(9, 20), "ACME-1", claim.RoleArchitect)
	b.registered(clock(9, 20), "ACME-1", false)
	b.approves(clock(10, 20), "ACME-1", "")
	b.waitsForChildren(clock(10, 20), "ACME-1", 1)
	b.gateOpened(clock(10, 20), "ACME-1", intake.DispatchArtifactApproved, 1)
	b.slotReleased(clock(10, 20), "ACME-1")
	b.childStarts(clock(10, 20), "ACME-2")
	b.turnStarts(clock(10, 20), "ACME-1", claim.RoleArchitect)
	b.turnEnds(clock(10, 30), "ACME-1", claim.RoleArchitect)
	return b.build()
}

// dispatchBlockerWindow holds dispatch_blocker. ACME-1, a root, is promoted at 09:05 and plans from
// 09:20. ACME-3, an issue Legion is not given, stands in backlog from 09:06. At 09:30 the architect
// files ACME-2 under ACME-1, todo and blocked by ACME-3: its gate is open and a slot is free, and
// the unfinished link alone holds it. At 10:30 a person closes ACME-3, and ACME-2 starts.
func dispatchBlockerWindow(t *testing.T) Window {
	b := newWindowBuilder(t, clock(9, 0), clock(15, 0))
	b.boot(clock(9, 0))
	b.file(clock(9, 5), "ACME-1", filing{Title: "Export the widget list", Status: "todo", Rank: "a0", Labels: handedToLegion})
	b.rootPromoted(clock(9, 5), "ACME-1")
	b.file(clock(9, 6), "ACME-3", filing{Title: "Agree the shelf names with the warehouse", Status: "backlog", Rank: "a3"})
	b.rootDesigned(clock(9, 20), "ACME-1")
	b.file(clock(9, 30), "ACME-2", filing{Title: "Write the export's header row", Parent: "ACME-1", Tree: "ACME-1", Status: "todo", Rank: "a1",
		BlockedBy: []string{"ACME-3"}})
	b.edit(clock(10, 30), "ACME-3", setStatus(record.DoneStatus))
	b.childStarts(clock(10, 30), "ACME-2", "ACME-3")
	return b.build()
}

// childrenFirstWindow holds children_first. ACME-1, a root, is promoted at 09:05; ACME-2 is filed
// todo under it at 09:10 and ACME-3 todo under ACME-2 at 09:12. At 09:20 ACME-1's design opens its
// gate: ACME-1 and ACME-2 each wait for the open child under them (children first), ACME-1 giving
// its slot back, and ACME-3, which has none, starts. At 10:20 a person moves ACME-3 to icebox: it
// leaves the workflow, no open child stands under ACME-2 any longer, and ACME-2 starts.
func childrenFirstWindow(t *testing.T) Window {
	b := newWindowBuilder(t, clock(9, 0), clock(15, 0))
	b.boot(clock(9, 0))
	b.file(clock(9, 5), "ACME-1", filing{Title: "Export the widget list", Status: "todo", Rank: "a0", Labels: handedToLegion})
	b.rootPromoted(clock(9, 5), "ACME-1")
	b.file(clock(9, 10), "ACME-2", filing{Title: "Write the export's header row", Parent: "ACME-1", Tree: "ACME-1", Status: "todo", Rank: "a1"})
	b.file(clock(9, 12), "ACME-3", filing{Title: "Name the header row's columns", Parent: "ACME-2", Tree: "ACME-1", Status: "todo", Rank: "a2"})
	b.turnEnds(clock(9, 20), "ACME-1", claim.RoleArchitect)
	b.registered(clock(9, 20), "ACME-1", true)
	b.waitsForChildren(clock(9, 20), "ACME-1", 1)
	b.waitsForChildren(clock(9, 20), "ACME-2", 1)
	b.slotReleased(clock(9, 20), "ACME-1")
	b.childStarts(clock(9, 20), "ACME-3")
	b.edit(clock(10, 20), "ACME-3", setStatus("icebox"))
	b.leaves(clock(10, 20), "ACME-3", "icebox")
	b.childStarts(clock(10, 20), "ACME-2")
	return b.build()
}

// acceptedAncestorWindow holds accepted_ancestor. ACME-1, a root, runs from its promotion at 09:05
// through planning, implementing, testing and its review, is signed off at 11:20 and works its
// retro. At 11:30 a person files ACME-2 under it, todo: ACME-1 stands accepted, so nothing of its
// tree may start beneath it. At 12:30 ACME-1's retro sends it back to implementing, it stands
// accepted no longer, and ACME-2 starts.
func acceptedAncestorWindow(t *testing.T) Window {
	b := newWindowBuilder(t, clock(9, 0), clock(15, 0))
	b.boot(clock(9, 0))
	b.file(clock(9, 5), "ACME-1", filing{Title: "Export the widget list", Status: "todo", Rank: "a0", Labels: handedToLegion})
	b.rootPromoted(clock(9, 5), "ACME-1")
	b.rootDesigned(clock(9, 20), "ACME-1")
	b.segmentEnds(clock(9, 50), "ACME-1", phase.Planning, phase.Implementing, workflow.TriggerPlannerCompleted)
	b.segmentEnds(clock(10, 20), "ACME-1", phase.Implementing, phase.Testing, workflow.TriggerImplementationReady)
	b.segmentEnds(clock(10, 40), "ACME-1", phase.Testing, phase.Reviewing, workflow.TriggerTesterPassed)
	b.segmentEnds(clock(11, 0), "ACME-1", phase.Reviewing, phase.Accepting, workflow.TriggerReviewApproved)
	b.segmentEnds(clock(11, 20), "ACME-1", phase.Accepting, phase.Retro, workflow.TriggerSignOff)
	b.file(clock(11, 30), "ACME-2", filing{Title: "Write the export's header row", Parent: "ACME-1", Tree: "ACME-1", Status: "todo", Rank: "a1"})
	b.segmentEnds(clock(12, 30), "ACME-1", phase.Retro, phase.Implementing, workflow.TriggerBackward)
	b.childStarts(clock(12, 30), "ACME-2")
	return b.build()
}

// atWorkWindow holds at_work, under a cap of one. ACME-1 takes the one slot at 09:05 and runs to
// its review; ACME-2, a root handed to Legion at 09:10, waits for it. At 11:00 ACME-1's review
// approves it into accepting, which keeps no slot under main: the slot goes to ACME-2, and ACME-1,
// at work past its review with no slot, has its sign-off from its architect at 11:30, as recorded,
// and its retro starts then on no slot, though the cap is full.
func atWorkWindow(t *testing.T) Window {
	b := newWindowBuilder(t, clock(9, 0), clock(15, 0))
	b.capped(1)
	b.boot(clock(9, 0))
	b.file(clock(9, 5), "ACME-1", filing{Title: "Export the widget list", Status: "todo", Rank: "a0", Labels: handedToLegion})
	b.rootPromoted(clock(9, 5), "ACME-1")
	b.file(clock(9, 10), "ACME-2", filing{Title: "Count the widgets per shelf", Status: "todo", Rank: "a1", Labels: handedToLegion})
	b.rootDesigned(clock(9, 20), "ACME-1")
	b.segmentEnds(clock(9, 50), "ACME-1", phase.Planning, phase.Implementing, workflow.TriggerPlannerCompleted)
	b.segmentEnds(clock(10, 20), "ACME-1", phase.Implementing, phase.Testing, workflow.TriggerImplementationReady)
	b.segmentEnds(clock(10, 40), "ACME-1", phase.Testing, phase.Reviewing, workflow.TriggerTesterPassed)
	b.segmentEnds(clock(11, 0), "ACME-1", phase.Reviewing, phase.Accepting, workflow.TriggerReviewApproved)
	b.rootPromoted(clock(11, 0), "ACME-2")
	b.segmentEnds(clock(11, 30), "ACME-1", phase.Accepting, phase.Retro, workflow.TriggerSignOff)
	return b.build()
}

// unclassifiedWindow holds unclassified at the window's start, 09:00, its inputs reaching four
// hours back. ACME-1, a root, ran from its promotion at 05:05 to merging at 07:40, and at 08:00
// its merger's claim failed: it stands held from merging, which keeps no slot, with its work
// stopped, so neither at_work nor any other word is true of it, which the tick of 08:01 logs.
// Nothing happens in the window itself.
func unclassifiedWindow(t *testing.T) Window {
	b := newWindowBuilder(t, clock(9, 0), clock(12, 0))
	b.reaches(4 * time.Hour)
	b.boot(clock(5, 0))
	b.file(clock(5, 5), "ACME-1", filing{Title: "Export the widget list", Status: "todo", Rank: "a0", Labels: handedToLegion})
	b.rootPromoted(clock(5, 5), "ACME-1")
	b.rootDesigned(clock(5, 20), "ACME-1")
	b.segmentEnds(clock(5, 50), "ACME-1", phase.Planning, phase.Implementing, workflow.TriggerPlannerCompleted)
	b.segmentEnds(clock(6, 20), "ACME-1", phase.Implementing, phase.Testing, workflow.TriggerImplementationReady)
	b.segmentEnds(clock(6, 40), "ACME-1", phase.Testing, phase.Reviewing, workflow.TriggerTesterPassed)
	b.segmentEnds(clock(7, 0), "ACME-1", phase.Reviewing, phase.Accepting, workflow.TriggerReviewApproved)
	b.segmentEnds(clock(7, 20), "ACME-1", phase.Accepting, phase.Retro, workflow.TriggerSignOff)
	b.segmentEnds(clock(7, 40), "ACME-1", phase.Retro, phase.Merging, workflow.TriggerRetroCompleted)
	b.claimFailed(clock(8, 0), "ACME-1", claim.RoleMerger)
	b.info(clock(8, 1), "admission: the record holds an issue no held word is true of", "issue", "ACME-1",
		"phase", string(phase.Held), "status", "retro", "reason", string(record.HeldUnclassified))
	return b.build()
}

// heldPhaseWindow holds the phase held. ACME-1, a root, is promoted at 09:05, plans from 09:20 and
// implements from 09:50. At 10:20 its implementer's claim fails, which holds it from implementing,
// keeping its slot. At 11:20 its architect retries the phase: the hold is released and the
// implementer is launched again.
func heldPhaseWindow(t *testing.T) Window {
	b := newWindowBuilder(t, clock(9, 0), clock(15, 0))
	b.boot(clock(9, 0))
	b.file(clock(9, 5), "ACME-1", filing{Title: "Export the widget list", Status: "todo", Rank: "a0", Labels: handedToLegion})
	b.rootPromoted(clock(9, 5), "ACME-1")
	b.rootDesigned(clock(9, 20), "ACME-1")
	b.segmentEnds(clock(9, 50), "ACME-1", phase.Planning, phase.Implementing, workflow.TriggerPlannerCompleted)
	b.claimFailed(clock(10, 20), "ACME-1", claim.RoleImplementer)
	b.info(clock(11, 20), "workflow: a held phase is released", "issue", "ACME-1", "phase", string(phase.Implementing), "reason", "retry held phase")
	b.launched(clock(11, 20), "ACME-1", claim.RoleImplementer, true)
	b.turnStarts(clock(11, 20), "ACME-1", claim.RoleImplementer)
	return b.build()
}

// pauseWindow holds a project's pause. The operator pauses the project at 09:20; ACME-1, a root,
// is handed to Legion at 09:40 and promoted by no pass while the pause stands. The operator
// resumes the project at 10:40, and the resume's own pass promotes ACME-1.
func pauseWindow(t *testing.T) Window {
	b := newWindowBuilder(t, clock(9, 0), clock(15, 0))
	b.boot(clock(9, 0))
	b.info(clock(9, 20), "pause: the project is paused", "pause", 1, "by", "operator", "by_session", "",
		"reason", "the operator moves the database to a larger disk", "stops", 0, "claims", []string{}, "told_controller", true)
	b.file(clock(9, 40), "ACME-1", filing{Title: "Export the widget list", Status: "todo", Rank: "a0", Labels: handedToLegion})
	b.info(clock(10, 40), "pause: the project is resumed", "pause", 1, "by", "operator", "by_session", "",
		"paused_by", "operator", "paused_by_session", "", "held", 0, "told_controller", true)
	b.rootPromoted(clock(10, 40), "ACME-1")
	return b.build()
}

// pendingWindow holds a boot's pending summaries. The daemon booted at 09:00 stops at 09:30;
// ACME-1, a root, is handed to Legion at 09:40 while no daemon runs. The daemon boots again at
// 10:00: its listing shows ACME-1 ahead of the record, the Dispatch consumer has not reached the
// stream's position, and admission holds the summary. At 10:10 the consumer catches up, the hold
// releases, and the release's pass promotes ACME-1.
func pendingWindow(t *testing.T) Window {
	b := newWindowBuilder(t, clock(9, 0), clock(15, 0))
	b.boot(clock(9, 0))
	b.stopped(clock(9, 30))
	b.file(clock(9, 40), "ACME-1", filing{Title: "Export the widget list", Status: "todo", Rank: "a0", Labels: handedToLegion})
	b.boot(clock(10, 0))
	b.info(clock(10, 0), reconcileHoldingLine, "count", 1, "target", 2, "ack_floor", 1)
	b.info(clock(10, 10), consumerCaughtUpLine, "count", 1, "ack_floor", 2, "idle", true, "admitted", 1)
	b.rootPromoted(clock(10, 10), "ACME-1")
	return b.build()
}

// catchingUpWindow holds a boot's roots catching up, in a deployment that gates the spec. ACME-1,
// a root, is promoted at 09:05, and its architect registers version 1 of the spec at 09:25 with no
// approval request. The daemon stops at 09:30, and a person approves the version at 09:40 while no
// daemon runs. The daemon boots again at 10:00 and holds ACME-1, a root whose design its architect
// may still hand a person, until its catch-up has read its Dispatch events: at 10:02 the catch-up
// applies the approval, the gate opens, and once the catch-up releases the root its first phase
// starts.
func catchingUpWindow(t *testing.T) Window {
	b := newWindowBuilder(t, clock(9, 0), clock(15, 0))
	b.gated(record.DesignStepSpec)
	b.boot(clock(9, 0))
	b.file(clock(9, 5), "ACME-1", filing{Title: "Export the widget list", Status: "todo", Rank: "a0", Labels: handedToLegion})
	b.rootPromoted(clock(9, 5), "ACME-1")
	b.specWritten(clock(9, 17), "ACME-1")
	b.turnEnds(clock(9, 25), "ACME-1", claim.RoleArchitect)
	b.registered(clock(9, 25), "ACME-1", false)
	b.stopped(clock(9, 30))
	b.approves(clock(9, 40), "ACME-1", "")
	b.boot(clock(10, 0))
	b.gateOpened(clock(10, 2), "ACME-1", intake.DispatchArtifactApproved, 1)
	b.rootFirstPhase(clock(10, 2), "ACME-1")
	b.turnStarts(clock(10, 2), "ACME-1", claim.RoleArchitect)
	b.turnEnds(clock(10, 10), "ACME-1", claim.RoleArchitect)
	return b.build()
}
