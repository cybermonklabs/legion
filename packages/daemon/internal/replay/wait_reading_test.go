package replay

import (
	"context"
	"math"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// doneBlockerWindow is a window from 09:00 to 13:00, its inputs reaching back to the daemon's boot
// at 06:00, in which an issue in flight that the daemon has not started names a blocker already
// done, and a todo child names that issue as a blocker of its own. ACME-3, an issue Legion is not
// given, is filed in backlog at 06:01 and closed at 06:03. ACME-1, a root handed to Legion and
// blocked by ACME-3, is filed and promoted at 06:05, plans from 06:20 and implements from 06:50;
// its architect files ACME-2 under it at 06:30, todo and blocked by ACME-1. ACME-1's implementer's
// turn ends at 08:30 with no move, and at 08:40 a pass takes its slot back with no wait. Nothing
// happens in the window itself: ACME-1 holds no slot, owes no start and carries no mark, so the
// daemon judges no wait of it (blockers' startedIssues), and ACME-2 waits behind it.
func doneBlockerWindow(t *testing.T) Window {
	b := newWindowBuilder(t, clock(9, 0), clock(13, 0))
	b.reaches(3 * time.Hour)
	b.boot(clock(6, 0))
	b.file(clock(6, 1), "ACME-3", filing{Title: "Agree the shelf names with the warehouse", Status: "backlog", Rank: "a3"})
	b.edit(clock(6, 3), "ACME-3", setStatus(record.DoneStatus))
	b.file(clock(6, 5), "ACME-1", filing{Title: "Ship the ACME-1 change", Status: "todo", Rank: "a1", Labels: handedToLegion,
		BlockedBy: []string{"ACME-3"}})
	b.rootPromoted(clock(6, 5), "ACME-1")
	b.rootDesigned(clock(6, 20), "ACME-1")
	b.filedBy(clock(6, 30), "ACME-2", filing{Title: "Ship the ACME-2 change", Parent: "ACME-1", Tree: "ACME-1", Status: "todo", Rank: "a2",
		BlockedBy: []string{"ACME-1"}}, b.session("ACME-1", claim.RoleArchitect))
	b.segmentEnds(clock(6, 50), "ACME-1", phase.Planning, phase.Implementing, workflow.TriggerPlannerCompleted)
	b.turnEnds(clock(8, 30), "ACME-1", claim.RoleImplementer)
	b.slotReleased(clock(8, 40), "ACME-1")
	return b.build()
}

// gateVersionWindow is a window from 09:00 to 13:00, in a deployment that gates the spec, in which
// a root's children wait ends with its gate closed by a new spec version and another document of
// the root is approved before a person approves that version. ACME-1, a root, is promoted at 09:05;
// ACME-2, its child, is filed todo at 09:10; the architect writes version 1 of the spec at 09:17 and
// registers it at 09:20, not approved. At 10:20 a person approves it: the gate opens, ACME-1 waits
// for ACME-2, gives its slot back and ACME-2 starts. At 11:00 the architect writes version 2: the
// gate closes, the wait ends and ACME-1 takes a slot back. At 11:30 the architect approves its plan,
// a document the deployment gates no step of, which opens nothing (workflow's applyArtifact). At
// 12:00 a person approves version 2: the gate opens, and ACME-1 waits for ACME-2 again, which the
// daemon tells no line of, having told the wait of this generation, and gives its slot back.
func gateVersionWindow(t *testing.T) Window {
	b := newWindowBuilder(t, clock(9, 0), clock(13, 0))
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

	b.specRevised(clock(11, 0), "ACME-1", 2)
	b.gateClosed(clock(11, 0), "ACME-1", intake.DispatchArtifactVersion, 2)
	b.waitEnded(clock(11, 0), "ACME-1", phase.Admitted)
	plan := "00000000-0000-4000-8000-0000000000c1"
	architect := b.session("ACME-1", claim.RoleArchitect)
	b.event(clock(11, 30), "ACME-1", "artifact.approved", architect, map[string]any{
		"actor": map[string]string{"id": architect.ID, "kind": architect.Kind}, "artifact_id": plan, "ask_id": nil, "name": "plan.md",
		"reason": nil, "version": 1,
	})
	b.approvesVersion(clock(12, 0), "ACME-1", "", 2)
	b.gateOpened(clock(12, 0), "ACME-1", intake.DispatchArtifactApproved, 2)
	b.slotReleased(clock(12, 0), "ACME-1")
	b.turnStarts(clock(12, 0), "ACME-1", claim.RoleArchitect)
	b.turnEnds(clock(12, 10), "ACME-1", claim.RoleArchitect)
	return b.build()
}

// touchingWaitsWindow is a window from 09:00 to 12:00 in which a root's wait on a link ends at the
// moment its wait on a child begins. ACME-1, a root, is promoted at 09:05; ACME-3, an issue Legion
// is not given, stands in backlog from 09:06, and at 09:10 a person links ACME-1 as blocked by it.
// At 09:20 the architect registers the design, its gate open: ACME-1 waits on ACME-3 and gives its
// slot back. At 10:00 the person files ACME-2 under ACME-1 in backlog and closes ACME-3, in that
// order, so no pass finds ACME-1 waiting on nothing: its wait on the link ends as its wait for that
// child begins. At 11:00 the person closes ACME-2: the wait ends, and ACME-1 takes a slot back and
// starts its own first phase.
func touchingWaitsWindow(t *testing.T) Window {
	b := newWindowBuilder(t, clock(9, 0), clock(12, 0))
	b.boot(clock(9, 0))
	b.file(clock(9, 5), "ACME-1", filing{Title: "Export the widget list", Status: "todo", Rank: "a0", Labels: handedToLegion})
	b.rootPromoted(clock(9, 5), "ACME-1")
	b.file(clock(9, 6), "ACME-3", filing{Title: "Agree the shelf names with the warehouse", Status: "backlog", Rank: "a3"})
	b.edit(clock(9, 10), "ACME-1", func(issue *builtIssue) { issue.BlockedBy = []string{"ACME-3"} })
	b.turnEnds(clock(9, 20), "ACME-1", claim.RoleArchitect)
	b.registered(clock(9, 20), "ACME-1", true)
	b.slotReleased(clock(9, 20), "ACME-1")
	b.file(clock(10, 0), "ACME-2", filing{Title: "Write the export's header row", Parent: "ACME-1", Status: "backlog", Rank: "a1"})
	b.edit(clock(10, 0), "ACME-3", setStatus(record.DoneStatus))
	b.waitsForChildren(clock(10, 0), "ACME-1", 1)
	b.edit(clock(11, 0), "ACME-2", setStatus(record.DoneStatus))
	b.waitEnded(clock(11, 0), "ACME-1", phase.Admitted)
	b.rootFirstPhase(clock(11, 0), "ACME-1")
	return b.build()
}

// toldBeforeStartWindow is a window from 10:00 to 13:00, its inputs reaching back to the daemon's
// boot at 08:00, in which a root's children wait the daemon told before the start ends in the
// window. ACME-1, a root, is promoted at 08:05, and its architect files ACME-2 under it in triage at
// 08:10, which the daemon releases at 08:11. At 08:20 the architect registers the design, its gate
// open: ACME-1 waits for its open child, which the daemon tells, and gives its slot back, and ACME-2
// takes a slot and plans. At 11:00 a person closes ACME-2: the wait ends, and ACME-1 takes a slot
// back and starts its own first phase.
func toldBeforeStartWindow(t *testing.T) Window {
	b := newWindowBuilder(t, clock(10, 0), clock(13, 0))
	b.reaches(2 * time.Hour)
	b.boot(clock(8, 0))
	promotedRoot(b, clock(8, 5), "ACME-1", "a1")
	b.filedBy(clock(8, 10), "ACME-2", filing{Title: "Ship the ACME-2 change", Parent: "ACME-1", Tree: "ACME-1", Status: "triage", Rank: "a2"},
		b.session("ACME-1", claim.RoleArchitect))
	b.daemonWrites(clock(8, 11), "ACME-2", "todo")
	b.rootWaitsForChildren(clock(8, 20), "ACME-1", 1)
	b.childStarts(clock(8, 20), "ACME-2")
	b.edit(clock(11, 0), "ACME-2", setStatus(record.DoneStatus))
	b.leaves(clock(11, 0), "ACME-2", record.DoneStatus)
	b.waitEnded(clock(11, 0), "ACME-1", phase.Admitted)
	b.rootFirstPhase(clock(11, 0), "ACME-1")
	return b.build()
}

// A wait that begins where another ends is one stretch of waiting (issueWaits), as the snapshot
// timeline reads it: in touchingWaitsWindow ACME-1's link wait ends with its blocker's done at 10:00,
// the moment its wait for the child filed then begins, and the record's scorecard counts one wait,
// from 09:10 to the child's close at 11:00, ended by it: child_done for 1 hour 50 minutes.
func TestAWaitThatBeginsWhereAnotherEndsIsOneWait(t *testing.T) {
	t.Parallel()
	card, err := Score(touchingWaitsWindow(t))
	if err != nil {
		t.Fatalf("score the record: %v", err)
	}
	done := card.Waits[string(WaitChildDone)]
	if len(card.Waits) != 1 || done.Count != 1 || math.Abs(done.Hours-11.0/6) > fixtureTolerance {
		t.Errorf("the record counts the waits %+v, want one wait of child_done for 11/6 hours", card.Waits)
	}
}

// A gate is read from the daemon's lines of its flips where the window holds them, and otherwise
// from its registrations and the verdicts on the document and at the version they name, as
// record.GateOpen opens a step (gateReader): an approval of another document, or of an older
// version, opens nothing, and with the lines no verdict moves the gate at all.
func TestAVerdictMovesTheGateOnlyOnItsDocumentAtItsVersion(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name  string
		lines bool
		facts []GateFact
		want  []bool
	}{
		{name: "without the lines", facts: []GateFact{
			{Kind: GateRegisteredFact, Artifact: "spec", Version: 1},
			{Kind: GateApproved, Artifact: "plan", Version: 1},
			{Kind: GateApproved, Artifact: "spec", Version: 2},
			{Kind: GateApproved, Artifact: "spec", Version: 1},
			{Kind: GateChangesRequested, Artifact: "plan", Version: 1},
			{Kind: GateRegisteredFact, Artifact: "spec", Version: 2},
			{Kind: GateChangesRequested, Artifact: "spec", Version: 2},
		}, want: []bool{false, false, false, true, true, false, false}},
		{name: "with the lines", lines: true, facts: []GateFact{
			{Kind: GateRegisteredFact, Artifact: "spec", Version: 1},
			{Kind: GateApproved, Artifact: "spec", Version: 1},
			{Kind: GateOpenedFact, Artifact: "spec", Version: 1},
			{Kind: GateChangesRequested, Artifact: "spec", Version: 1},
			{Kind: GateClosedFact, Artifact: "spec", Version: 2},
			{Kind: GateApproved, Artifact: "spec", Version: 2},
		}, want: []bool{false, false, true, true, false, false}},
	} {
		t.Run(c.name, func(t *testing.T) {
			gate := gateReader{lines: c.lines}
			for i, fact := range c.facts {
				if got := gate.apply(fact); got != c.want[i] {
					t.Errorf("after the %s of %s at version %d the gate stands open %v, want %v", fact.Kind, fact.Artifact, fact.Version, got, c.want[i])
				}
			}
		})
	}
}

