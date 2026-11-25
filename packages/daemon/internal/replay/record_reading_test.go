package replay

import (
	"context"
	"maps"
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// replayMoves replays w under main on the main branch and answers each issue's moves in the
// replay's own log and the moves the daemon's code refused.
func replayMoves(t *testing.T, w Window) (map[string][]fixtureMove, []RefusedMove) {
	t.Helper()
	out := t.TempDir()
	result, err := Run(context.Background(), RunConfig{Window: w, Rule: mainRule(t), Branch: MainBranch, Seed: 1, Slowdown: 1,
		PGDSN: replayDSN(t), Out: out})
	if err != nil {
		t.Fatalf("replay the window under main: %v", err)
	}
	return replayedMoves(replayLog(t, out)), result.Divergence.Refused
}

// checkMoves fails for each issue whose moves in a replay differ from want, and for each move the
// replay refused.
func checkMoves(t *testing.T, moved, want map[string][]fixtureMove, refused []RefusedMove) {
	t.Helper()
	if len(refused) != 0 {
		t.Errorf("the replay refused the recorded moves %+v, want none", refused)
	}
	for _, issue := range slices.Sorted(maps.Keys(unionKeys(moved, want))) {
		if !slices.Equal(moved[issue], want[issue]) {
			t.Errorf("%s moved %s in the replay, and the record moved it %s", issue, describeMoves(moved[issue]), describeMoves(want[issue]))
		}
	}
}

// failedClaimsGoOnWindow is a window from 10:00 to 13:00, its inputs reaching back to the daemon's
// boot at 08:00, recorded by a release that writes no line where a phase is held, so the trace
// reads each hold from its worker's failed claim. Four roots, each holding one of the four slots
// from its promotion, have a phase worker's claim fail, and the record shows the phase going on
// with no `workflow: a held phase is released`. ACME-1's reviewer fails at 09:30 and is launched
// again at 09:31, and its review sends it back to implementing at 10:30. ACME-4's implementer fails
// at 09:40 and is launched again at 10:20, inside the window, and moves it to testing at 11:50.
// ACME-2's implementer fails at 10:40 and is launched again at 10:45, and moves it to testing at
// 11:30. ACME-3's implementer fails at 11:00 and is never launched again, and the record moves it to
// testing at 11:40, the move's line naming implementing as the phase it left.
func failedClaimsGoOnWindow(t *testing.T) Window {
	b := newWindowBuilder(t, clock(10, 0), clock(13, 0))
	b.reaches(2 * time.Hour)
	b.boot(clock(8, 0))
	implementing := func(at time.Time) builtMove {
		return builtMove{at, phase.Implementing, workflow.TriggerPlannerCompleted}
	}
	rootThrough(b, "ACME-1", "a1", clock(8, 5), clock(8, 20), implementing(clock(8, 40)),
		builtMove{clock(9, 0), phase.Testing, workflow.TriggerImplementationReady}, builtMove{clock(9, 20), phase.Reviewing, workflow.TriggerTesterPassed})
	rootThrough(b, "ACME-2", "a2", clock(8, 10), clock(8, 25), implementing(clock(8, 45)))
	rootThrough(b, "ACME-3", "a3", clock(8, 15), clock(8, 30), implementing(clock(8, 50)))
	rootThrough(b, "ACME-4", "a4", clock(8, 20), clock(8, 35), implementing(clock(8, 55)))
	b.claimFailed(clock(9, 30), "ACME-1", claim.RoleReviewer)
	b.launched(clock(9, 31), "ACME-1", claim.RoleReviewer, true)
	b.turnStarts(clock(9, 31), "ACME-1", claim.RoleReviewer)
	b.claimFailed(clock(9, 40), "ACME-4", claim.RoleImplementer)
	b.launched(clock(10, 20), "ACME-4", claim.RoleImplementer, true)
	b.turnStarts(clock(10, 20), "ACME-4", claim.RoleImplementer)
	b.segmentEnds(clock(10, 30), "ACME-1", phase.Reviewing, phase.Implementing, workflow.TriggerReviewRejected)
	b.claimFailed(clock(10, 40), "ACME-2", claim.RoleImplementer)
	b.launched(clock(10, 45), "ACME-2", claim.RoleImplementer, true)
	b.turnStarts(clock(10, 45), "ACME-2", claim.RoleImplementer)
	b.claimFailed(clock(11, 0), "ACME-3", claim.RoleImplementer)
	b.segmentEnds(clock(11, 30), "ACME-2", phase.Implementing, phase.Testing, workflow.TriggerImplementationReady)
	b.moved(clock(11, 40), "ACME-3", phase.Implementing, phase.Testing, workflow.TriggerImplementationReady)
	b.turnStarts(clock(11, 40), "ACME-3", claim.RoleTester)
	b.segmentEnds(clock(11, 50), "ACME-4", phase.Implementing, phase.Testing, workflow.TriggerImplementationReady)
	return b.build()
}

// A hold read from a phase worker's failed claim ends where the record shows the phase going on
// with no line of its release: at the next launch of a claim of the issue in the failed role, the
// phase's worker running again, or at the issue's next move, whose line names the phase it stood
// in. In failedClaimsGoOnWindow the trace ends ACME-1's hold at its reviewer's launch at 09:31,
// before the window's start, ACME-4's at 10:20, ACME-2's at 10:45, and ACME-3's at its move at
// 11:40, each by WaitWentOn. Replayed under main, the rule the record ran, ACME-1 starts in
// reviewing rather than held, ACME-4, held at the start, is released at 10:20, ACME-2 and ACME-3 at
// their ends, and each makes its recorded move: no move is refused and none is left out.
func TestAHoldTheRecordShowsGoingOnEndsWhereItGoesOn(t *testing.T) {
	t.Parallel()
	window := failedClaimsGoOnWindow(t)
	trace := placedTrace(t, window)
	for _, want := range []struct {
		key        string
		role       claim.Role
		from       phase.Phase
		start, end time.Time
	}{
		{"ACME-1", claim.RoleReviewer, phase.Reviewing, clock(9, 30), clock(9, 31)},
		{"ACME-2", claim.RoleImplementer, phase.Implementing, clock(10, 40), clock(10, 45)},
		{"ACME-3", claim.RoleImplementer, phase.Implementing, clock(11, 0), clock(11, 40)},
		{"ACME-4", claim.RoleImplementer, phase.Implementing, clock(9, 40), clock(10, 20)},
	} {
		holds := slices.DeleteFunc(slices.Clone(trace.Issues[want.key].Waits), func(w Wait) bool { return w.Kind != WaitHeld })
		if len(holds) != 1 || holds[0].Role != want.role || holds[0].From != want.from || !holds[0].Start.Equal(want.start) ||
			!holds[0].End.Equal(want.end) || holds[0].EndedBy != WaitWentOn {
			t.Errorf("%s's holds are %s; want one of its %s from %s at %s, ended by %s at %s", want.key, describeReading(holds), want.role,
				want.from, want.start.Format("15:04"), WaitWentOn, want.end.Format("15:04"))
		}
	}

	moved, refused := replayMoves(t, window)
	tested := fixtureMove{From: phase.Implementing, To: phase.Testing, Trigger: workflow.TriggerImplementationReady}
	checkMoves(t, moved, map[string][]fixtureMove{
		"ACME-1": {{From: phase.Reviewing, To: phase.Implementing, Trigger: workflow.TriggerReviewRejected}},
		"ACME-2": {tested}, "ACME-3": {tested}, "ACME-4": {tested},
	}, refused)
}

// childOfAReadmittedTreeWindow is a window from 10:00 to 13:00, its inputs reaching back to the
// daemon's boot at 08:00. ACME-4, a root, runs to its merge at 08:45, a person sets it back to todo
// at 09:00, and the daemon takes its tree back at its second generation and promotes it. At 10:05
// its architect files ACME-5 under it in triage, and at 10:06 the daemon's own session releases
// the child to todo (release_children), which the record takes into the tree at its generation.
// At 10:10 the architect registers ACME-4's design: the root waits for its open child and gives
// its slot back, and ACME-5 takes it and starts planning. At 10:30 the architect moves ACME-5's
// rank alone, and ACME-5 moves to implementing at 11:00 and to testing at 11:30.
func childOfAReadmittedTreeWindow(t *testing.T) Window {
	b := newWindowBuilder(t, clock(10, 0), clock(13, 0))
	b.reaches(2 * time.Hour)
	b.boot(clock(8, 0))
	mergedRoot(b, "ACME-4", "a4", clock(8, 5))
	b.edit(clock(9, 0), "ACME-4", setStatus("todo"))
	b.ownTree("ACME-4")
	b.rootPromoted(clock(9, 0), "ACME-4")
	architect := b.session("ACME-4", claim.RoleArchitect)
	b.filedBy(clock(10, 5), "ACME-5", filing{Title: "Write the export's header row", Parent: "ACME-4", Tree: "ACME-4", Status: "triage",
		Rank: "a5"}, architect)
	b.daemonWrites(clock(10, 6), "ACME-5", "todo")
	b.rootWaitsForChildren(clock(10, 10), "ACME-4", 1)
	b.childStarts(clock(10, 10), "ACME-5")
	b.changed(clock(10, 30), "ACME-5", architect, func(issue *builtIssue) { issue.Rank = "a3" })
	b.segmentEnds(clock(11, 0), "ACME-5", phase.Planning, phase.Implementing, workflow.TriggerPlannerCompleted)
	b.segmentEnds(clock(11, 30), "ACME-5", phase.Implementing, phase.Testing, workflow.TriggerImplementationReady)
	return b.build()
}

// A child filed in the window enters its tree at the tree's generation, as the daemon records a
// child it holds no record of (workflow's recordChild, enterChild with its root's generation), and
// the record's moves of it are made at that generation. No event raises the child's generation in
// a replay that the record did not raise: the replay's daemon enters ACME-5 of
// childOfAReadmittedTreeWindow at the daemon's write of its todo, at its tree's second generation,
// as the record's did, and the architect's later change of its rank alone writes no status. The
// trace holds ACME-5's entry at its tree's generation, 2, and a replay under main makes each of its
// recorded moves and refuses none. It fails where the trace counts the child's generation from
// one, for which the replay refuses each recorded move of it ("ACME-5 stands at generation 2, and
// the recorded move of planning by planner_completed is generation 1's").
func TestAChildFiledInTheWindowMovesAtItsTreesGeneration(t *testing.T) {
	t.Parallel()
	window := childOfAReadmittedTreeWindow(t)
	trace := placedTrace(t, window)
	if root := trace.Issues["ACME-4"]; root.Generation() != 2 {
		t.Errorf("the trace holds ACME-4 at generation %d, want 2: its tree was taken back at 09:00", root.Generation())
	}
	if entries := trace.Issues["ACME-5"].Entries; len(entries) != 1 || entries[0].Tree != "ACME-4" || entries[0].Generation != 2 ||
		!entries[0].At.Equal(clock(10, 6)) {
		t.Errorf("ACME-5's entries are %s; want one into ACME-4's tree at 10:06, at the tree's generation 2", describeReading(entries))
	}

	moved, refused := replayMoves(t, window)
	checkMoves(t, moved, map[string][]fixtureMove{
		"ACME-5": {
			{From: phase.Admitted, To: phase.Planning, Trigger: workflow.TriggerGateOpened},
			{From: phase.Planning, To: phase.Implementing, Trigger: workflow.TriggerPlannerCompleted},
			{From: phase.Implementing, To: phase.Testing, Trigger: workflow.TriggerImplementationReady},
		},
	}, refused)
}

// adoptedRootWindow is a window from 10:00 to 12:00 under a cap of 1, its inputs reaching back to
// the daemon's boot at 07:00. ACME-4, a root, holds the slot from its promotion at 07:05 while its
// architect designs its tree. ACME-6, a root handed to Legion at 07:10, waits for the slot, and a
// person sets it back to backlog at 09:00: it leaves the workflow, and its tree lingers. At 10:05
// ACME-4's architect makes ACME-6 a child of ACME-4, and at 10:06 releases it into its tree
// (release_children): the daemon adopts it at its second generation, parked at done since its
// record stands in backlog, and the release's own write of its todo a second later runs it again at
// its third. At 10:10 the architect registers ACME-4's design: the root waits for its open child and
// gives its slot back, and ACME-6 takes it and starts planning; it moves to implementing at 11:00
// and to testing at 11:30.
func adoptedRootWindow(t *testing.T) Window {
	b := newWindowBuilder(t, clock(10, 0), clock(12, 0))
	b.reaches(3 * time.Hour)
	b.capped(1)
	b.boot(clock(7, 0))
	b.file(clock(7, 5), "ACME-4", filing{Title: "Export the widget list", Status: "todo", Rank: "a4", Labels: handedToLegion})
	b.rootPromoted(clock(7, 5), "ACME-4")
	b.file(clock(7, 10), "ACME-6", filing{Title: "Count the widgets per shelf", Status: "todo", Rank: "a6", Labels: handedToLegion})
	b.edit(clock(9, 0), "ACME-6", setStatus("backlog"))
	b.leaves(clock(9, 0), "ACME-6", "backlog")
	architect := b.session("ACME-4", claim.RoleArchitect)
	b.changed(clock(10, 5), "ACME-6", architect, func(issue *builtIssue) { issue.Parent = "ACME-4" })
	adopted := b.issue("ACME-6")
	adopted.tree, adopted.generation = "ACME-4", adopted.generation+1
	b.info(clock(10, 6), "workflow: a queued root joined a live tree", "issue", "ACME-6", "tree", "ACME-4", "generation", adopted.generation)
	b.daemonWrites(clock(10, 6).Add(time.Second), "ACME-6", "todo")
	adopted.generation++
	b.rootWaitsForChildren(clock(10, 10), "ACME-4", 1)
	b.childStarts(clock(10, 10), "ACME-6")
	b.segmentEnds(clock(11, 0), "ACME-6", phase.Planning, phase.Implementing, workflow.TriggerPlannerCompleted)
	b.segmentEnds(clock(11, 30), "ACME-6", phase.Implementing, phase.Testing, workflow.TriggerImplementationReady)
	return b.build()
}

// A root an adoption parks enters its tree's run at the generation after the one the adoption
// gave it: the daemon writes the adopted record at its next generation, the one the line of the
// adoption names, parked at done where it does not stand at todo (workflow's moveIntoTree), and the
// release's own todo then runs it again at the next (ReenterChild). The replay's daemon does the
// same with ACME-6 of adoptedRootWindow. The trace holds ACME-6's entry at its third generation, and
// a replay under main makes each of its recorded moves and refuses none. It fails where the trace
// counts the todo's entry from the issue's run before the adoption, one generation short, for which
// the replay refuses each recorded move of it ("ACME-6 stands at generation 3, and the recorded move
// of planning by planner_completed is generation 2's") and ACME-6 holds its slot in planning with no
// turn.
func TestAnAdoptedRootMovesAtTheGenerationAfterItsAdoption(t *testing.T) {
	t.Parallel()
	window := adoptedRootWindow(t)
	trace := placedTrace(t, window)
	entries := trace.Issues["ACME-6"].Entries
	if n := len(entries); n != 2 || entries[1].Tree != "ACME-4" || entries[1].Generation != 3 || !entries[1].At.Equal(clock(10, 6).Add(time.Second)) {
		t.Errorf("ACME-6's entries are %s; want a second into ACME-4's tree at 10:06:01, at generation 3", describeReading(entries))
	}

	moved, refused := replayMoves(t, window)
	checkMoves(t, moved, map[string][]fixtureMove{
		"ACME-6": {
			{From: phase.Admitted, To: phase.Planning, Trigger: workflow.TriggerGateOpened},
			{From: phase.Planning, To: phase.Implementing, Trigger: workflow.TriggerPlannerCompleted},
			{From: phase.Implementing, To: phase.Testing, Trigger: workflow.TriggerImplementationReady},
		},
	}, refused)
}

// slotTakenWithNoLineWindow is a window from 10:00 to 12:00, its inputs reaching back to the
// daemon's boot at 08:00, under a cap of three. ACME-1, ACME-2 and ACME-4, roots, take the three
// slots at their promotions; ACME-3, a root handed to Legion at 08:35, waits for one. ACME-4 moves
// past its review to accepting at 09:40, giving its slot back, and ACME-3 takes it; ACME-2 moves to
// accepting at 09:45, giving its slot back. At 09:50 ACME-2's architect sends it back to
// implementing, and it takes the free slot by a way that writes no slot line: its implementer
// starts at 09:52. At 09:58 ACME-4's architect sends it back too, at a full cap: it owes its
// implementer's start. In the window ACME-1 moves past its review at 11:10, its slot goes back,
// whose line says two slots in use, and the pass gives it to ACME-4, whose work came back.
func slotTakenWithNoLineWindow(t *testing.T) Window {
	b := newWindowBuilder(t, clock(10, 0), clock(12, 0))
	b.reaches(2 * time.Hour)
	b.capped(3)
	b.boot(clock(8, 0))
	promotedRoot(b, clock(8, 5), "ACME-1", "a1")
	promotedRoot(b, clock(8, 10), "ACME-2", "a2")
	promotedRoot(b, clock(8, 15), "ACME-4", "a4")
	b.rootDesigned(clock(8, 20), "ACME-1")
	b.rootDesigned(clock(8, 25), "ACME-2")
	b.rootDesigned(clock(8, 30), "ACME-4")
	b.file(clock(8, 35), "ACME-3", filing{Title: "Ship the ACME-3 change", Status: "todo", Rank: "a3", Labels: handedToLegion})
	b.segmentEnds(clock(8, 40), "ACME-1", phase.Planning, phase.Implementing, workflow.TriggerPlannerCompleted)
	b.segmentEnds(clock(8, 45), "ACME-2", phase.Planning, phase.Implementing, workflow.TriggerPlannerCompleted)
	b.segmentEnds(clock(8, 50), "ACME-4", phase.Planning, phase.Implementing, workflow.TriggerPlannerCompleted)
	b.segmentEnds(clock(9, 5), "ACME-2", phase.Implementing, phase.Testing, workflow.TriggerImplementationReady)
	b.segmentEnds(clock(9, 10), "ACME-4", phase.Implementing, phase.Testing, workflow.TriggerImplementationReady)
	b.segmentEnds(clock(9, 25), "ACME-2", phase.Testing, phase.Reviewing, workflow.TriggerTesterPassed)
	b.segmentEnds(clock(9, 25), "ACME-4", phase.Testing, phase.Reviewing, workflow.TriggerTesterPassed)
	b.segmentEnds(clock(9, 40), "ACME-4", phase.Reviewing, phase.Accepting, workflow.TriggerReviewApproved)
	b.rootPromoted(clock(9, 40), "ACME-3")
	b.segmentEnds(clock(9, 45), "ACME-2", phase.Reviewing, phase.Accepting, workflow.TriggerReviewApproved)
	b.turnEnds(clock(9, 50), "ACME-2", claim.RoleArchitect)
	b.moved(clock(9, 50), "ACME-2", phase.Accepting, phase.Implementing, workflow.TriggerBackward)
	b.heldWithNoLine("ACME-2")
	b.turnStarts(clock(9, 52), "ACME-2", claim.RoleImplementer)
	b.rootDesigned(clock(9, 55), "ACME-3")
	b.turnEnds(clock(9, 58), "ACME-4", claim.RoleArchitect)
	b.moved(clock(9, 58), "ACME-4", phase.Accepting, phase.Implementing, workflow.TriggerBackward)
	b.segmentEnds(clock(10, 30), "ACME-1", phase.Implementing, phase.Testing, workflow.TriggerImplementationReady)
	b.segmentEnds(clock(10, 50), "ACME-1", phase.Testing, phase.Reviewing, workflow.TriggerTesterPassed)
	b.segmentEnds(clock(11, 10), "ACME-1", phase.Reviewing, phase.Accepting, workflow.TriggerReviewApproved)
	b.workCameBack(clock(11, 10), "ACME-4", phase.Implementing)
	b.turnStarts(clock(11, 10), "ACME-4", claim.RoleImplementer)
	return b.build()
}

// An issue that stands at the window's start in a worker's phase that keeps a slot, holding none
// by the slot lines, with no wait that gives the slot back, whose worker started after its last
// slot went back, held one the window holds no line of (assumption 34), and the slots the trace
// holds are checked against the record's count at the window's start, where the start state places
// them, not at the reach's first slot line. In slotTakenWithNoLineWindow the trace infers ACME-2's
// slot from its send-back at 09:50, open at the window's end, and none for ACME-4, whose work came
// back at a full cap and whose implementer has not started: it owes its start. The count is taken
// at the first slot line at or after 10:00, ACME-1's release at 11:10, which says two in use, and
// the trace holds two right after it: ACME-2's and ACME-3's. Seeded at 10:00, the start state
// writes the three slots in the order they were taken, ACME-1's, ACME-3's and ACME-2's, counts one
// inferred, reports no difference, writes ACME-4's owed start, and names no issue.
func TestTheTraceInfersASlotTakenWithNoLineAtTheStart(t *testing.T) {
	t.Parallel()
	window := slotTakenWithNoLineWindow(t)
	trace := placedTrace(t, window)
	sentBack := trace.Issues["ACME-2"].Slots
	if n := len(sentBack); n != 2 || !sentBack[1].Inferred || sentBack[1].Before || !sentBack[1].Start.Equal(clock(9, 50)) || !sentBack[1].End.IsZero() {
		t.Errorf("ACME-2's slots are %s; want its slot to 09:45, then one inferred from 09:50, open at the window's end", describeReading(sentBack))
	}
	if owing := trace.Issues["ACME-4"].Slots; slices.ContainsFunc(owing, func(span SlotSpan) bool { return span.Inferred }) {
		t.Errorf("ACME-4's slots are %s; want none inferred: its work came back at a full cap and its implementer has not started", describeReading(owing))
	}
	if trace.InferredSlots != 1 {
		t.Errorf("the trace counts %d inferred slots, want 1", trace.InferredSlots)
	}
	if count := trace.SlotCount; !count.At.Equal(clock(11, 10)) || count.Recorded != 2 || count.Held != 2 {
		t.Errorf("the trace's slot count is %+v; want the line of 11:10, the first at or after the window's start, carrying 2 slots in use and 2 the trace holds",
			count)
	}

	st := seedStore(t)
	report := seedTrace(t, st, trace, window.From, func(tx pgx.Tx) {
		ctx := context.Background()
		if holders, want := seededSlots(t, tx), []string{"ACME-1@08:05", "ACME-3@09:40", "ACME-2@09:50"}; !slices.Equal(holders, want) {
			t.Errorf("the seeded slots are %v, want %v", holders, want)
		}
		owed, err := record.NewStore().OwedStarts(ctx, tx, builtProject)
		if err != nil {
			t.Fatalf("read the seeded owed starts: %v", err)
		}
		var starts []string
		for _, start := range owed {
			starts = append(starts, start.Issue+" "+string(start.Role)+" "+start.CreatedAt.UTC().Format("15:04"))
		}
		if want := []string{"ACME-4 implementer 09:58"}; !slices.Equal(starts, want) {
			t.Errorf("the seeded owed starts are %q, want %q", starts, want)
		}
	})
	if report.InferredSlots != 1 || report.SlotCount.Difference() != 0 || len(report.Unplaced) != 0 {
		t.Errorf("the seed reports %d inferred slots, a difference of %d and the issues %+v; want 1, 0 and none",
			report.InferredSlots, report.SlotCount.Difference(), report.Unplaced)
	}
}

// deploymentShapedWindow is a window from 10:00 to 16:00 that holds what a deployment's window
// holds and the fixture windows do not: issues placed at its start, holds read from failed claims,
// children an architect files in it, and a daemon that stops and boots again inside it. Its inputs
// reach back to the boot at 07:00, under a cap of four, recorded by a release that writes no line
// where a phase is held.
//
// Before the start ACME-1, ACME-2 and ACME-3, roots, take three slots and work: ACME-1's reviewer
// fails at 08:40 and is launched again at 08:41; ACME-3 moves past its review at 08:50 and is sent
// back at 09:00, taking the slot again by a way that writes no slot line, its implementer starting
// at 09:02; ACME-4, a root, takes the fourth slot at 08:55; ACME-6 and ACME-7, roots handed to
// Legion at 09:10 and 09:15, wait for one; and ACME-2's implementer fails at 09:50, so ACME-2
// stands held at the start.
//
// In the window ACME-4's architect files ACME-5 in triage at 10:05 and the daemon releases it at
// 10:06; at 10:10 the architect registers the design, ACME-4 waits for its open child and gives its
// slot back, and ACME-5 takes it. ACME-2's implementer is launched again at 10:20. ACME-1 moves past
// its review at 10:30 and ACME-6 takes its slot. ACME-2's implementer fails again at 11:45, the
// daemon stops at 12:00 and boots again at 12:20, and the implementer is launched at 12:30. ACME-5
// moves past its review at 13:00 and ACME-7 takes its slot; at 13:10 a person closes ACME-5, which
// ends ACME-4's wait at a full cap, and at 13:20 the architect files ACME-8, released at 13:21:
// ACME-4 waits for an open child again, which the daemon writes no second line of, since it tells
// a generation's wait once. ACME-3 moves past its review at 14:20 and ACME-8 takes its slot.
func deploymentShapedWindow(t *testing.T) Window {
	b := newWindowBuilder(t, clock(10, 0), clock(16, 0))
	b.reaches(3 * time.Hour)
	b.boot(clock(7, 0))
	promotedRoot(b, clock(7, 5), "ACME-1", "a1")
	promotedRoot(b, clock(7, 10), "ACME-2", "a2")
	promotedRoot(b, clock(7, 15), "ACME-3", "a3")
	b.rootDesigned(clock(7, 20), "ACME-1")
	b.rootDesigned(clock(7, 25), "ACME-2")
	b.rootDesigned(clock(7, 30), "ACME-3")
	b.segmentEnds(clock(7, 40), "ACME-1", phase.Planning, phase.Implementing, workflow.TriggerPlannerCompleted)
	b.segmentEnds(clock(7, 45), "ACME-2", phase.Planning, phase.Implementing, workflow.TriggerPlannerCompleted)
	b.segmentEnds(clock(7, 50), "ACME-3", phase.Planning, phase.Implementing, workflow.TriggerPlannerCompleted)
	b.segmentEnds(clock(8, 0), "ACME-1", phase.Implementing, phase.Testing, workflow.TriggerImplementationReady)
	b.segmentEnds(clock(8, 10), "ACME-3", phase.Implementing, phase.Testing, workflow.TriggerImplementationReady)
	b.segmentEnds(clock(8, 20), "ACME-1", phase.Testing, phase.Reviewing, workflow.TriggerTesterPassed)
	b.segmentEnds(clock(8, 30), "ACME-3", phase.Testing, phase.Reviewing, workflow.TriggerTesterPassed)
	b.claimFailed(clock(8, 40), "ACME-1", claim.RoleReviewer)
	b.launched(clock(8, 41), "ACME-1", claim.RoleReviewer, true)
	b.turnStarts(clock(8, 41), "ACME-1", claim.RoleReviewer)
	b.segmentEnds(clock(8, 50), "ACME-3", phase.Reviewing, phase.Accepting, workflow.TriggerReviewApproved)
	promotedRoot(b, clock(8, 55), "ACME-4", "a4")
	b.turnEnds(clock(9, 0), "ACME-3", claim.RoleArchitect)
	b.moved(clock(9, 0), "ACME-3", phase.Accepting, phase.Implementing, workflow.TriggerBackward)
	b.heldWithNoLine("ACME-3")
	b.turnStarts(clock(9, 2), "ACME-3", claim.RoleImplementer)
	b.file(clock(9, 10), "ACME-6", filing{Title: "Ship the ACME-6 change", Status: "todo", Rank: "a6", Labels: handedToLegion})
	b.file(clock(9, 15), "ACME-7", filing{Title: "Ship the ACME-7 change", Status: "todo", Rank: "a7", Labels: handedToLegion})
	b.claimFailed(clock(9, 50), "ACME-2", claim.RoleImplementer)

	architect := b.session("ACME-4", claim.RoleArchitect)
	child := func(filed time.Time, key, rank string) {
		b.filedBy(filed, key, filing{Title: "Ship the " + key + " change", Parent: "ACME-4", Tree: "ACME-4", Status: "triage", Rank: rank},
			architect)
		b.daemonWrites(filed.Add(time.Minute), key, "todo")
	}
	child(clock(10, 5), "ACME-5", "a5")
	b.rootWaitsForChildren(clock(10, 10), "ACME-4", 1)
	b.childStarts(clock(10, 10), "ACME-5")
	b.launched(clock(10, 20), "ACME-2", claim.RoleImplementer, true)
	b.turnStarts(clock(10, 20), "ACME-2", claim.RoleImplementer)
	b.segmentEnds(clock(10, 30), "ACME-1", phase.Reviewing, phase.Accepting, workflow.TriggerReviewApproved)
	b.rootPromoted(clock(10, 30), "ACME-6")
	b.rootDesigned(clock(10, 45), "ACME-6")
	b.segmentEnds(clock(11, 0), "ACME-5", phase.Planning, phase.Implementing, workflow.TriggerPlannerCompleted)
	b.segmentEnds(clock(11, 15), "ACME-3", phase.Implementing, phase.Testing, workflow.TriggerImplementationReady)
	b.segmentEnds(clock(11, 30), "ACME-1", phase.Accepting, phase.Retro, workflow.TriggerSignOff)
	b.segmentEnds(clock(11, 40), "ACME-5", phase.Implementing, phase.Testing, workflow.TriggerImplementationReady)
	b.claimFailed(clock(11, 45), "ACME-2", claim.RoleImplementer)
	b.stopped(clock(12, 0))
	b.boot(clock(12, 20))
	b.launched(clock(12, 30), "ACME-2", claim.RoleImplementer, true)
	b.turnStarts(clock(12, 30), "ACME-2", claim.RoleImplementer)
	b.segmentEnds(clock(12, 40), "ACME-5", phase.Testing, phase.Reviewing, workflow.TriggerTesterPassed)
	b.segmentEnds(clock(12, 45), "ACME-1", phase.Retro, phase.Merging, workflow.TriggerRetroCompleted)
	b.segmentEnds(clock(13, 0), "ACME-5", phase.Reviewing, phase.Accepting, workflow.TriggerReviewApproved)
	b.rootPromoted(clock(13, 0), "ACME-7")
	b.turnEnds(clock(13, 10), "ACME-5", claim.RoleArchitect)
	b.edit(clock(13, 10), "ACME-5", setStatus(record.DoneStatus))
	b.leaves(clock(13, 10), "ACME-5", record.DoneStatus)
	child(clock(13, 20), "ACME-8", "a8")
	b.segmentEnds(clock(13, 30), "ACME-2", phase.Implementing, phase.Testing, workflow.TriggerImplementationReady)
	b.segmentEnds(clock(14, 0), "ACME-3", phase.Testing, phase.Reviewing, workflow.TriggerTesterPassed)
	b.segmentEnds(clock(14, 20), "ACME-3", phase.Reviewing, phase.Accepting, workflow.TriggerReviewApproved)
	b.childStarts(clock(14, 20), "ACME-8")
	b.segmentEnds(clock(15, 0), "ACME-8", phase.Planning, phase.Implementing, workflow.TriggerPlannerCompleted)
	b.segmentEnds(clock(15, 30), "ACME-6", phase.Planning, phase.Implementing, workflow.TriggerPlannerCompleted)
	return b.build()
}
