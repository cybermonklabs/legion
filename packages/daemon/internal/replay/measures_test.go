package replay

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"math"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/projection"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// handStint is the state an issue of a hand-worked timeline stands in from at on.
func handStint(at time.Time, state IssueState) Stint {
	return Stint{At: at, IssueState: state}
}

// assertOccupancy fails where got is not want, floats to within fixtureTolerance.
func assertOccupancy(t *testing.T, name string, got, want Occupancy) {
	t.Helper()
	if math.Abs(got.Average-want.Average) > fixtureTolerance || got.Max != want.Max || math.Abs(got.Hours-want.Hours) > fixtureTolerance {
		t.Errorf("%s = %+v, want %+v", name, got, want)
	}
}

// assertNear fails where got stands further than fixtureTolerance from want.
func assertNear(t *testing.T, name string, got, want float64) {
	t.Helper()
	if math.Abs(got-want) > fixtureTolerance {
		t.Errorf("%s = %v, want %v", name, got, want)
	}
}

// stateAt is the state c stands in at at, its stint at at among them, and false where it stands in
// none yet.
func stateAt(c *Course, at time.Time) (IssueState, bool) {
	i := sort.Search(len(c.States), func(i int) bool { return c.States[i].At.After(at) }) - 1
	if i < 0 {
		return IssueState{}, false
	}
	return c.States[i].IssueState, true
}

// The two readings of started work that waits for a slot stand side by side (spec R4; `replay.md`,
// "The measures"): reading (b) counts each member of the line's returned part that holds
// no slot, reading (a) the same members while admission.blocked does not name them, and each
// gives its average over the span, its largest count and its hours. Over six hours: ACME-6 waits
// only for a slot from 09:00 to 10:00; ACME-1 is given back for a wait at 10:00, blocked to 11:00,
// and waits only for a slot to 12:00; ACME-2 waits only for a slot from 11:30 to 13:00; ACME-5 is
// an answered root in the returned part that holds its slot, which neither reading counts. ACME-6
// leaves the line at the instant ACME-1 enters it, so the two are never counted together and the
// largest count is the two of 11:30 to 12:00. Reading (b): 1 + 2 + 1.5 = 4.5 hours, an average of
// 0.75, at most 2. Reading (a): 1 + 1 + 1.5 = 3.5 hours, an average of 0.58333…, at most 2. The
// issues past their review stand apart: ACME-3 with its slot from 13:00 to 14:00, ACME-4 with none
// from 13:00 to 15:00.
func TestStartedWorkWithoutASlotTwoReadings(t *testing.T) {
	t.Parallel()
	returned := func(blocked bool) IssueState {
		return IssueState{InFlight: true, Part: record.PartReturned, Blocked: blocked, Waiting: !blocked}
	}
	holding := IssueState{InFlight: true, Holds: true}
	tl := Timeline{From: clock(9, 0), To: clock(15, 0), Issues: map[string]*Course{
		"ACME-6": {States: []Stint{handStint(clock(9, 0), returned(false)), handStint(clock(10, 0), holding)}},
		"ACME-1": {States: []Stint{handStint(clock(9, 0), holding), handStint(clock(10, 0), returned(true)),
			handStint(clock(11, 0), returned(false)), handStint(clock(12, 0), holding)}},
		"ACME-2": {States: []Stint{handStint(clock(11, 30), returned(false)), handStint(clock(13, 0), holding)}},
		"ACME-5": {States: []Stint{handStint(clock(9, 0), IssueState{InFlight: true, Holds: true, Part: record.PartReturned})}},
		"ACME-3": {States: []Stint{handStint(clock(13, 0), IssueState{InFlight: true, Holds: true, PastReview: true}),
			handStint(clock(14, 0), IssueState{})}},
		"ACME-4": {States: []Stint{handStint(clock(13, 0), IssueState{InFlight: true, PastReview: true})}},
	}}
	card := Measure(tl)
	assertOccupancy(t, "startedWithoutSlot", card.StartedWithoutSlot, Occupancy{Average: 4.5 / 6, Max: 2, Hours: 4.5})
	assertOccupancy(t, "waitOnlyForSlot", card.WaitOnlyForSlot, Occupancy{Average: 3.5 / 6, Max: 2, Hours: 3.5})
	assertOccupancy(t, "pastReviewHolding", card.PastReviewHolding, Occupancy{Average: 1.0 / 6, Max: 1, Hours: 1})
	assertOccupancy(t, "pastReviewNotHolding", card.PastReviewNotHolding, Occupancy{Average: 2.0 / 6, Max: 1, Hours: 2})
}

// Priority taken is the time-average share of the pairs of a slot holder and an issue that waits
// for a slot in which the waiting issue ranks above the holder (spec R4), by record.RankLess over
// the tree roots' ranks and then the issues' own, and the waiting issues are the plan's: members of
// either part of the line that hold no slot and that no wait, admission.blocked or admission.held
// holds. Over four hours, with ACME-1 (a2) holding throughout and ACME-2 (a0) holding but from
// 11:00 to 12:00: ACME-3, a queued root at a1, waits from 09:00 to 11:00; ACME-4, a first-phase
// child of ACME-2's tree at z9, waits from 10:00 on; ACME-7, given back and blocked, and ACME-8, a
// queued root its link holds back at 0, wait for no slot and count in no pair.
//
//	09:00 to 10:00: ACME-3 above ACME-1 of 2 pairs, 1/2.
//	10:00 to 11:00: ACME-3 above ACME-1, and ACME-4 above ACME-1 by its root's a0, of 4; ACME-4
//	stands below ACME-2 in ACME-2's own tree: 2/4.
//	11:00 to 12:00: ACME-4 above ACME-1, of 1: 1.
//	12:00 to 13:00: ACME-4 above ACME-1 of 2: 1/2.
//
// The average is (0.5 + 0.5 + 1 + 0.5)/4 = 0.625.
func TestPriorityInversionShare(t *testing.T) {
	t.Parallel()
	holder := func(key, rank string) IssueState {
		return IssueState{Tree: key, Rank: rank, RootRank: rank, InFlight: true, Holds: true}
	}
	waiting := func(tree, rank, rootRank string, part record.LinePart) IssueState {
		return IssueState{Tree: tree, Rank: rank, RootRank: rootRank, Part: part, Waiting: true}
	}
	tl := Timeline{From: clock(9, 0), To: clock(13, 0), Issues: map[string]*Course{
		"ACME-1": {States: []Stint{handStint(clock(9, 0), holder("ACME-1", "a2"))}},
		"ACME-2": {States: []Stint{handStint(clock(9, 0), holder("ACME-2", "a0")),
			handStint(clock(11, 0), IssueState{InFlight: true, PastReview: true}),
			handStint(clock(12, 0), holder("ACME-2", "a0"))}},
		"ACME-3": {States: []Stint{handStint(clock(9, 0), waiting("ACME-3", "a1", "a1", record.PartStarting)),
			handStint(clock(11, 0), IssueState{})}},
		"ACME-4": {States: []Stint{handStint(clock(10, 0), waiting("ACME-2", "z9", "a0", record.PartStarting))}},
		"ACME-7": {States: []Stint{handStint(clock(9, 0), IssueState{InFlight: true, Part: record.PartReturned, Blocked: true})}},
		"ACME-8": {States: []Stint{handStint(clock(9, 0), IssueState{Part: record.PartStarting, Blocked: true})}},
	}}
	assertNear(t, "priorityTaken", Measure(tl).PriorityTaken, 0.625)
}

