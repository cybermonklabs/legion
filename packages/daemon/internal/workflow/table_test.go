package workflow

import (
	"fmt"
	"slices"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/phase"
)

var allWorkflowTriggers = []TriggerKind{
	TriggerGateOpened, TriggerPlannerCompleted, TriggerImplementationReady, TriggerTesterPassed,
	TriggerTesterFailed, TriggerReviewApproved, TriggerReviewRejected, TriggerRetroCompleted,
	TriggerPullRequestMerged, TriggerSignOff, TriggerBackward,
}

// testLines are the lines the table tests hold TableOf to: today's, which an issue with no plan
// rows walks, and six that a plan of this release walks (plan.Line): a task of an implementer and
// a reviewer, one of a planner, an implementer and the owner's acceptance, one of an implementer
// alone, and three in another order than today's: a review before the test, the owner's
// acceptance before the review, and a planner after the implementer.
var testLines = [][]phase.Phase{
	phase.Order,
	{phase.Implementing, phase.Reviewing, phase.Retro, phase.Merging},
	{phase.Planning, phase.Implementing, phase.Accepting, phase.Retro, phase.Merging},
	{phase.Implementing, phase.Retro, phase.Merging},
	{phase.Implementing, phase.Reviewing, phase.Testing, phase.Retro, phase.Merging},
	{phase.Implementing, phase.Accepting, phase.Reviewing, phase.Retro, phase.Merging},
	{phase.Implementing, phase.Planning, phase.Retro, phase.Merging},
}

// todaysTable is the table the workflow walked before an issue's plan picked its line, written
// out row by row as it stood: the control that TableOf(phase.Order) changes nothing for an issue
// with no plan rows.
func todaysTable() []Row {
	rows := []Row{
		{From: phase.Admitted, Trigger: TriggerGateOpened, Guard: allow, To: phase.Planning, Effects: []EffectKind{EffectStart, EffectNotice}},
		{From: phase.Planning, Trigger: TriggerPlannerCompleted, Guard: allow, To: phase.Implementing, Effects: []EffectKind{EffectStart, EffectNotice}},
		{From: phase.Implementing, Trigger: TriggerImplementationReady, Guard: func(s Snapshot) bool { return s.HasOpenOrMergedPR }, To: phase.Testing, Effects: []EffectKind{EffectStart, EffectNotice}},
		{From: phase.Testing, Trigger: TriggerTesterPassed, Guard: allow, To: phase.Reviewing, Effects: []EffectKind{EffectStart, EffectNotice}},
		{From: phase.Testing, Trigger: TriggerTesterFailed, Guard: allow, To: phase.Implementing, Effects: []EffectKind{EffectStart, EffectNotice}},
		{From: phase.Reviewing, Trigger: TriggerReviewApproved, Guard: allow, To: phase.Accepting, Effects: []EffectKind{EffectNotice}},
		{From: phase.Accepting, Trigger: TriggerSignOff, Guard: allow, To: phase.Retro, Effects: []EffectKind{EffectStart, EffectNotice}},
		{From: phase.Reviewing, Trigger: TriggerReviewRejected, Guard: allow, To: phase.Implementing, Effects: []EffectKind{EffectStart, EffectNotice}},
		{From: phase.Retro, Trigger: TriggerRetroCompleted, Guard: allow, To: phase.Merging, Effects: []EffectKind{EffectStart, EffectNotice}},
		{From: phase.Merging, Trigger: TriggerPullRequestMerged, Guard: allow, To: phase.Done, Effects: []EffectKind{EffectSuspend, EffectNotice, EffectLinger}},
	}
	for _, text := range phase.BackwardPairs() {
		from, to, _ := phase.ParsePair(text)
		rows = append(rows, Row{From: from, Trigger: TriggerBackward, Guard: allow, To: to, Effects: []EffectKind{EffectStart, EffectNotice}})
	}
	return rows
}

// An issue with no plan rows moves by the table its walk reads (todaysWalk, Walk.table), and that
// table is today's, row for row and in today's order: the same phases, trigger, target and
// effects, and a guard that answers every snapshot as today's does (the one guard that reads
// anything reads the pull request).
func TestAWalkWithNoPlanRowsMovesByTodaysTable(t *testing.T) {
	t.Parallel()
	want, got := todaysTable(), todaysWalk().table()
	if len(got) != len(want) {
		t.Fatalf("the table of a walk with no plan rows has %d rows, want today's %d", len(got), len(want))
	}
	snapshots := []Snapshot{{}, {HasOpenOrMergedPR: true}, {GateOpen: true}, {HasOpenOrMergedPR: true, GateOpen: true}}
	for i := range want {
		g, w := got[i], want[i]
		if g.From != w.From || g.Trigger != w.Trigger || g.To != w.To || !slices.Equal(g.Effects, w.Effects) {
			t.Fatalf("row %d = %s/%s -> %s %v, want today's %s/%s -> %s %v", i, g.From, g.Trigger, g.To, g.Effects, w.From, w.Trigger, w.To, w.Effects)
		}
		if g.Guard == nil {
			t.Fatalf("row %d (%s/%s) has no guard", i, g.From, g.Trigger)
		}
		for _, s := range snapshots {
			if g.Guard(s) != w.Guard(s) {
				t.Fatalf("row %d (%s/%s) guards %+v %t, today's %t", i, g.From, g.Trigger, s, g.Guard(s), w.Guard(s))
			}
		}
	}
}

