package replay

import (
	"context"
	"maps"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// mergedRoot files key at filed as a root handed to Legion at rank and runs it through every phase,
// one every five minutes from its promotion, to its merge forty minutes after its filing, which
// closes it.
func mergedRoot(b *windowBuilder, key, rank string, filed time.Time) {
	b.t.Helper()
	at := func(steps int) time.Time { return filed.Add(time.Duration(steps) * 5 * time.Minute) }
	rootThrough(b, key, rank, filed, at(1),
		builtMove{at(2), phase.Implementing, workflow.TriggerPlannerCompleted},
		builtMove{at(3), phase.Testing, workflow.TriggerImplementationReady},
		builtMove{at(4), phase.Reviewing, workflow.TriggerTesterPassed},
		builtMove{at(5), phase.Accepting, workflow.TriggerReviewApproved},
		builtMove{at(6), phase.Retro, workflow.TriggerSignOff},
		builtMove{at(7), phase.Merging, workflow.TriggerRetroCompleted},
		builtMove{at(8), phase.Done, workflow.TriggerPullRequestMerged})
}

// checkCloses is the close of key's production check at at (workflow's markCheckClosed), and, for
// an issue back in the workflow, the line closeCheck writes in place of retiring its claims.
func checkCloses(b *windowBuilder, at time.Time, key string, runsAgain phase.Phase) {
	b.t.Helper()
	b.info(at, "workflow: production check look reported", "issue", key, "mergeCommit", "merge-"+key, "look", 1, "cause", "merge",
		"verdict", "recorded")
	b.info(at, checkClosedLine, "issue", key, "mergeCommit", "merge-"+key, "outcome", string(record.CheckRecorded), "looks", 1)
	if runsAgain != "" {
		b.info(at, "workflow: production check closed while its issue runs again; its claims and pod are the run's", "issue", key,
			"mergeCommit", "merge-"+key, "phase", string(runsAgain))
	}
}

// checkedWindow holds production checks whose close retires the tester's claim with no line of the
// retirement's own, in a window from 10:00 to 12:00 whose inputs reach back to the daemon's boot at
// 08:00. ACME-1 merges at 08:45, its tester's check starts a turn at 08:50, and the check closes at
// 09:20. ACME-2 merges at 08:50, its check's turn starts at 09:30, and the check closes at 10:30,
// so the turn stands at the window's start in an issue outside the workflow. ACME-3 merges at
// 08:55, a person reopens it at 09:00, it runs again to testing at 09:15, and its check closes at
// 09:40 while it runs again, which retires none of its claims: its tester's turn of the new run
// goes on.
func checkedWindow(t *testing.T) Window {
	b := newWindowBuilder(t, clock(10, 0), clock(12, 0))
	b.reaches(2 * time.Hour)
	b.boot(clock(8, 0))
	mergedRoot(b, "ACME-1", "a1", clock(8, 5))
	mergedRoot(b, "ACME-2", "a2", clock(8, 10))
	mergedRoot(b, "ACME-3", "a3", clock(8, 15))

	b.launched(clock(8, 50), "ACME-1", claim.RoleTester, true)
	b.turnStarts(clock(8, 50), "ACME-1", claim.RoleTester)
	checkCloses(b, clock(9, 20), "ACME-1", "")

	b.launched(clock(9, 30), "ACME-2", claim.RoleTester, true)
	b.turnStarts(clock(9, 30), "ACME-2", claim.RoleTester)
	checkCloses(b, clock(10, 30), "ACME-2", "")

	b.edit(clock(9, 0), "ACME-3", setStatus("todo"))
	b.ownTree("ACME-3")
	b.rootPromoted(clock(9, 0), "ACME-3")
	b.rootDesigned(clock(9, 5), "ACME-3")
	b.segmentEnds(clock(9, 10), "ACME-3", phase.Planning, phase.Implementing, workflow.TriggerPlannerCompleted)
	b.segmentEnds(clock(9, 15), "ACME-3", phase.Implementing, phase.Testing, workflow.TriggerImplementationReady)
	checkCloses(b, clock(9, 40), "ACME-3", phase.Testing)
	return b.build()
}

// lastTurnOf is iss's last turn of role, and whether it has one.
func lastTurnOf(iss *IssueTrace, role claim.Role) (Turn, bool) {
	for _, turn := range slices.Backward(iss.Turns) {
		if turn.Claim.Role == role {
			return turn, true
		}
	}
	return Turn{}, false
}

// The close of an issue's production check retires the tester's claim with no line of the
// retirement's own in any release (CheckClosed), so the trace ends the open turn of that claim at
// the check's `workflow: production check closed` where the issue stands outside the workflow
// (checkedWindow): ACME-1's check turn from 08:50 ends at 09:20 and ACME-2's from 09:30 at 10:30,
// each retired; ACME-3, back in the workflow when its check closes, keeps its tester's turn of the
// new run open. The trace reads the line and places every fact.
func TestAProductionChecksTurnEndsWhereTheCheckCloses(t *testing.T) {
	t.Parallel()
	if !TraceReads(checkClosedLine) {
		t.Errorf("the trace does not read %q, which ends a production check's turn", checkClosedLine)
	}
	trace := placedTrace(t, checkedWindow(t))
	for _, want := range []struct {
		key        string
		start, end time.Time
		by         TurnEnd
	}{
		{"ACME-1", clock(8, 50), clock(9, 20), TurnEndRetired},
		{"ACME-2", clock(9, 30), clock(10, 30), TurnEndRetired},
		{"ACME-3", clock(9, 15), time.Time{}, TurnEndOpen},
	} {
		turn, ok := lastTurnOf(trace.Issues[want.key], claim.RoleTester)
		if !ok || !turn.Start.Equal(want.start) || !turn.End.Equal(want.end) || turn.EndedBy != want.by {
			t.Errorf("%s's last tester turn is %s; want one from %s ended at %v by %s", want.key, describeReading(turn),
				want.start.Format("15:04"), want.end, want.by)
		}
	}
}

// A turn standing at the window's start of an issue outside the workflow is a production check's,
// which changes nothing of a replay (assumption 30): the start state writes no claim of it and names
// no issue for it, and its report counts it under assumption 30. Seeded at 10:00, checkedWindow's
// ACME-2 check turn is left out and counted, and the one claim written is ACME-3's tester's, of the
// turn its new run stands in; no issue is named.
func TestSeedLeavesOutTheTurnOfAnIssueOutsideTheWorkflow(t *testing.T) {
	t.Parallel()
	st := seedStore(t)
	window := checkedWindow(t)
	_, report := seedReported(t, st, window, window.From, func(tx pgx.Tx) {
		claims, err := st.ClaimsTx(context.Background(), tx)
		if err != nil {
			t.Fatalf("read the seeded claims: %v", err)
		}
		var written []string
		for _, c := range claims {
			written = append(written, string(c.Token))
		}
		if tester := builtToken(t, "ACME-3", claim.RoleTester); !slices.Equal(written, []string{string(tester)}) {
			t.Errorf("the seeded claims are %v, want ACME-3's tester's alone", written)
		}
	})
	if len(report.Unplaced) != 0 {
		t.Errorf("the seed names %+v, want no issue: a production check's turn is left out, not named", report.Unplaced)
	}
	if want := map[int]int{assumeNoProductionCheck: 1}; !maps.Equal(report.LeftOut, want) {
		t.Errorf("the seed reports the turns it left out as %v, want %v", report.LeftOut, want)
	}
}

// A linger closed before the window's start leaves its tree closed at the start whether the trace
// held its start or not, so the seed names no issue for its close; a linger the trace does not hold
// that would still stand at the start is one the start state lacks, and its issue is named. ACME-1
// and ACME-2 are roots their records closed before the reach; ACME-1's linger closes at 09:00 and
// ACME-2's at 10:30, neither begun in the log. Seeded at 10:00, ACME-2 alone is named, for the
// linger that stands then; seeded at 11:00, after both closes, neither is. The trace holds both
// closes as facts of a linger.
func TestSeedNamesALingerItDoesNotHoldOnlyWhereItStandsAtTheStart(t *testing.T) {
	t.Parallel()
	st := seedStore(t)
	b := newWindowBuilder(t, clock(10, 0), clock(12, 0))
	b.reaches(2 * time.Hour)
	b.boot(clock(8, 0))
	for _, key := range []string{"ACME-1", "ACME-2"} {
		b.file(clock(7, 0), key, filing{Title: "Ship the " + key + " change", Status: record.DoneStatus, Rank: "a" + key[5:], Labels: handedToLegion})
	}
	rootThrough(b, "ACME-3", "a3", clock(8, 5), clock(8, 20))
	b.lingerCloses(clock(9, 0), "ACME-1", 1)
	b.lingerCloses(clock(10, 30), "ACME-2", 1)
	window := b.build()

	trace, report := seedReported(t, st, window, window.From, nil)
	facts := map[string]time.Time{}
	for _, fact := range trace.Unplaced {
		if fact.Kind == UnplacedLinger {
			facts[fact.Issue] = fact.At
		}
	}
	if len(trace.Unplaced) != 2 || !facts["ACME-1"].Equal(clock(9, 0)) || !facts["ACME-2"].Equal(clock(10, 30)) {
		t.Errorf("the trace could not place %+v; want the closes of ACME-1's linger at 09:00 and ACME-2's at 10:30", trace.Unplaced)
	}
	if len(report.Unplaced) != 1 || report.Unplaced[0].Issue != "ACME-2" ||
		!strings.Contains(report.Unplaced[0].Reason, "stands at the window's start") {
		t.Errorf("seeded at 10:00, the seed names %+v; want ACME-2 alone, for the linger that stands then", report.Unplaced)
	}
	if later := seedTrace(t, st, trace, clock(11, 0), nil); len(later.Unplaced) != 0 {
		t.Errorf("seeded at 11:00, the seed names %+v; want none, both lingers having closed", later.Unplaced)
	}
}

// heldFromBeforeTheReach is a window from 10:00 to 12:00 whose inputs reach back to 08:00, with
// recorded holders the slots the record held from before the reach. ACME-1, a root handed to
// Legion and promoted before the reach, stands in implementing where the log begins, its
// implementer in a turn from 08:30, and moves to testing at 09:00, its tester in a turn from then:
// no slot line names it. ACME-2 is promoted at 08:10, the first slot line, which carries the slots
// in use counted with the recorded holders, and plans from 08:20.
func heldFromBeforeTheReach(t *testing.T, recorded ...string) Window {
	b := newWindowBuilder(t, clock(10, 0), clock(12, 0))
	b.reaches(2 * time.Hour)
	b.boot(clock(8, 0))
	for _, key := range recorded {
		b.heldWithNoLine(key)
	}
	b.file(clock(7, 0), "ACME-1", filing{Title: "Ship the ACME-1 change", Status: "todo", Rank: "a1", Labels: handedToLegion})
	b.daemonWrites(clock(7, 5), "ACME-1", "in_progress")
	b.turnStarts(clock(8, 30), "ACME-1", claim.RoleImplementer)
	b.segmentEnds(clock(9, 0), "ACME-1", phase.Implementing, phase.Testing, workflow.TriggerImplementationReady)
	promotedRoot(b, clock(8, 10), "ACME-2", "a2")
	b.rootDesigned(clock(8, 20), "ACME-2")
	return b.build()
}

// The trace infers the slot of an issue the log first shows in a phase that keeps one, which no
// slot line names: it held one from before the reach (assumption 34). heldFromBeforeTheReach's
// ACME-1 holds a slot from 08:00, the reach, still held at the window's end, and is the trace's one
// inferred holder. The slots in use the first slot line carries, ACME-2's promotion at 08:10, are
// checked against the slots the trace holds right after it, two: where the record held ACME-1's
// slot from before the reach, its line carries two and the count agrees; where the record also held
// a slot for an issue the window holds no line of, it carries three and the trace holds one fewer.
// Seeded at 10:00, the start state writes ACME-1's slot before ACME-2's, counts it inferred,
// reports the difference and names no issue.
func TestTheTraceInfersASlotHeldFromBeforeTheReach(t *testing.T) {
	t.Parallel()
	st := seedStore(t)
	for _, tc := range []struct {
		name       string
		recorded   []string
		difference int
	}{
		{name: "the inferred holder makes the count agree", recorded: []string{"ACME-1"}, difference: 0},
		{name: "a holder the window holds no line of leaves the trace one short", recorded: []string{"ACME-1", "ACME-9"}, difference: -1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			window := heldFromBeforeTheReach(t, tc.recorded...)
			trace, report := seedReported(t, st, window, window.From, func(tx pgx.Tx) {
				if holders, want := seededSlots(t, tx), []string{"ACME-1@08:00", "ACME-2@08:10"}; !slices.Equal(holders, want) {
					t.Errorf("the seeded slots are %v, want %v", holders, want)
				}
			})
			if len(trace.Unplaced) != 0 {
				t.Errorf("the trace could not place %+v", trace.Unplaced)
			}
			want := []SlotSpan{{Start: clock(8, 0), Before: true, Inferred: true}}
			if got := trace.Issues["ACME-1"].Slots; !sameReading(got, want) {
				t.Errorf("ACME-1's slots are %s; want %s", describeReading(got), describeReading(want))
			}
			if trace.InferredSlots != 1 {
				t.Errorf("the trace counts %d inferred holders, want 1", trace.InferredSlots)
			}
			if count := trace.SlotCount; !count.At.Equal(clock(8, 10)) || count.Recorded != 1+len(tc.recorded) || count.Held != 2 {
				t.Errorf("the trace's slot count is %+v, want the line of 08:10 carrying %d slots in use and 2 the trace holds", count,
					1+len(tc.recorded))
			}
			if report.InferredSlots != 1 || report.SlotCount.Difference() != tc.difference || len(report.Unplaced) != 0 {
				t.Errorf("the seed reports %d inferred slots, a difference of %d and the issues %+v; want 1, %d and none",
					report.InferredSlots, report.SlotCount.Difference(), report.Unplaced, tc.difference)
			}
		})
	}
}

