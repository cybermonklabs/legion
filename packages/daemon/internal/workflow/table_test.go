package workflow

import (
	"fmt"
	"maps"
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
// rows walks, and three that a plan of this release walks (plan.Line): a task of an implementer and
// a reviewer, one of a planner, an implementer and the owner's acceptance, and one of an
// implementer alone.
var testLines = [][]phase.Phase{
	phase.Order,
	{phase.Implementing, phase.Reviewing, phase.Retro, phase.Merging},
	{phase.Planning, phase.Implementing, phase.Accepting, phase.Retro, phase.Merging},
	{phase.Implementing, phase.Retro, phase.Merging},
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
	for _, line := range testLines {
		if err := validateTable(line, TableOf(line)); err != nil {
			t.Errorf("line %v: %v", line, err)
		}
	}
}

func TestDeletingAnyTransitionRowBreaksCoverage(t *testing.T) {
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

// The daemon counts a round for every row that moves an issue back (Engine.transition reads
// phase.MovesBack), and the config loader refuses a hook written for a pair outside
// phase.BackwardPairs. The two must name the same moves: a row added here and not there would be
// a move the daemon counts and the loader refuses, and a pair added there and not here a hook
// that never runs. A line of a plan moves back only between its own phases, so its table holds the
// pairs of phase.BackwardPairs whose two phases are both in it, and today's line holds them all.
func TestBackwardRowsOfEachLineAreBackwardPairs(t *testing.T) {
	for _, line := range testLines {
		fromTable := map[string]bool{}
		fromTrigger := map[string]bool{}
		for _, row := range TableOf(line) {
			if !phase.MovesBack(row.From, row.To) {
				continue
			}
			fromTable[phase.Pair(row.From, row.To)] = true
			if row.Trigger == TriggerBackward {
				fromTrigger[phase.Pair(row.From, row.To)] = true
			}
		}
		fromPhase := map[string]bool{}
		for _, pair := range phase.BackwardPairs() {
			if from, to, _ := phase.ParsePair(pair); slices.Contains(line, from) && slices.Contains(line, to) {
				fromPhase[pair] = true
			}
		}
		if len(fromPhase) == 0 {
			t.Fatalf("line %v has no backward pair to compare: the control read nothing", line)
		}
		if !maps.Equal(fromTable, fromPhase) {
			t.Fatalf("line %v: table backward moves = %v, its pairs of phase.BackwardPairs = %v", line, sortedPairs(fromTable), sortedPairs(fromPhase))
		}
		// The backward trigger carries every pair: a worker may ask for any move back the phases
		// allow, and a move back another trigger makes (a tester's fail, a review's request for
		// changes) is over a pair the backward trigger carries too, never one it is missing.
		if !maps.Equal(fromTrigger, fromPhase) {
			t.Fatalf("line %v: %s rows = %v, want %v", line, TriggerBackward, sortedPairs(fromTrigger), sortedPairs(fromPhase))
		}
	}
	if len(phase.BackwardPairs()) != len(phase.Order)*(len(phase.Order)-1)/2 {
		t.Fatalf("phase.BackwardPairs has %d pairs, want every pair of phase.Order", len(phase.BackwardPairs()))
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
// tester and a reviewer the line holds, a conflict from each phase of the line after implementing,
// and a move back from each phase of the line with a phase before it.
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
	return fmt.Sprintf("%s does not advance %s", trigger, current)
}