func TestEveryLineHasARowForEachPhaseAndTrigger(t *testing.T) {
	t.Parallel()
	for _, line := range testLines {
		if err := validateTable(line, TableOf(line)); err != nil {
			t.Errorf("line %v: %v", line, err)
		}
	}
}

func TestDeletingAnyTransitionRowBreaksCoverage(t *testing.T) {
	t.Parallel()
	for _, line := range testLines {
		table := TableOf(line)
		if len(table) == 0 {
			t.Fatalf("line %v has no rows", line)
		}
		for index := range table {
			rows := append([]Row{}, table[:index]...)
			rows = append(rows, table[index+1:]...)
			if err := validateTable(line, rows); err == nil {
				t.Fatalf("line %v: deleting %s/%s -> %s left the table covered", line, table[index].From, table[index].Trigger, table[index].To)
			}
		}
	}
}

// A line moves back by its own order: a backward row goes from each phase of the line to each
// phase before it in that line, each phase paired with every phase before it, in the line's order.
// For a line that keeps today's order (a subsequence of phase.Order) those are the pairs of
// phase.BackwardPairs whose two phases it holds, in that list's order, so today's line holds them
// all; for a line in another order they are its own pairs, later to earlier, whatever
// phase.Order says of the two phases. Every row that moves an issue back by the line (a tester's
// fail, a review's request for changes) is one of those pairs, and the backward trigger carries
// them all: a worker may ask for any move back the line allows.
func TestBackwardRowsOfEachLineAreItsOwnPairs(t *testing.T) {
	t.Parallel()
	for _, line := range testLines {
		var want []string
		for at, from := range line {
			for _, to := range line[:at] {
				want = append(want, phase.Pair(from, to))
			}
		}
		if len(want) == 0 {
			t.Fatalf("line %v has no backward pair to compare: the control read nothing", line)
		}
		if keepsTodaysOrder(line) {
			var fromPhase []string
			for _, pair := range phase.BackwardPairs() {
				if from, to, _ := phase.ParsePair(pair); slices.Contains(line, from) && slices.Contains(line, to) {
					fromPhase = append(fromPhase, pair)
				}
			}
			if !slices.Equal(want, fromPhase) {
				t.Fatalf("line %v keeps today's order, and its pairs %v are not its pairs of phase.BackwardPairs %v", line, want, fromPhase)
			}
		}
		var fromTrigger []string
		for _, row := range TableOf(line) {
			pair := phase.Pair(row.From, row.To)
			if row.Trigger == TriggerBackward {
				fromTrigger = append(fromTrigger, pair)
			}
			if returnsWork(row.Trigger) && !slices.Contains(want, pair) {
				t.Fatalf("line %v: the %s row %s moves the work to no phase before it on the line, want one of %v", line, row.Trigger, pair, want)
			}
		}
		if !slices.Equal(fromTrigger, want) {
			t.Fatalf("line %v: %s rows = %v, want %v", line, TriggerBackward, fromTrigger, want)
		}
	}
}

// keepsTodaysOrder says whether line is a subsequence of phase.Order.
func keepsTodaysOrder(line []phase.Phase) bool {
	last := -1
	for _, p := range line {
		at := slices.Index(phase.Order, p)
		if at <= last {
			return false
		}
		last = at
	}
	return true
}