// A replay never stops at a recorded turn it cannot write a claim of. ACME-1, a root, holds the one
// slot from 08:05; ACME-2, a root handed to Legion at 08:10, waits for it, and its architect, whose
// claim the log shows no launch of, takes turns with no slot, from 09:00 to 09:30, before the
// window's start at 10:00, and from 10:30 to 10:45, in it. ACME-2's tree runs no lifecycle in the
// replay, which the record never shows open — no slot, no gate opened and no fresh launch of its
// architect in its run (seeder.lifecycles) — so no claim of the 10:30 turn can be admitted: the run
// writes none, counts the turn as one begun with no lifecycle, and finishes the window.
func TestARunGoesOnPastATurnInATreeThatRunsNoLifecycle(t *testing.T) {
	t.Parallel()
	dsn := replayDSN(t)
	b := newWindowBuilder(t, clock(10, 0), clock(12, 0))
	b.reaches(2 * time.Hour)
	b.capped(1)
	b.boot(clock(8, 0))
	rootThrough(b, "ACME-1", "a1", clock(8, 5), clock(8, 20))
	b.file(clock(8, 10), "ACME-2", filing{Title: "Ship the ACME-2 change", Status: "todo", Rank: "a2", Labels: handedToLegion})
	b.launchedBefore("ACME-2", claim.RoleArchitect)
	b.turnStarts(clock(9, 0), "ACME-2", claim.RoleArchitect)
	b.turnEnds(clock(9, 30), "ACME-2", claim.RoleArchitect)
	b.turnStarts(clock(10, 30), "ACME-2", claim.RoleArchitect)
	b.turnEnds(clock(10, 45), "ACME-2", claim.RoleArchitect)

	result, err := Run(context.Background(), RunConfig{Window: b.build(), Rule: mainRule(t), Branch: MainBranch, Seed: 1, Slowdown: 1,
		PGDSN: dsn, Out: t.TempDir()})
	if err != nil {
		t.Fatalf("the replay stops: %v", err)
	}
	if result.Divergence.NoLifecycleTurns != 1 || len(result.Divergence.TurnsLeftOut) != 0 {
		t.Errorf("the replay counts %d turns begun with no lifecycle and left out %v; want ACME-2's turn at 10:30 alone, of an issue in the workflow",
			result.Divergence.NoLifecycleTurns, result.Divergence.TurnsLeftOut)
	}
}