// scorecardJSON reads card as scorecard.json writes it into v, so a test reads a measure by the JSON
// name a fixture window's expect.json gives it.
func scorecardJSON(t *testing.T, card Scorecard, v any) {
	t.Helper()
	raw, err := json.Marshal(card)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, v); err != nil {
		t.Fatal(err)
	}
}

// urgentWaitJSON is one priority's wait of urgent work as scorecard.json writes it (urgentWaits).
type urgentWaitJSON struct {
	Issues    int          `json:"issues"`
	FirstSlot waitTimeJSON `json:"firstSlot"`
	Done      waitTimeJSON `json:"done"`
}

// waitTimeJSON is one wait of urgent work as scorecard.json writes it: its median and its 90th
// percentile in minutes, and how many of its waits the end of the run cut.
type waitTimeJSON struct {
	MedianMinutes float64 `json:"medianMinutes"`
	P90Minutes    float64 `json:"p90Minutes"`
	Cut           int     `json:"cut"`
}

// urgentWorkWindow is a window from 09:00 to 12:00 under main, its inputs reaching back to the
// daemon's boot at 08:00 and its run-out to 13:00, whose issues are worked by hand. ACME-4, at P0,
// was filed todo at 08:30, before the window, and stands todo at its start. ACME-1, at P0, is filed
// todo at 09:10; ACME-2, at P1, at 09:20, and a person raises it to P0 at 09:40; ACME-3, at P1, at
// 09:30; ACME-6, at P2, at 09:45; and ACME-5, at P1, at 10:00, which a person raises to P0 at 10:20.
// ACME-1 is promoted at 09:10, ACME-2 at 09:50, ACME-5 at 10:00 and ACME-4 at 10:30. ACME-1 and
// ACME-4 each work 15 minutes in each phase to their done two hours after their promotion, at 11:10
// and, in the run-out, at 12:30; ACME-2 and ACME-5 are not done by 13:00; ACME-3 and ACME-6 are
// never promoted.
func urgentWorkWindow(t *testing.T) Window {
	b := newWindowBuilder(t, clock(9, 0), clock(12, 0))
	b.window.After = Duration(time.Hour)
	b.boot(clock(8, 0))
	level := func(n int) *int { return &n }
	filed := func(at time.Time, key, rank string, priority *int) {
		b.file(at, key, filing{Title: "Ship the " + key + " change", Status: "todo", Rank: rank, Labels: handedToLegion, Priority: priority})
	}
	worked := func(key string, promoted time.Time) {
		b.rootThroughReview(promoted, key)
		stepThrough(b, builtStep{promoted.Add(75 * time.Minute), key, phase.Reviewing, phase.Accepting, workflow.TriggerReviewApproved},
			builtStep{promoted.Add(90 * time.Minute), key, phase.Accepting, phase.Retro, workflow.TriggerSignOff},
			builtStep{promoted.Add(105 * time.Minute), key, phase.Retro, phase.Merging, workflow.TriggerRetroCompleted},
			builtStep{promoted.Add(120 * time.Minute), key, phase.Merging, phase.Done, workflow.TriggerPullRequestMerged})
	}
	raised := func(at time.Time, key string) {
		b.edit(at, key, func(issue *builtIssue) { issue.Priority = level(0) })
	}
	filed(clock(8, 30), "ACME-4", "a4", level(0))
	filed(clock(9, 10), "ACME-1", "a1", level(0))
	worked("ACME-1", clock(9, 10))
	filed(clock(9, 20), "ACME-2", "a2", level(1))
	filed(clock(9, 30), "ACME-3", "a3", level(1))
	raised(clock(9, 40), "ACME-2")
	filed(clock(9, 45), "ACME-6", "a6", level(2))
	b.rootPromoted(clock(9, 50), "ACME-2")
	filed(clock(10, 0), "ACME-5", "a5", level(1))
	b.rootPromoted(clock(10, 0), "ACME-5")
	raised(clock(10, 20), "ACME-5")
	worked("ACME-4", clock(10, 30))
	return b.build()
}

// The wait of urgent work (spec R4) reads, for P0 and for P1 apart, the issues whose own Dispatch
// priority is that one at their first todo in the span, or is raised to it at or before their first
// slot, each counted in the highest of those; the median and the 90th percentile (nearest rank) of
// the minutes from that first todo to the first slot and to the done; and how many of those waits
// the end of the run cut: an issue with no first slot, or no done, by the end of the window's
// run-out counts with its wait to that end. An issue that stood todo from before the span counts
// from the span's start. The record of urgentWorkWindow, worked by hand, the run-out's end, 13:00,
// cutting the waits:
//
//	P0: ACME-1, P0 at its first todo, 09:10: its first slot 0 minutes on, its done 120.
//	    ACME-2, P1 at 09:20, raised to P0 at 09:40, before its first slot at 09:50: 30; no done
//	    by 13:00, 220, cut.
//	    ACME-4, todo from before the window, counted from 09:00: its first slot at 10:30, 90, and
//	    its done in the run-out at 12:30, 210, not cut.
//	    The first slot: 0, 30 and 90, a median of 30 and a 90th percentile of 90, none cut. The
//	    done: 120, 210 and 220, a median of 210 and a 90th percentile of 220, one cut.
//	P1: ACME-3, P1 at 09:30, no first slot and no done by 13:00: 210 and 210, both cut.
//	    ACME-5, P1 at 10:00, its first slot then, 0, and raised to P0 at 10:20, after it, so P1;
//	    no done by 13:00: 180, cut.
//	    The first slot: 0 and 210, a median of 105 and a 90th percentile of 210, one cut. The
//	    done: 180 and 210, a median of 195 and a 90th percentile of 210, two cut.
//
// ACME-6, at P2, counts in neither.
func TestUrgentWorkWaits(t *testing.T) {
	t.Parallel()
	card, err := Score(urgentWorkWindow(t))
	if err != nil {
		t.Fatal(err)
	}
	var read struct {
		UrgentWaits map[string]urgentWaitJSON `json:"urgentWaits"`
	}
	scorecardJSON(t, card, &read)
	want := map[string]urgentWaitJSON{
		"P0": {Issues: 3, FirstSlot: waitTimeJSON{MedianMinutes: 30, P90Minutes: 90}, Done: waitTimeJSON{MedianMinutes: 210, P90Minutes: 220, Cut: 1}},
		"P1": {Issues: 2, FirstSlot: waitTimeJSON{MedianMinutes: 105, P90Minutes: 210, Cut: 1}, Done: waitTimeJSON{MedianMinutes: 195, P90Minutes: 210, Cut: 2}},
	}
	if !maps.Equal(read.UrgentWaits, want) {
		t.Errorf("the record's scorecard gives the urgent waits %+v, want %+v", read.UrgentWaits, want)
	}
}