// A line in another order than today's goes back by the line, not by phase.Order: the table of
// `implementer reviewer tester` moves back from testing to reviewing, the review before it, and
// holds no move back from reviewing to testing, which is its move on (review_approved). A planner
// after the implementer moves on into retro, and goes back to implementing on a backward move.
// The control is the table of today's line, phase.Order, whose backward rows hold
// reviewing-testing and lack testing-reviewing.
func TestTheTableOfALineInAnyOrderGoesBackByTheLine(t *testing.T) {
	t.Parallel()
	rowsOf := func(line []phase.Phase, trigger TriggerKind) []string {
		var pairs []string
		for _, row := range TableOf(line) {
			if row.Trigger == trigger {
				pairs = append(pairs, phase.Pair(row.From, row.To))
			}
		}
		return pairs
	}
	reviewFirst := []phase.Phase{phase.Implementing, phase.Reviewing, phase.Testing, phase.Retro, phase.Merging}
	want := []string{"reviewing-implementing", "testing-implementing", "testing-reviewing", "retro-implementing", "retro-reviewing",
		"retro-testing", "merging-implementing", "merging-reviewing", "merging-testing", "merging-retro"}
	backward := rowsOf(reviewFirst, TriggerBackward)
	if !slices.Equal(backward, want) {
		t.Fatalf("the backward rows of %v = %v, want %v", reviewFirst, backward, want)
	}
	if today := rowsOf(phase.Order, TriggerBackward); !slices.Contains(today, "reviewing-testing") || slices.Contains(today, "testing-reviewing") {
		t.Fatalf("control: the backward rows of today's line %v = %v, want reviewing-testing and no testing-reviewing", phase.Order, today)
	}
	if approved := rowsOf(reviewFirst, TriggerReviewApproved); !slices.Equal(approved, []string{"reviewing-testing"}) {
		t.Fatalf("review_approved of %v = %v, want reviewing-testing", reviewFirst, approved)
	}
	plannerAfter := []phase.Phase{phase.Implementing, phase.Planning, phase.Retro, phase.Merging}
	if completed := rowsOf(plannerAfter, TriggerPlannerCompleted); !slices.Equal(completed, []string{"planning-retro"}) {
		t.Fatalf("planner_completed of %v = %v, want planning-retro", plannerAfter, completed)
	}
	if !slices.Contains(rowsOf(plannerAfter, TriggerBackward), "planning-implementing") {
		t.Fatalf("the table of %v has no backward row planning-implementing: %v", plannerAfter, rowsOf(plannerAfter, TriggerBackward))
	}
}

// validateTable says why rows is not the whole table of line, nil when it is: every phase and
// trigger the line declares has a complete row into the phase that line sends it to, every
// backward edge of the line has its row, and no row stands where the line declares none.
func validateTable(line []phase.Phase, rows []Row) error {
	for _, current := range phase.All {
		for _, trigger := range allWorkflowTriggers {
			hasRow := false
			for _, row := range rows {
				if row.From != current || row.Trigger != trigger {
					continue
				}
				hasRow = true
				if row.To == "" || row.Guard == nil || len(row.Effects) == 0 {
					return fmt.Errorf("%s/%s is not a complete transition row", current, trigger)
				}
				if want, ok := wantTarget(line, current, trigger); ok && row.To != want {
					return fmt.Errorf("%s/%s moves to %s, want %s", current, trigger, row.To, want)
				}
			}
			if workflowTransitionRequired(line, current, trigger) {
				if !hasRow {
					return fmt.Errorf("%s/%s is missing a transition row", current, trigger)
				}
				continue
			}
			if hasRow {
				return fmt.Errorf("%s/%s has a row where %q says it is ignored", current, trigger, namedIgnore(current, trigger))
			}
			if namedIgnore(current, trigger) == "" {
				return fmt.Errorf("%s/%s has neither a row nor a named ignore", current, trigger)
			}
		}
	}
	for _, from := range phase.All {
		for _, to := range requiredBackwardTargets(line, from) {
			if !hasBackwardRow(rows, from, to) {
				return fmt.Errorf("%s/backward -> %s is missing a transition row", from, to)
			}
		}
	}
	for _, row := range rows {
		if !workflowTransitionRequired(line, row.From, row.Trigger) {
			return fmt.Errorf("%s/%s is not a declared transition", row.From, row.Trigger)
		}
		if row.Trigger == TriggerBackward && !containsPhase(requiredBackwardTargets(line, row.From), row.To) {
			return fmt.Errorf("%s/backward -> %s is not a declared backward edge", row.From, row.To)
		}
	}
	return nil
}

func hasBackwardRow(rows []Row, from, to phase.Phase) bool {
	for _, row := range rows {
		if row.From == from && row.Trigger == TriggerBackward && row.To == to {
			return true
		}
	}
	return false
}

