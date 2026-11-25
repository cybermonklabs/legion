package workflow

import (
	"fmt"
	"maps"
	"reflect"
	"slices"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

var allWorkflowTriggers = []TriggerKind{
	TriggerGateOpened, TriggerPlannerCompleted, TriggerImplementationReady, TriggerTesterPassed,
	TriggerTesterFailed, TriggerReviewApproved, TriggerReviewRejected, TriggerStepPassed, TriggerStepFailed,
	TriggerRetroCompleted, TriggerPullRequestMerged, TriggerSignOff, TriggerBackward,
}

// testLines are the lines the table tests hold TableOf to: today's, which an issue with no plan
// rows walks, and seven that a plan of this release walks (plan.Line): a task of an implementer
// and a reviewer, one of a planner, an implementer and the owner's acceptance, one of an
// implementer alone, three in another order than today's: a review before the test, the owner's
// acceptance before the review, and a planner after the implementer, and one of an implementer
// and a role of the deployment, which runs as step.
var testLines = [][]phase.Phase{
	phase.Order,
	{phase.Implementing, phase.Reviewing, phase.Retro, phase.Merging},
	{phase.Planning, phase.Implementing, phase.Accepting, phase.Retro, phase.Merging},
	{phase.Implementing, phase.Retro, phase.Merging},
	{phase.Implementing, phase.Reviewing, phase.Testing, phase.Retro, phase.Merging},
	{phase.Implementing, phase.Accepting, phase.Reviewing, phase.Retro, phase.Merging},
	{phase.Implementing, phase.Planning, phase.Retro, phase.Merging},
	{phase.Implementing, phase.Step, phase.Retro, phase.Merging},
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

// The table of a line that ends at its last step, the line of a task another task needs
// (plan.Line of a task that does not merge), moves that step's pass to done, the task keeping the
// place of its last step, and holds no row out of a retro or a merging it does not walk. A line
// that merges keeps its retro's move into merging and its merge's move to done.
//
// The fault it catches is a task that does not merge and stands at its last step for ever, or a
// table built past the end of its line (its control: TableOf moving the last step on to the next
// phase of the line, which there is none of).
func TestTheLastStepOfALineThatDoesNotMergeMovesToDone(t *testing.T) {
	for _, tc := range []struct {
		line    []phase.Phase
		trigger TriggerKind
	}{
		{[]phase.Phase{phase.Implementing, phase.Reviewing}, TriggerReviewApproved},
		{[]phase.Phase{phase.Implementing}, TriggerImplementationReady},
		{[]phase.Phase{phase.Implementing, phase.Step}, TriggerStepPassed},
	} {
		last := len(tc.line)
		place := record.TaskPlace{Kind: record.PlaceStep, Position: last}
		var done []Row
		for _, row := range TableOf(tc.line, record.LinePlaces(tc.line, false)) {
			if row.From == phase.Retro || row.From == phase.Merging {
				t.Fatalf("the table of %v holds the row %s/%s -> %s, out of a phase the line does not walk", tc.line, row.From, row.Trigger, row.To)
			}
			if row.To == phase.Done {
				done = append(done, row)
			}
		}
		if len(done) != 1 || done[0].From != tc.line[last-1] || done[0].Trigger != tc.trigger || done[0].FromPlace != place || done[0].ToPlace != place {
			t.Fatalf("the rows of %v into done = %+v, want the one pass of its last step at %+v, keeping that place", tc.line, done, place)
		}
	}
	mergingLine := []phase.Phase{phase.Implementing, phase.Retro, phase.Merging}
	merges := TableOf(mergingLine, record.LinePlaces(mergingLine, false))
	var into []phase.Phase
	for _, row := range merges {
		if row.Trigger == TriggerRetroCompleted || row.Trigger == TriggerPullRequestMerged {
			into = append(into, row.To)
		}
	}
	if !slices.Equal(into, []phase.Phase{phase.Merging, phase.Done}) {
		t.Fatalf("the retro's and the merge's rows of a line that merges move into %v, want merging then done", into)
	}
}

func TestEveryLineHasARowForEachPhaseAndTrigger(t *testing.T) {
	t.Parallel()
	for _, line := range testLines {
		if err := validateTable(line, TableOf(line, record.LinePlaces(line, false))); err != nil {
			t.Errorf("line %v: %v", line, err)
		}
	}
}

func TestDeletingAnyTransitionRowBreaksCoverage(t *testing.T) {
	t.Parallel()
	for _, line := range testLines {
		table := TableOf(line, record.LinePlaces(line, false))
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
		fromTable := map[string]bool{}
		for _, row := range TableOf(line, record.LinePlaces(line, false)) {
			if row.Trigger == TriggerBackward {
				fromTrigger = append(fromTrigger, phase.Pair(row.From, row.To))
			}
			if at, to := slices.Index(line, row.From), slices.Index(line, row.To); at >= 0 && to >= 0 && to < at {
				fromTable[phase.Pair(row.From, row.To)] = true
			}
		}
		if !slices.Equal(fromTrigger, want) {
			t.Fatalf("line %v: %s rows = %v, want %v", line, TriggerBackward, fromTrigger, want)
		}
		if !maps.Equal(fromTable, setOf(want)) {
			t.Fatalf("line %v: table moves back = %v, its own pairs = %v", line, sortedPairs(fromTable), want)
		}
	}
	if len(phase.BackwardPairs()) != len(phase.Order)*(len(phase.Order)-1)/2 {
		t.Fatalf("phase.BackwardPairs has %d pairs, want every pair of phase.Order", len(phase.BackwardPairs()))
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

func setOf(pairs []string) map[string]bool {
	set := make(map[string]bool, len(pairs))
	for _, pair := range pairs {
		set[pair] = true
	}
	return set
}

// A line in another order than today's goes back by the line, not by phase.Order: the table of
// `implementer reviewer tester` moves back from testing to reviewing, the review before it, and
// holds no move back from reviewing to testing, which is its move on (review_approved). A planner
// after the implementer moves on into retro, and goes back to implementing on a backward move.
// The control is the table of today's backward rows (phase.BackwardPairs), which holds
// reviewing-testing and lacks testing-reviewing.
func TestTheTableOfALineInAnyOrderGoesBackByTheLine(t *testing.T) {
	t.Parallel()
	rowsOf := func(line []phase.Phase, trigger TriggerKind) []string {
		var pairs []string
		for _, row := range TableOf(line, record.LinePlaces(line, false)) {
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
	if slices.Contains(backward, "reviewing-testing") {
		t.Fatalf("the table of %v moves back from reviewing to testing, its move on", reviewFirst)
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

// Two steps of roles of the deployment both run as step, so the rows of a stored walk are keyed by
// the place: the line of `implementer checker linter` moves on from step 2 to step 3 and from
// step 3 to its retro, a failed step goes back to each step before it (step 3 to step 2 and to
// step 1, step 2 to step 1 alone), and a move back goes from step 3 to step 2. TableOf builds its
// rows by the positions of the line, not by the first place of each phase, and StoredTableOf adds
// the fail rows to each earlier step that the fail row to the implementer does not reach.
func TestTwoPlacesThatRunAsStepAreTwoRows(t *testing.T) {
	line := []phase.Phase{phase.Implementing, phase.Step, phase.Step, phase.Retro, phase.Merging}
	step := func(n int) record.TaskPlace { return record.TaskPlace{Kind: record.PlaceStep, Position: n} }
	retro := record.TaskPlace{Kind: record.PlaceRetro}
	targets := func(rows []Row, trigger TriggerKind, from record.TaskPlace) []record.TaskPlace {
		var to []record.TaskPlace
		for _, row := range rows {
			if row.Trigger == trigger && row.FromPlace == from {
				to = append(to, row.ToPlace)
			}
		}
		return to
	}
	places := record.LinePlaces(line, false)
	stored, plain := StoredTableOf(line, places), TableOf(line, places)
	for _, tc := range []struct {
		name    string
		rows    []Row
		trigger TriggerKind
		from    record.TaskPlace
		want    []record.TaskPlace
	}{
		{"TableOf: step 2 passes to step 3", plain, TriggerStepPassed, step(2), []record.TaskPlace{step(3)}},
		{"TableOf: step 3 passes to the retro", plain, TriggerStepPassed, step(3), []record.TaskPlace{retro}},
		{"StoredTableOf: step 2 passes to step 3", stored, TriggerStepPassed, step(2), []record.TaskPlace{step(3)}},
		{"StoredTableOf: step 3 passes to the retro", stored, TriggerStepPassed, step(3), []record.TaskPlace{retro}},
		{"StoredTableOf: step 3 fails to step 1 and to step 2", stored, TriggerStepFailed, step(3), []record.TaskPlace{step(1), step(2)}},
		{"StoredTableOf: step 2 fails to step 1 alone", stored, TriggerStepFailed, step(2), []record.TaskPlace{step(1)}},
		{"StoredTableOf: step 3 moves back to step 1 and to step 2", stored, TriggerBackward, step(3), []record.TaskPlace{step(1), step(2)}},
	} {
		if got := targets(tc.rows, tc.trigger, tc.from); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s: %s from %+v goes to %+v, want %+v", tc.name, tc.trigger, tc.from, got, tc.want)
		}
	}
}

func sortedPairs(pairs map[string]bool) []string {
	return slices.Sorted(maps.Keys(pairs))
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
	case TriggerStepPassed, TriggerStepFailed:
		return in(phase.Step)
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
	case TriggerTesterFailed, TriggerReviewRejected, TriggerStepFailed:
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
	return fmt.Sprintf("%s does not advance %s", trigger, current)
}