// The record of the fixture window urgent-work-waits gives the wait of urgent work its README works
// out by hand, which its expect.json holds under main: the record gives ACME-2 its slot at 09:30,
// as a replay under a rule that starts a P0 issue outside the cap does, and every wait is cut at
// 10:15, the end of the window's run-out, not at 10:00. The window's replay under main makes that
// start since cybermonklabs/legion#5488; the record reads the waits by the same code (Measure).
func TestTheRecordOfUrgentWorkWaitsGivesItsHandWorkedWaits(t *testing.T) {
	t.Parallel()
	window, expect := readFixture(t, "urgent-work-waits")
	card, err := Score(window)
	if err != nil {
		t.Fatal(err)
	}
	if want := expect.Rules[record.MainRule.Name].UrgentWaits; !maps.Equal(card.UrgentWaits, want) {
		t.Errorf("the record of urgent-work-waits gives the urgent waits %+v, want %+v", card.UrgentWaits, want)
	}
}

// bothOrdersWindow is a window from 09:00 to 11:00 under main at a cap of 1, its inputs reaching
// back to the daemon's boot at 08:00. ACME-1, at P2 and ranked a1, is filed todo and promoted at
// 09:00 and holds the one slot to the window's end. ACME-2, at P0 and ranked a2, is filed todo at
// 09:00, and ACME-3, with no priority and ranked a0, at 10:00; both wait for a slot to the end.
func bothOrdersWindow(t *testing.T) Window {
	b := newWindowBuilder(t, clock(9, 0), clock(11, 0))
	b.capped(1)
	b.boot(clock(8, 0))
	level := func(n int) *int { return &n }
	filed := func(at time.Time, key, rank string, priority *int) {
		b.file(at, key, filing{Title: "Ship the " + key + " change", Status: "todo", Rank: rank, Labels: handedToLegion, Priority: priority})
	}
	filed(clock(9, 0), "ACME-1", "a1", level(2))
	b.rootPromoted(clock(9, 0), "ACME-1")
	filed(clock(9, 0), "ACME-2", "a2", level(0))
	filed(clock(10, 0), "ACME-3", "a0", nil)
	return b.build()
}

// Priority taken is read in two orders (spec R4): by rank alone, record.RankLess over the tree
// root's rank and then the issue's own (priorityTaken), the order of the lines the recorded windows
// ran; and in today's line's order (priorityTakenBanded), first by the band of the Dispatch
// priority that places each issue (record.PriorityOf: its own, else its tree root's; none after
// P3) and then by rank. The record of bothOrdersWindow, worked by hand:
//
//	09:00 to 10:00: ACME-1 (P2, a1) holds and ACME-2 (P0, a2) waits. By rank ACME-2 stands below
//	ACME-1, a share of 0; with the band P0 comes before P2, a share of 1.
//	10:00 to 11:00: ACME-3 (none, a0) waits too. By rank ACME-3 stands above ACME-1 and ACME-2
//	below it, 1 of 2; with the band ACME-2 stands above ACME-1 and ACME-3, with none, after P2,
//	below it, 1 of 2.
//
// By rank the share is (0 + 0.5)/2 = 0.25; with the band, (1 + 0.5)/2 = 0.75.
func TestPriorityInversionShareInBothOrders(t *testing.T) {
	t.Parallel()
	card, err := Score(bothOrdersWindow(t))
	if err != nil {
		t.Fatal(err)
	}
	var read struct {
		PriorityTaken       float64  `json:"priorityTaken"`
		PriorityTakenBanded *float64 `json:"priorityTakenBanded"`
	}
	scorecardJSON(t, card, &read)
	assertNear(t, "priorityTaken", read.PriorityTaken, 0.25)
	if read.PriorityTakenBanded == nil {
		t.Fatalf("the record's scorecard gives no priorityTakenBanded")
	}
	assertNear(t, "priorityTakenBanded", *read.PriorityTakenBanded, 0.75)
}

// Each send-back loop is counted by its cause (causes.go), one round for each move back inside the
// span, its hours from the move back to the issue's next entry into the phase the move left, or to
// the span's end; and each wait on another issue by what ended it, its hours inside the span, a
// wait that stands at the end counted as open (spec R4). Over six hours: ACME-1's review asks for
// changes at 10:00 and it is back in reviewing at 11:30 (1.5 hours), and its architect sends it
// back from accepting at 13:00 and it is not back by 15:00 (2 hours); ACME-2's test fails at 12:00
// and it is back in testing at 12:20 (1/3 hour); ACME-3 is sent back from accepting at 09:30 and is
// back at 10:30 (1 hour); ACME-4's move back at 08:00, before the span, is no round of it. The
// waits: ACME-1's link wait 09:00 to 10:00, ended by its blocker's done; ACME-3's from 08:00, its
// link taken off at 09:30 (half an hour in the span); ACME-2's children wait from 14:00, open at
// 15:00 (1 hour); and ACME-4's from 10:00 to 16:00, which still stands at 15:00 and so is open
// whatever ended it later (5 hours).
func TestLoopHoursByCause(t *testing.T) {
	t.Parallel()
	move := func(at time.Time, from, to phase.Phase, trigger workflow.TriggerKind) CourseMove {
		return moveOf(at, from, to, trigger, "")
	}
	tl := Timeline{From: clock(9, 0), To: clock(15, 0), Issues: map[string]*Course{
		"ACME-1": {
			Moves: []CourseMove{
				move(clock(9, 0), phase.Testing, phase.Reviewing, workflow.TriggerTesterPassed),
				move(clock(10, 0), phase.Reviewing, phase.Implementing, workflow.TriggerReviewRejected),
				move(clock(11, 0), phase.Implementing, phase.Testing, workflow.TriggerImplementationReady),
				move(clock(11, 30), phase.Testing, phase.Reviewing, workflow.TriggerTesterPassed),
				move(clock(12, 0), phase.Reviewing, phase.Accepting, workflow.TriggerReviewApproved),
				move(clock(13, 0), phase.Accepting, phase.Implementing, workflow.TriggerBackward),
			},
			Waits: []CourseWait{{Start: clock(9, 0), End: clock(10, 0), EndedBy: WaitBlockerDone}},
		},
		"ACME-2": {
			Moves: []CourseMove{
				move(clock(12, 0), phase.Testing, phase.Implementing, workflow.TriggerTesterFailed),
				move(clock(12, 20), phase.Implementing, phase.Testing, workflow.TriggerImplementationReady),
			},
			Waits: []CourseWait{{Start: clock(14, 0), EndedBy: WaitOpen}},
		},
		"ACME-3": {
			Moves: []CourseMove{
				move(clock(9, 30), phase.Accepting, phase.Implementing, workflow.TriggerBackward),
				move(clock(10, 0), phase.Implementing, phase.Testing, workflow.TriggerImplementationReady),
				move(clock(10, 15), phase.Testing, phase.Reviewing, workflow.TriggerTesterPassed),
				move(clock(10, 30), phase.Reviewing, phase.Accepting, workflow.TriggerReviewApproved),
			},
			Waits: []CourseWait{{Start: clock(8, 0), End: clock(9, 30), EndedBy: WaitLinkRemoved}},
		},
		"ACME-4": {
			Moves: []CourseMove{
				move(clock(8, 0), phase.Reviewing, phase.Implementing, workflow.TriggerReviewRejected),
				move(clock(8, 30), phase.Implementing, phase.Testing, workflow.TriggerImplementationReady),
			},
			Waits: []CourseWait{{Start: clock(10, 0), End: clock(16, 0), EndedBy: WaitBlockerDone}},
		},
	}}
	card := Measure(tl)
	wantLoops := map[string]LoopHours{
		string(CauseReviewChanges): {Rounds: 1, Hours: 1.5},
		string(CauseTestFailed):    {Rounds: 1, Hours: 1.0 / 3},
		string(CauseSendBack):      {Rounds: 2, Hours: 3},
	}
	wantWaits := map[string]WaitHours{
		string(WaitBlockerDone): {Count: 1, Hours: 1},
		string(WaitLinkRemoved): {Count: 1, Hours: 0.5},
		string(WaitOpen):        {Count: 2, Hours: 6},
	}
	for _, diff := range scorecardDiffs(Scorecard{Loops: card.Loops, Waits: card.Waits}, Scorecard{Loops: wantLoops, Waits: wantWaits}) {
		t.Error(diff)
	}
}