// A re-admitted root whose generation no promotion has opened at the window's start carries the mark
// that its tree's members are still owed their return (record.Issue.MembersOwed), which a window
// recorded before the mark holds none of: the start state writes it as the daemon's upgrade to the
// release that keeps it does (seeder.membersOwed), so the replay's daemon brings the members back at
// the root's promotion, or at the first pass that finds its tree running, as the daemon does. In a
// window from 10:00 to 12:00 reaching back to 08:00, ACME-1 and ACME-2 run to their merges at 08:45
// and 08:50 and a person sets each back to todo at 09:00, which the record takes as each root's
// tree at its second generation; ACME-2 is promoted at 09:30, and ACME-1 still waits in the line at
// 10:00. ACME-3, a root handed to Legion at 09:10, has never run. Seeded at 10:00, ACME-1 alone
// carries the mark: ACME-2's promotion holds its slot, and ACME-3 is at its first generation.
func TestSeedOwesTheMembersOfARootReadmittedBeforeTheStart(t *testing.T) {
	t.Parallel()
	st := seedStore(t)
	b := newWindowBuilder(t, clock(10, 0), clock(12, 0))
	b.reaches(2 * time.Hour)
	b.boot(clock(8, 0))
	mergedRoot(b, "ACME-1", "a1", clock(8, 5))
	mergedRoot(b, "ACME-2", "a2", clock(8, 10))
	for _, key := range []string{"ACME-1", "ACME-2"} {
		b.edit(clock(9, 0), key, setStatus("todo"))
		b.ownTree(key)
	}
	b.file(clock(9, 10), "ACME-3", filing{Title: "Ship the ACME-3 change", Status: "todo", Rank: "a3", Labels: handedToLegion})
	b.rootPromoted(clock(9, 30), "ACME-2")
	window := b.build()

	_, report := seedReported(t, st, window, window.From, func(tx pgx.Tx) {
		rows, err := tx.Query(context.Background(), `select key from issues where members_owed order by key`)
		if err != nil {
			t.Fatalf("read the seeded marks: %v", err)
		}
		owed, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			t.Fatalf("read the seeded marks: %v", err)
		}
		if !slices.Equal(owed, []string{"ACME-1"}) {
			t.Errorf("the seeded roots owed their members' return are %v, want ACME-1 alone", owed)
		}
	})
	if len(report.Unplaced) != 0 {
		t.Errorf("the seed names %+v, want no issue", report.Unplaced)
	}
}