// workflowTransitionRequired says whether line declares a row of from on trigger: the gate's
// opening out of admitted, each step phase of the line on its role's trigger, the returns of a
// tester and a reviewer the line holds, and a move back from each phase of the line with a phase
// before it. A head that conflicts with its base asks for no row: it moves no phase.
func workflowTransitionRequired(line []phase.Phase, from phase.Phase, trigger TriggerKind) bool {
	in := func(p phase.Phase) bool { return from == p && slices.Contains(line, p) }
	switch trigger {
	case TriggerGateOpened:
		return from == phase.Admitted
	case TriggerPlannerCompleted:
		return in(phase.Planning)
	case TriggerImplementationReady:
		return in(phase.Implementing)
	case TriggerTesterPassed, TriggerTesterFailed:
		return in(phase.Testing)
	case TriggerReviewApproved, TriggerReviewRejected:
		return in(phase.Reviewing)
	case TriggerRetroCompleted:
		return in(phase.Retro)
	case TriggerPullRequestMerged:
		return in(phase.Merging)
	case TriggerSignOff:
		return in(phase.Accepting)
	case TriggerBackward:
		return len(requiredBackwardTargets(line, from)) != 0
	default:
		return false
	}
}

// wantTarget is the phase line sends from to on trigger, ok false for a move back, which
// requiredBackwardTargets holds: the first phase of the line out of admitted, the next phase of the
// line on a step's own trigger, implementing on a return, and done on the merge.
func wantTarget(line []phase.Phase, from phase.Phase, trigger TriggerKind) (phase.Phase, bool) {
	switch trigger {
	case TriggerGateOpened:
		return line[0], true
	case TriggerTesterFailed, TriggerReviewRejected:
		return phase.Implementing, true
	case TriggerPullRequestMerged:
		return phase.Done, true
	case TriggerBackward:
		return "", false
	}
	at := slices.Index(line, from)
	if at < 0 || at+1 >= len(line) {
		return "", false
	}
	return line[at+1], true
}

// requiredBackwardTargets is every phase of line before from: where a move back from from can go.
func requiredBackwardTargets(line []phase.Phase, from phase.Phase) []phase.Phase {
	if at := slices.Index(line, from); at > 0 {
		return line[:at]
	}
	return nil
}

func containsPhase(phases []phase.Phase, wanted phase.Phase) bool {
	for _, candidate := range phases {
		if candidate == wanted {
			return true
		}
	}
	return false
}

func namedIgnore(current phase.Phase, trigger TriggerKind) string {
	if current == phase.AwaitingChildren {
		return awaitingChildrenIgnores[trigger]
	}
	return fmt.Sprintf("%s does not advance %s", trigger, current)
}

// awaitingChildrenIgnores is why each trigger leaves a parent in awaiting_children where it stands
// (dispatch://INFRA-3320 R9 to R12): the parent goes back to the start it held only at the
// admission pass that finds no open child under it, so the table has no row out of the phase, and
// a fact that would move a phase is recorded where it lands and starts nothing.
var awaitingChildrenIgnores = map[TriggerKind]string{
	TriggerGateOpened:          "a parent in awaiting_children has started its phases, so no gate's opening starts its first",
	TriggerPlannerCompleted:    "the planner's completion of a phase the wait stopped is refused, HANDOFF_NOT_CURRENT_PHASE",
	TriggerImplementationReady: "the implementer's completion of a phase the wait stopped is refused, HANDOFF_NOT_CURRENT_PHASE; a push of its moves the head and nothing else",
	TriggerTesterPassed:        "the tester's completion of a phase the wait stopped is refused, HANDOFF_NOT_CURRENT_PHASE",
	TriggerTesterFailed:        "the tester's completion of a phase the wait stopped is refused, HANDOFF_NOT_CURRENT_PHASE",
	TriggerReviewApproved:      "a review GitHub records during the wait is recorded and starts no phase",
	TriggerReviewRejected:      "a review GitHub records during the wait is recorded and starts no phase",
	TriggerRetroCompleted:      "no retro runs before the parent is accepted, after its children",
	TriggerPullRequestMerged:   "a merge during the wait is recorded on the pull request and read at the return",
	TriggerSignOff:             "sign_off on a parent in awaiting_children is refused, WAITING_ON_CHILDREN",
	TriggerBackward:            "request_backward_move on a parent in awaiting_children is refused, WAITING_ON_CHILDREN",
}

// Every trigger reaching a parent in awaiting_children is a named ignore: no row of the table
// leaves the phase, and validateTable (TestEveryLineHasARowForEachPhaseAndTrigger) holds each
// trigger there to its reason in awaitingChildrenIgnores. The two triggers of one act, a tester's
// pass and failure or a review's approval and rejection, share one reason.
func TestEveryTriggerInAwaitingChildrenIsANamedIgnore(t *testing.T) {
	t.Parallel()
	for _, row := range Table {
		if row.From == phase.AwaitingChildren {
			t.Errorf("the table holds a row out of awaiting_children (%s -> %s), want none: only the admission pass's return leaves it", row.Trigger, row.To)
		}
	}
}