// Slot-hours are read over each slot from the pass that gives it to its release, with a worker of
// the holder, of any role, in a turn or with none (spec R4); a turn of an issue that holds no slot
// is in neither. Over three hours: ACME-1 holds a slot from 09:00 to 11:00, in a turn to 10:00 and
// in none after; ACME-2 holds one from 10:00 on, in a turn throughout; ACME-3 is in a turn from
// 09:00 with no slot. In a turn: 1 + 2 = 3 hours; in none: 1 hour. The issues in flight, ACME-1
// and ACME-3 for three hours and ACME-2 for two: 8 hours, an average of 8/3, at most 3.
func TestSlotHoursByTurn(t *testing.T) {
	t.Parallel()
	tl := Timeline{From: clock(9, 0), To: clock(12, 0), Issues: map[string]*Course{
		"ACME-1": {States: []Stint{handStint(clock(9, 0), IssueState{InFlight: true, Holds: true, InTurn: true}),
			handStint(clock(10, 0), IssueState{InFlight: true, Holds: true}), handStint(clock(11, 0), IssueState{InFlight: true, InTurn: true})}},
		"ACME-2": {States: []Stint{handStint(clock(10, 0), IssueState{InFlight: true, Holds: true, InTurn: true})}},
		"ACME-3": {States: []Stint{handStint(clock(9, 0), IssueState{InFlight: true, InTurn: true})}},
	}}
	card := Measure(tl)
	assertNear(t, "slotHoursInTurn", card.SlotHoursInTurn, 3)
	assertNear(t, "slotHoursNoTurn", card.SlotHoursNoTurn, 1)
	assertOccupancy(t, "inFlight", card.InFlight, Occupancy{Average: 8.0 / 3, Max: 3, Hours: 8})
}

// A window's own scorecard needs no replay (spec R4): Score reads every measure from the record
// alone, by the code a replay is measured by. Each fixture window whose expect.json names the rule
// its record was made under (its parts' rule) on the main branch is one whose replay under that
// rule keeps the record's course, so its record's scorecard is the one its README works out by
// hand for that rule: `main` for most, and legion-v22.14.1 for the two windows whose record a
// daemon before cybermonklabs/legion#4592 made, root-idle-while-children-run and every-trigger,
// where today's main moves a started parent into awaiting_children and the record does not. What a
// replay alone names is left out: the source is the record, and a record runs under no rule,
// branch, seed or slowdown, rests on none of a replay's assumptions and takes no never-started
// scenario. What a scorecard reads from its trace alone (traceMeasures) and what the forecast
// reads of its line timeline (forecastReadings), which the replay's snapshot scorecard in
// expect.json holds none of, is not compared here.
func TestScorecardOfTheRecord(t *testing.T) {
	t.Parallel()
	scored := 0
	for _, name := range fixtureWindowNames(t) {
		window, expect := readFixture(t, name)
		recorded := partsRule(window.Parts)
		want, named := expect.Rules[recorded]
		if recorded == "" || !named || expect.Branch != "" {
			continue
		}
		scored++
		t.Run(name, func(t *testing.T) {
			got, err := Score(window)
			if err != nil {
				t.Fatalf("score the record of %s: %v", name, err)
			}
			want.Source, want.Rule, want.Branch, want.Seed, want.Slowdown = "record", "", "", 0, 0
			want.Assumptions, want.Scenario = []int{}, ""
			want.DoneByAnyone, want.GateClosedHours, want.Filed, want.SlotsGiven = got.DoneByAnyone, got.GateClosedHours, got.Filed, got.SlotsGiven
			want.DailyMaxima, want.UrgentMeans = got.DailyMaxima, got.UrgentMeans
			for _, diff := range scorecardDiffs(got, want) {
				t.Errorf("the record of %s: %s", name, diff)
			}
		})
	}
	if scored == 0 {
		t.Fatalf("no fixture window has a scorecard under the rule its record was made under: nothing was scored")
	}
}