// The start state marks the children waits the record told in its tree's current run before the
// start (childrenToldMarks), so the replay's daemon tells none of them again: in
// toldBeforeStartWindow the record told ACME-1's wait at 08:20, the start state at 10:00 holds the
// mark at ACME-1's generation, a second telling of it finds it told, and a replay of the window
// writes no line of that wait. Both of its timelines count one wait, from the start to ACME-2's
// close at 11:00: child_done for an hour.
func TestTheStartStateMarksAChildrenWaitTheRecordTold(t *testing.T) {
	t.Parallel()
	window := toldBeforeStartWindow(t)
	st := seedStore(t)
	_, report := seedReported(t, st, window, window.From, func(tx pgx.Tx) {
		told, err := record.NewStore().TellChildrenFirst(context.Background(), tx, "ACME-1", "ACME-1", 1, window.From)
		if err != nil {
			t.Fatalf("tell ACME-1's children wait: %v", err)
		}
		if told {
			t.Errorf("the start state holds no mark of ACME-1's children wait, which the record told at 08:20")
		}
	})
	if at, marked := report.ChildrenTold["ACME-1"]; !marked || !at.Equal(clock(8, 20)) {
		t.Errorf("the start state reports the children waits it marked told as %v, want ACME-1's at 08:20", report.ChildrenTold)
	}

	out := t.TempDir()
	result, err := Run(context.Background(), RunConfig{Window: window, Rule: record.MainRule, Branch: MainBranch, Seed: 1, Slowdown: 1,
		PGDSN: replayDSN(t), Out: out})
	if err != nil {
		t.Fatalf("replay the window: %v", err)
	}
	for _, line := range replayLog(t, out) {
		if line.Msg == waitsForChildrenLine && line.Issue == "ACME-1" {
			t.Errorf("the replay tells ACME-1's children wait again at %s, which the record told before the start", stamp(line.Time))
		}
	}
	assertTimelineWaits(t, result, map[WaitEnd]WaitHours{WaitChildDone: {Count: 1, Hours: 1}})
}