// The replay measures from the same states `legion state` reads (spec R6): at ten moments of a
// fixture's replay, spread over the run, each a moment every act of the instant has run at, the
// slot holders in each state of the replay's snapshot timeline are the holders projection.Project
// counts in each over the replay's own database at that moment, read as the daemon reads its
// state: its claims, the claims nothing will wake (workflow.Stranded) and the holders' claims as
// the tick reads them.
func TestReplayStatesAreTheProjectionsStates(t *testing.T) {
	t.Parallel()
	dsn := replayDSN(t)
	const name = "two-waiters-one-blocker"
	window := fixtureWindow(t, name)
	token, err := claim.ProjectToken(window.Project)
	if err != nil {
		t.Fatalf("the claim token of %s: %v", window.Project, err)
	}
	type moment struct {
		at     time.Time
		counts map[record.HolderState]int
	}
	var projected []moment
	rule := record.MainRule
	look := func(ctx context.Context, st *store.Store, at time.Time) error {
		return store.Transact(ctx, st.Pool(), func(ctx context.Context, tx pgx.Tx) error {
			records := record.NewStore()
			claims, err := st.ClaimsTx(ctx, tx)
			if err != nil {
				return err
			}
			holderClaims, err := records.HolderClaims(ctx, tx, token)
			if err != nil {
				return err
			}
			stranded, err := workflow.Stranded(ctx, tx, records, window.Project, window.GateSteps, claims, nil, at)
			if err != nil {
				return err
			}
			state, err := projection.Project(ctx, projection.Inputs{Tx: tx, Store: records, Project: window.Project,
				Steps: window.GateSteps, Claims: claims, Stranded: stranded, Now: at, SlotRule: rule, Cap: capAt(window, at),
				HolderClaims: holderClaims})
			if err != nil {
				return err
			}
			slots := state.Admission.Slots
			if slots == nil {
				return fmt.Errorf("the state at %s carries no slots reading", stamp(at))
			}
			projected = append(projected, moment{at: at, counts: map[record.HolderState]int{record.HolderWorking: slots.Working.Count,
				record.HolderStopped: slots.Stopped.Count, record.HolderPerson: slots.Person.Count, record.HolderIssue: slots.Issue.Count,
				record.HolderIdle: slots.Idle.Count}})
			return nil
		})
	}
	result, err := Run(context.Background(), RunConfig{Window: window, Rule: rule, Branch: MainBranch, Seed: 1, Slowdown: 1, PGDSN: dsn,
		Out: t.TempDir(), atSettled: look})
	if err != nil {
		t.Fatalf("replay %s: %v", name, err)
	}
	const moments = 10
	if len(projected) < moments {
		t.Fatalf("the replay of %s settled at %d moments, want at least %d", name, len(projected), moments)
	}
	for i := range moments {
		at := projected[i*len(projected)/moments]
		got := map[record.HolderState]int{}
		for _, state := range record.HolderStates {
			got[state] = 0
		}
		for _, c := range result.snapshot.Issues {
			if state, ok := stateAt(c, at.at); ok && state.Holds {
				got[state.Holder]++
			}
		}
		if fmt.Sprint(got) != fmt.Sprint(at.counts) {
			t.Errorf("at %s the replay counts its holders by state %v, and projection.Project over its database %v", stamp(at.at), got, at.counts)
		}
	}
}

// The two timelines of a replay agree (design 10.1; the design review's Q5): on every fixture
// window, on a window shaped as a deployment's (deploymentShapedWindow: issues placed at its
// start, holds read from failed claims, children filed in it, a stop and a boot), on a window
// with a drop from the merge queue (queueDropWindow), on two windows that hold each way an issue
// in flight comes to hold no slot (returnedPartWindow, restartAndTreeWindow), and on a window of a
// started parent waiting on its child while the root of its tree lingers (lingeringParentWindow),
// each measure of the line timeline, which BuildTrace reads from the replay's own log through the
// window's line kinds as it reads a record, stands within a third of R5's gap for that measure of
// the snapshot timeline, which the admission passes' own views give: 10% of the issues done and
// of the slots given each day, 0.05 of the share of priority taken, 30% of every other measure
// (the median wait for a slot and the hours of reading (a) among them), and, for each issue's
// first slot and its done, 60 minutes at the median. A gap of the line timeline wider than that
// would make a record's scorecard, which only the lines can give, err by more than a replay may.
// The deployment-shaped window holds a root that waits for its children again after its last
// child closed, with no line of the second wait, since the daemon tells a generation's wait once.
// In the queue-drop window both timelines name the merger's send-back one round of queue_drop and
// none of send_back: the replay writes the line of the pull request leaving the merge queue that
// the record shows before the move, as the trace of its own log needs to name the move's cause.
// In the two windows of an issue with no slot, the replay's daemon places each issue in the line
// as the record's line reading does (TestAnIssueWithNoSlotWaitsForOneOnlyWhereTheLinePlacesIt,
// TestARetriedHoldOwesItsStartAndATreeThatIsNotLiveWaitsForNoSlot): the snapshot timeline counts
// each issue's hours in both readings as that test counts them in the record. A wait that stands
// while its tree stops being live ends in both timelines where the record's facts end it, though
// no pass judges it from then: in the second of those windows the link wait of an issue whose
// tree stopped being live at 11:20 ends at 11:40 with its blocker's done, though the passes after
// 11:20 read no mirror row of that issue (unfinishedLinks), and in the lingering-parent window,
// replayed under a rule whose wait stops the phase, the wait of a started parent on its child ends
// at 11:40 with the child's close, though the passes after its root's leave at 11:20 judge no wait
// of it on its children (openChildren), and a child of it that the daemon recorded and that left
// the workflow in backlog holds it no longer (recordOf). Both timelines count only the waits of an
// issue in flight before its review (`replay.md`, "The measures"): in childrenFirstWindow
// no wait of ACME-2, a child that has not started whose own first phase its open child holds from
// 09:20 to 10:20, and in linkPastReviewWindow a link wait cut at the move past the review, though
// its blocker is done only later (issueWaits). Both timelines count each wait by what ended it as
// the window's own doc counts it, a parent's second wait on its children in one run of its tree
// among them, each a move into awaiting_children the replay's own log holds (childrenAgainWindow,
// buildTrace). And neither has an issue a hold of the daemon's line keeps from
// its first start wait for a slot (startingAt): ACME-2 of childrenFirstWindow while its own open
// child holds it, of acceptedAncestorWindow while its parent stands accepted, and of
// ancestryWindow, a queued root, while its Dispatch chain names a live root. Four windows hold the
// ways a reading of a wait can part from the daemon's facts, and both timelines count each wait as
// the window's doc counts it: no wait of an issue in flight whose blocker is already done, though a
// pass reads its row as another issue's blocker with no row of that blocker (doneBlockerWindow,
// unfinishedLinks); a children wait that begins again at the opening of a gate a new spec version
// closed, and not at the approval of another document (gateVersionWindow, gateReader); a link
// wait and a children wait that touch as one (touchingWaitsWindow, issueWaits); and a children
// wait the record told before the start as one, its telling marked in the start state
// (toldBeforeStartWindow, childrenToldMarks). The fixture windows' runs under main are held to these
// checks in TestFixtureWindows, and childrenFirstWindow, acceptedAncestorWindow and ancestryWindow
// in TestEveryHoldHasASourceInTheReplay, on the replays those tests make (checkTimelines).
func TestSnapshotAndLineTimelinesAgree(t *testing.T) {
	t.Parallel()
	dsn := replayDSN(t)
	t.Run("deployment-shaped", func(t *testing.T) {
		t.Parallel()
		timelinesAgree(t, dsn, deploymentShapedWindow(t), record.MainRule)
	})
	t.Run("queue-drop", func(t *testing.T) {
		t.Parallel()
		result := timelinesAgree(t, dsn, queueDropWindow(t), record.MainRule)
		for _, timeline := range []struct {
			name string
			card Scorecard
		}{{"snapshot", result.Scorecard}, {"line", result.LineScorecard}} {
			drop, sent := timeline.card.Loops[string(CauseQueueDrop)], timeline.card.Loops[string(CauseSendBack)]
			if drop.Rounds != 1 || drop.Hours == 0 || sent.Rounds != 0 {
				t.Errorf("the %s timeline counts %+v of queue_drop and %+v of send_back, want one round of queue_drop and none of send_back",
					timeline.name, drop, sent)
			}
		}
	})
	t.Run("returned-part", func(t *testing.T) {
		t.Parallel()
		result := timelinesAgree(t, dsn, returnedPartWindow(t), record.MainRule)
		assertSnapshotHours(t, result, []issueHours{{key: "ACME-1"}, {key: "ACME-2", b: 1, a: 1}, {key: "ACME-3", b: 2, a: 1.0 / 3}})
	})
	t.Run("restart-and-tree", func(t *testing.T) {
		t.Parallel()
		result := timelinesAgree(t, dsn, restartAndTreeWindow(t), record.MainRule)
		assertSnapshotHours(t, result, []issueHours{{key: "ACME-1", b: 1.5, a: 1.5}, {key: "ACME-2", b: 2}, {key: "ACME-3", b: 4.0 / 3}})
		assertTimelineWaits(t, result, map[WaitEnd]WaitHours{WaitLeft: {Count: 1, Hours: 2}, WaitBlockerDone: {Count: 1, Hours: 5.0 / 3}})
	})
	t.Run("lingering-parent", func(t *testing.T) {
		t.Parallel()
		result := timelinesAgree(t, dsn, lingeringParentWindow(t), record.MainRule)
		assertSnapshotHours(t, result, []issueHours{{key: "ACME-1", b: 2}, {key: "ACME-2", b: 3}})
		assertTimelineWaits(t, result, map[WaitEnd]WaitHours{WaitLeft: {Count: 1, Hours: 2}, WaitChildDone: {Count: 1, Hours: 5.0 / 3}})
	})
	t.Run("children-again", func(t *testing.T) {
		t.Parallel()
		result := timelinesAgree(t, dsn, childrenAgainWindow(t), record.MainRule)
		assertSnapshotHours(t, result, []issueHours{{key: "ACME-1", b: 11.0 / 3}, {key: "ACME-2", b: 1}})
		assertTimelineWaits(t, result, map[WaitEnd]WaitHours{WaitOpen: {Count: 1, Hours: 11.0 / 3}, WaitChildDone: {Count: 2, Hours: 1}})
	})
	t.Run("link-past-review", func(t *testing.T) {
		t.Parallel()
		result := timelinesAgree(t, dsn, linkPastReviewWindow(t), record.MainRule)
		assertSnapshotHours(t, result, []issueHours{{key: "ACME-1", b: 1.0 / 3}})
		assertTimelineWaits(t, result, map[WaitEnd]WaitHours{WaitMovedOn: {Count: 1, Hours: 1.0 / 3}})
	})
	t.Run("done-blocker", func(t *testing.T) {
		t.Parallel()
		result := timelinesAgree(t, dsn, doneBlockerWindow(t), record.MainRule)
		assertSnapshotHours(t, result, []issueHours{{key: "ACME-1", b: 239.0 / 60}})
		assertTimelineWaits(t, result, map[WaitEnd]WaitHours{WaitOpen: {Count: 1, Hours: 239.0 / 60}})
	})
	t.Run("gate-version", func(t *testing.T) {
		t.Parallel()
		// ACME-1 waits on ACME-2 from the gate's opening at 10:20 to its closing at 11:00, and again
		// from its opening at 12:00 to the window's end at 13:00.
		result := timelinesAgree(t, dsn, gateVersionWindow(t), record.MainRule)
		assertTimelineWaits(t, result, map[WaitEnd]WaitHours{WaitGateClosed: {Count: 1, Hours: 2.0 / 3}, WaitOpen: {Count: 1, Hours: 1}})
	})
	t.Run("touching-waits", func(t *testing.T) {
		t.Parallel()
		result := timelinesAgree(t, dsn, touchingWaitsWindow(t), record.MainRule)
		assertTimelineWaits(t, result, map[WaitEnd]WaitHours{WaitChildDone: {Count: 1, Hours: 11.0 / 6}})
	})
	t.Run("told-before-start", func(t *testing.T) {
		t.Parallel()
		result := timelinesAgree(t, dsn, toldBeforeStartWindow(t), record.MainRule)
		assertTimelineWaits(t, result, map[WaitEnd]WaitHours{WaitChildDone: {Count: 1, Hours: 1}})
	})
}

// assertTimelineWaits fails where either timeline of result counts the waits on other issues by
// what ended them otherwise than want: the same words, each with its count and its hours.
func assertTimelineWaits(t *testing.T, result RunResult, want map[WaitEnd]WaitHours) {
	t.Helper()
	for _, timeline := range []struct {
		name string
		card Scorecard
	}{{"snapshot", result.Scorecard}, {"line", result.LineScorecard}} {
		same := len(timeline.card.Waits) == len(want)
		for word, hours := range want {
			got, ok := timeline.card.Waits[string(word)]
			same = same && ok && got.Count == hours.Count && math.Abs(got.Hours-hours.Hours) <= fixtureTolerance
		}
		if !same {
			t.Errorf("the %s timeline counts the waits %+v, want %+v", timeline.name, timeline.card.Waits, want)
		}
	}
}

// assertWaitsForNoSlot fails where key, an issue a hold of the daemon's line keeps from its first
// start at at, waits for a slot then in the snapshot timeline of result, which the daemon's own
// passes give, or in the line timeline of window's record, which Score reads.
func assertWaitsForNoSlot(t *testing.T, window Window, result RunResult, key string, at time.Time) {
	t.Helper()
	trace, err := BuildTrace(window)
	if err != nil {
		t.Fatalf("build the trace: %v", err)
	}
	for _, timeline := range []struct {
		name string
		tl   Timeline
	}{{"snapshot", result.snapshot}, {"line", recordLineTimeline(trace)}} {
		c := timeline.tl.Issues[key]
		if c == nil {
			continue
		}
		if state, ok := stateAt(c, at); ok && waitsForSlot(state) {
			t.Errorf("the %s timeline has %s wait for a slot at %s, in %+v: the daemon's line holds it", timeline.name, key, stamp(at), state)
		}
	}
}

// queueDropWindow is a window from 09:00 to 15:00 with a drop from the merge queue. ACME-1, a root,
// is promoted at 09:05 and works 15 to 30 minutes in each phase to merging at 11:40. At 12:00 its
// pull request leaves the merge queue, and at 12:10 its merger sends it back to implementing. It
// works through each phase again to merging at 14:00, and merges at 14:20.
func queueDropWindow(t *testing.T) Window {
	b := newWindowBuilder(t, clock(9, 0), clock(15, 0))
	b.boot(clock(9, 0))
	b.file(clock(9, 5), "ACME-1", filing{Title: "Export the widget list", Status: "todo", Rank: "a0", Labels: handedToLegion})
	b.rootPromoted(clock(9, 5), "ACME-1")
	b.rootDesigned(clock(9, 20), "ACME-1")
	b.segmentEnds(clock(9, 50), "ACME-1", phase.Planning, phase.Implementing, workflow.TriggerPlannerCompleted)
	toMerging := func(implemented time.Time) {
		b.segmentEnds(implemented, "ACME-1", phase.Implementing, phase.Testing, workflow.TriggerImplementationReady)
		b.segmentEnds(implemented.Add(20*time.Minute), "ACME-1", phase.Testing, phase.Reviewing, workflow.TriggerTesterPassed)
		b.segmentEnds(implemented.Add(40*time.Minute), "ACME-1", phase.Reviewing, phase.Accepting, workflow.TriggerReviewApproved)
		b.segmentEnds(implemented.Add(60*time.Minute), "ACME-1", phase.Accepting, phase.Retro, workflow.TriggerSignOff)
		b.segmentEnds(implemented.Add(80*time.Minute), "ACME-1", phase.Retro, phase.Merging, workflow.TriggerRetroCompleted)
	}
	toMerging(clock(10, 20))
	b.info(clock(12, 0), "workflow: a pull request in merging left the merge queue; the daemon reads it from GitHub once", "issue", "ACME-1",
		"pullRequest", 41, "head", "3c9e1f0a7b2d4e5f6a7b8c9d0e1f2a3b4c5d6e7f")
	b.segmentEnds(clock(12, 10), "ACME-1", phase.Merging, phase.Implementing, workflow.TriggerBackward)
	toMerging(clock(12, 40))
	b.segmentEnds(clock(14, 20), "ACME-1", phase.Merging, phase.Done, workflow.TriggerPullRequestMerged)
	b.leaves(clock(14, 20), "ACME-1", record.DoneStatus)
	return b.build()
}

// timelinesAgree replays window under rule on the main branch, at seed 1 and f 1, holds its two
// timelines to agree (checkTimelines) and answers the replay's result.
func timelinesAgree(t *testing.T, dsn string, window Window, rule record.SlotRule) RunResult {
	t.Helper()
	result, err := Run(context.Background(), RunConfig{Window: window, Rule: rule, Branch: MainBranch, Seed: 1, Slowdown: 1, PGDSN: dsn,
		Out: t.TempDir()})
	if err != nil {
		t.Fatalf("the replay failed: %v", err)
	}
	checkTimelines(t, result)
	return result
}

// checkTimelines fails for each measure in which result's line timeline stands further from its
// snapshot timeline than a third of R5's gap for it, and where the median gap of each issue's first
// slot and done is above a third of R5's 60 minutes (TestSnapshotAndLineTimelinesAgree).
func checkTimelines(t *testing.T, result RunResult) {
	t.Helper()
	var timeErrors []float64
	for _, gap := range result.Gaps {
		if strings.HasPrefix(gap.Measure, "firstSlot[") || strings.HasPrefix(gap.Measure, "doneAt[") {
			timeErrors = append(timeErrors, math.Abs(gap.Gap()))
			continue
		}
		if allowed := validationGap(gap.Measure, gap.Snapshot) / 3; math.Abs(gap.Gap()) > allowed+fixtureTolerance {
			t.Errorf("%s is %v in the snapshot timeline and %v in the line timeline, a gap of %v, above a third of R5's gap, %v",
				gap.Measure, gap.Snapshot, gap.Line, gap.Gap(), allowed)
		}
	}
	if errorMinutes := median(timeErrors); errorMinutes > 60.0/3+fixtureTolerance {
		t.Errorf("the median gap of each issue's first slot and done between the two timelines is %v minutes, above a third of R5's 60",
			errorMinutes)
	}
}

// validationGap is the gap R5 allows the replay of a measure whose recorded value is recorded
// (R5Gap), and 30% of a number no test of R5 reads, as of every measure R5 does not name.
func validationGap(measure string, recorded float64) float64 {
	gap, tested := R5Gap(measure)
	if !tested {
		gap = gapThirty
	}
	return gap.Allowed(recorded)
}

// coveredChildWindow is a window from 09:00 to 13:00 under a cap of two in which a started parent
// waits on a child filed under it while it implements. ACME-1, a root, is promoted at 09:05, plans
// from 09:20 and implements from 09:50, its one turn running to the move to testing at 10:40. At
// 10:00 a person files ACME-2 under it, todo, which starts at once and works 10 minutes in each
// phase to its merge at 11:10.
func coveredChildWindow(t *testing.T) Window {
	b := newWindowBuilder(t, clock(9, 0), clock(13, 0))
	b.capped(2)
	b.boot(clock(9, 0))
	promotedRoot(b, clock(9, 5), "ACME-1", "a0")
	b.rootDesigned(clock(9, 20), "ACME-1")
	b.segmentEnds(clock(9, 50), "ACME-1", phase.Planning, phase.Implementing, workflow.TriggerPlannerCompleted)
	b.file(clock(10, 0), "ACME-2", filing{Title: "Name the export's columns", Parent: "ACME-1", Tree: "ACME-1", Status: "todo", Rank: "a1"})
	b.childStarts(clock(10, 0), "ACME-2")
	b.through("ACME-2", phase.Planning, moveAt(10, 10, phase.Implementing, workflow.TriggerPlannerCompleted),
		moveAt(10, 20, phase.Testing, workflow.TriggerImplementationReady), moveAt(10, 30, phase.Reviewing, workflow.TriggerTesterPassed),
		moveAt(10, 40, phase.Accepting, workflow.TriggerReviewApproved), moveAt(10, 50, phase.Retro, workflow.TriggerSignOff),
		moveAt(11, 0, phase.Merging, workflow.TriggerRetroCompleted))
	b.turnEnds(clock(11, 10), "ACME-2", claim.RoleMerger)
	b.moved(clock(11, 10), "ACME-2", phase.Merging, phase.Done, workflow.TriggerPullRequestMerged)
	b.leaves(clock(11, 10), "ACME-2", record.DoneStatus)
	b.segmentEnds(clock(10, 40), "ACME-1", phase.Implementing, phase.Testing, workflow.TriggerImplementationReady)
	return b.build()
}

// Under cover a started parent that waits on its open child keeps its phase, and the child starts
// on the parent's slot (record.SlotRule.StartedParents, blockers.CoverWaits). That wait is a wait
// on another issue, which the measures count by what ended it (spec R4; `replay.md`, "The
// measures", waits), as they count the same parent's wait in awaiting_children under main. The test
// replays coveredChildWindow under both rules, each with its two timelines held to agree
// (timelinesAgree). Under cover ACME-2, filed under ACME-1 at 10:00 while ACME-1 implements, starts
// on ACME-1's slot and holds it for ACME-1 to its done at 11:10; ACME-1 implements on to 10:40 and
// moves to testing, whose start waits for the cover's end. Under main ACME-1 moves into
// awaiting_children at 10:00, gives its slot back, and comes back to implementing at ACME-2's done.
// Under each rule both timelines count one wait of child_done, the 70 minutes from 10:00 to 11:10,
// and the snapshot timeline counts ACME-1 those 70 minutes in reading (b), covered under cover and
// in awaiting_children with no slot under main, and none in reading (a).
func TestAParentsWaitOnItsOpenChildCountsUnderCover(t *testing.T) {
	t.Parallel()
	dsn := replayDSN(t)
	for _, rule := range []record.SlotRule{replayedRule(t, "cover"), record.MainRule} {
		t.Run(rule.Name, func(t *testing.T) {
			t.Parallel()
			result := timelinesAgree(t, dsn, coveredChildWindow(t), rule)
			assertTimelineWaits(t, result, map[WaitEnd]WaitHours{WaitChildDone: {Count: 1, Hours: 7.0 / 6}})
			assertSnapshotHours(t, result, []issueHours{{key: "ACME-1", b: 7.0 / 6}})
		})
	}
}

// gateHoursWindow is a window from 09:00 to 12:00 in a deployment that gates the spec. ACME-1, a
// root promoted at 09:05, has its architect write version 1 of its spec at 09:17 and register it at
// 09:20 unapproved, which closes its gate; a person approves the version at 10:20, the gate opens
// and the root's planning begins; its architect writes version 2 at 11:00, which closes the gate
// again to the window's end. The gate stands closed from 09:20 to 10:20 and from 11:00 to 12:00.
func gateHoursWindow(t *testing.T) Window {
	b := newWindowBuilder(t, clock(9, 0), clock(12, 0))
	b.gated(record.DesignStepSpec)
	b.boot(clock(9, 0))
	b.file(clock(9, 5), "ACME-1", filing{Title: "Export the widget list", Status: "todo", Rank: "a0", Labels: handedToLegion})
	b.rootPromoted(clock(9, 5), "ACME-1")
	b.specWritten(clock(9, 17), "ACME-1")
	b.turnEnds(clock(9, 20), "ACME-1", claim.RoleArchitect)
	b.registered(clock(9, 20), "ACME-1", false)
	b.approves(clock(10, 20), "ACME-1", "")
	b.gateOpened(clock(10, 20), "ACME-1", intake.DispatchArtifactApproved, 1)
	b.rootFirstPhase(clock(10, 20), "ACME-1")
	b.specRevised(clock(11, 0), "ACME-1", 2)
	b.gateClosed(clock(11, 0), "ACME-1", intake.DispatchArtifactVersion, 2)
	return b.build()
}

// A scorecard gives each tree the hours its design gate stood closed in the window, from the gate's
// first fact on, read by one code from the record's trace and from a replay's own log (the red
// team's finding 5), so validate can name a tree whose gate the replay holds otherwise than the
// record. The test reads gateHoursWindow, whose ACME-1 gate stands closed from 09:20 to 10:20 and
// from 11:00 to 12:00: the record's scorecard gives it 2 hours, and so does the line scorecard of its
// replay under main, the rule that recorded it. Neither gives another tree.
func TestTheScorecardGivesEachTreesClosedGateHours(t *testing.T) {
	t.Parallel()
	window := gateHoursWindow(t)
	card, err := Score(window)
	if err != nil {
		t.Fatal(err)
	}
	result, err := Run(context.Background(), RunConfig{Window: window, Rule: mainRule(t), Branch: MainBranch, Seed: 1, Slowdown: 1,
		PGDSN: replayDSN(t), Out: t.TempDir()})
	if err != nil {
		t.Fatalf("replay the window: %v", err)
	}
	for _, c := range []struct {
		name  string
		hours map[string]float64
	}{{"the record's scorecard", card.GateClosedHours}, {"the replay's line scorecard", result.LineScorecard.GateClosedHours}} {
		if len(c.hours) != 1 || math.Abs(c.hours["ACME-1"]-2) > 1.0/3600 {
			t.Errorf("%s gives the gates closed %v hours, want ACME-1's alone, 2", c.name, c.hours)
		}
	}
}

// doneByAnyoneWindow is a window from 09:00 to 12:00 under main. ACME-1, a root promoted at 09:00,
// works 15 minutes in each phase to its merge at 11:00, the done Legion makes. ACME-2, which nobody
// hands to Legion, is filed at 09:10 and closed done by a person at 10:30. ACME-3, filed at 08:10,
// was closed by a person at 08:40, before the window.
func doneByAnyoneWindow(t *testing.T) Window {
	b := newWindowBuilder(t, clock(9, 0), clock(12, 0))
	b.boot(clock(8, 0))
	b.file(clock(8, 10), "ACME-3", filing{Title: "Paint the shelf numbers", Status: "todo", Rank: "z3"})
	b.edit(clock(8, 40), "ACME-3", setStatus(record.DoneStatus))
	b.file(clock(9, 0), "ACME-1", filing{Title: "Export the widget list", Status: "todo", Rank: "a1", Labels: handedToLegion})
	b.file(clock(9, 10), "ACME-2", filing{Title: "Sweep the loading bay", Status: "todo", Rank: "z2"})
	b.rootThroughReview(clock(9, 0), "ACME-1")
	stepThrough(b, builtStep{clock(10, 15), "ACME-1", phase.Reviewing, phase.Accepting, workflow.TriggerReviewApproved},
		builtStep{clock(10, 30), "ACME-1", phase.Accepting, phase.Retro, workflow.TriggerSignOff})
	b.edit(clock(10, 30), "ACME-2", setStatus(record.DoneStatus))
	stepThrough(b, builtStep{clock(10, 45), "ACME-1", phase.Retro, phase.Merging, workflow.TriggerRetroCompleted},
		builtStep{clock(11, 0), "ACME-1", phase.Merging, phase.Done, workflow.TriggerPullRequestMerged})
	return b.build()
}

// A scorecard gives, beside the issues Legion moves to done (done), the issues of the window's
// project Dispatch shows reaching done in the window by any actor (the red team's finding 9): work
// done outside Legion is kept as recorded under every rule, and a reader weighs Legion's done
// against it. The test reads doneByAnyoneWindow: Legion's done is ACME-1's, and done by anyone
// ACME-1's and ACME-2's, ACME-3's coming before the window, in the record's scorecard and in the
// line scorecard of its replay under main, the rule that recorded it.
func TestTheScorecardCountsTheIssuesDoneByAnyone(t *testing.T) {
	t.Parallel()
	window := doneByAnyoneWindow(t)
	card, err := Score(window)
	if err != nil {
		t.Fatal(err)
	}
	result, err := Run(context.Background(), RunConfig{Window: window, Rule: mainRule(t), Branch: MainBranch, Seed: 1, Slowdown: 1,
		PGDSN: replayDSN(t), Out: t.TempDir()})
	if err != nil {
		t.Fatalf("replay the window: %v", err)
	}
	for _, c := range []struct {
		name string
		card Scorecard
	}{{"the record's scorecard", card}, {"the replay's line scorecard", result.LineScorecard}} {
		if c.card.Done != 1 || c.card.DoneByAnyone != 2 {
			t.Errorf("%s gives %d done and %d done by anyone, want 1 and 2", c.name, c.card.Done, c.card.DoneByAnyone)
		}
	}
}
