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
	TriggerPullRequestMerged, TriggerSignOff, TriggerBackward, TriggerConflict,
}

// todaysTable is the transition table table.go wrote out by hand before the table was generated
// from a sequence (dispatch://INFRA-3570), copied here row for row: the rows every issue walked,
// and the rows the default sequence must still generate. It is the fixed point
// TestTheDefaultSequenceGeneratesTodaysTable reads the generated table against, so it is data and
// is never itself generated.
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
		{From: phase.Testing, Trigger: TriggerConflict, Guard: allow, To: phase.Implementing, Effects: []EffectKind{EffectStart, EffectNotice}},
		{From: phase.Reviewing, Trigger: TriggerConflict, Guard: allow, To: phase.Implementing, Effects: []EffectKind{EffectStart, EffectNotice}},
		{From: phase.Accepting, Trigger: TriggerConflict, Guard: allow, To: phase.Implementing, Effects: []EffectKind{EffectStart, EffectNotice}},
		{From: phase.Retro, Trigger: TriggerConflict, Guard: allow, To: phase.Implementing, Effects: []EffectKind{EffectStart, EffectNotice}},
		{From: phase.Merging, Trigger: TriggerConflict, Guard: allow, To: phase.Implementing, Effects: []EffectKind{EffectStart, EffectNotice}},
		{From: phase.Merging, Trigger: TriggerPullRequestMerged, Guard: allow, To: phase.Done, Effects: []EffectKind{EffectSuspend, EffectNotice, EffectLinger}},
	}
	for _, text := range phase.BackwardPairs() {
		from, to, _ := phase.ParsePair(text)
		rows = append(rows, Row{From: from, Trigger: TriggerBackward, Guard: allow, To: to, Effects: []EffectKind{EffectStart, EffectNotice}})
	}
	return rows
}

// rowKey names one row by what the engine looks it up by: its phase, its trigger and its target.
type rowKey struct {
	from    phase.Phase
	trigger TriggerKind
	to      phase.Phase
}

// guardSnapshots are every snapshot a guard can tell apart: a guard reads HasOpenOrMergedPR and
// nothing else of the row's own phase, so the two values of it answer for every snapshot.
var guardSnapshots = []Snapshot{{HasOpenOrMergedPR: false}, {HasOpenOrMergedPR: true}}

// firstRow is the row the engine takes for a lookup (Engine.row): the first, in the table's
// order, of the row's phase and trigger whose target matches — any, for the empty target — and
// whose guard lets the snapshot through.
func firstRow(rows []Row, from phase.Phase, trigger TriggerKind, target phase.Phase, snapshot Snapshot) (Row, bool) {
	for _, row := range rows {
		if row.From == from && row.Trigger == trigger && (target == "" || row.To == target) && (row.Guard == nil || row.Guard(snapshot)) {
			return row, true
		}
	}
	return Row{}, false
}

// sameTables says how got differs from want, "" where it does not: the same rows by phase,
// trigger and target, each row once, each with the same effects and a guard that answers every
// snapshot alike; and, read the way the engine reads a table, the same row for every lookup the
// engine can make — every phase, every trigger, every target and none, and every snapshot — which
// is what holds the order of the rows wherever the order decides a lookup.
func sameTables(got, want []Row) string {
	keyed := func(rows []Row) (map[rowKey]Row, string) {
		out := map[rowKey]Row{}
		for _, row := range rows {
			key := rowKey{row.From, row.Trigger, row.To}
			if _, twice := out[key]; twice {
				return nil, fmt.Sprintf("%s/%s -> %s is written twice", row.From, row.Trigger, row.To)
			}
			out[key] = row
		}
		return out, ""
	}
	gotRows, why := keyed(got)
	if why != "" {
		return "the generated table: " + why
	}
	wantRows, why := keyed(want)
	if why != "" {
		return "the expected table: " + why
	}
	for key, row := range wantRows {
		generated, ok := gotRows[key]
		if !ok {
			return fmt.Sprintf("%s/%s -> %s is missing", key.from, key.trigger, key.to)
		}
		if !slices.Equal(generated.Effects, row.Effects) {
			return fmt.Sprintf("%s/%s -> %s has effects %v, want %v", key.from, key.trigger, key.to, generated.Effects, row.Effects)
		}
		for _, snapshot := range guardSnapshots {
			if generated.Guard(snapshot) != row.Guard(snapshot) {
				return fmt.Sprintf("%s/%s -> %s guards %+v with %t, want %t", key.from, key.trigger, key.to, snapshot, generated.Guard(snapshot), row.Guard(snapshot))
			}
		}
	}
	for key := range gotRows {
		if _, ok := wantRows[key]; !ok {
			return fmt.Sprintf("%s/%s -> %s is a row the expected table does not hold", key.from, key.trigger, key.to)
		}
	}
	for _, from := range phase.All {
		for _, trigger := range allWorkflowTriggers {
			for _, target := range append([]phase.Phase{""}, phase.All...) {
				for _, snapshot := range guardSnapshots {
					gotRow, gotOK := firstRow(got, from, trigger, target, snapshot)
					wantRow, wantOK := firstRow(want, from, trigger, target, snapshot)
					if gotOK != wantOK || gotRow.To != wantRow.To || !slices.Equal(gotRow.Effects, wantRow.Effects) {
						return fmt.Sprintf("the lookup %s/%s -> %q at %+v finds %s (%t), want %s (%t)", from, trigger, target, snapshot, gotRow.To, gotOK, wantRow.To, wantOK)
					}
				}
			}
		}
	}
	return ""
}

// The default sequence is today's line, and its table is the one table.go wrote out by hand
// before the table was generated (dispatch://INFRA-3570): the same rows, the same targets, the
// same effects, guards that answer alike, and the same answer to every lookup the engine makes.
// It is the control that generating the table changed nothing for an issue that names no other
// sequence, and it reads the package's Table, which was the hand-written table before and is the
// default sequence's generated one now, so the same test passes on both.
func TestTheDefaultSequenceGeneratesTodaysTable(t *testing.T) {
	if why := sameTables(Table, todaysTable()); why != "" {
		t.Fatal(why)
	}
}

// sameTables can fail: a table with one row's target, one row's effects or one row's guard
// changed, a row left out or a row added each reads as different from today's. Without this the
// test above could pass a generated table that differed in exactly the way it exists to catch.
func TestSameTablesTellsAChangedTable(t *testing.T) {
	for _, testCase := range []struct {
		name   string
		change func([]Row) []Row
	}{
		{name: "a target", change: func(rows []Row) []Row { rows[3].To = phase.Accepting; return rows }},
		{name: "an effect", change: func(rows []Row) []Row { rows[5].Effects = []EffectKind{EffectStart, EffectNotice}; return rows }},
		{name: "a guard", change: func(rows []Row) []Row { rows[2].Guard = allow; return rows }},
		{name: "a row left out", change: func(rows []Row) []Row { return slices.Delete(rows, 0, 1) }},
		{name: "a row added", change: func(rows []Row) []Row {
			return append(rows, Row{From: phase.Reviewing, Trigger: TriggerReviewApproved, Guard: allow, To: phase.Merging, Effects: []EffectKind{EffectStart, EffectNotice}})
		}},
		// The same rows in the reverse order: every key, effect and guard is today's, and only a
		// lookup that takes the first of several rows — a backward move naming no target — reads
		// another row first.
		{name: "a lookup that finds another row first", change: func(rows []Row) []Row { slices.Reverse(rows); return rows }},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			if why := sameTables(testCase.change(todaysTable()), todaysTable()); why == "" {
				t.Fatalf("a table with %s changed reads as today's", testCase.name)
			}
		})
	}
}

// light walks implementing, reviewing and merging alone: the gate's opening starts the
// implementer, the implementer's pull request goes to the reviewer, and the reviewer's approval
// to the merger, with no acceptance and no retro between; a rejection and a conflicting head send
// the work back to implementing, and the merge closes the issue as it does today. No row leaves a
// phase light does not walk — planning, testing, accepting, retro — so a trigger of one moves
// nothing (Engine.row answers false).
func TestTheLightSequenceGeneratesItsRows(t *testing.T) {
	light, ok := phase.SequenceFor("light")
	if !ok {
		t.Fatal("the library holds no light sequence")
	}
	start, merged := []EffectKind{EffectStart, EffectNotice}, []EffectKind{EffectSuspend, EffectNotice, EffectLinger}
	prGuard := func(s Snapshot) bool { return s.HasOpenOrMergedPR }
	want := []Row{
		{From: phase.Admitted, Trigger: TriggerGateOpened, Guard: allow, To: phase.Implementing, Effects: start},
		{From: phase.Implementing, Trigger: TriggerImplementationReady, Guard: prGuard, To: phase.Reviewing, Effects: start},
		{From: phase.Reviewing, Trigger: TriggerReviewApproved, Guard: allow, To: phase.Merging, Effects: start},
		{From: phase.Reviewing, Trigger: TriggerReviewRejected, Guard: allow, To: phase.Implementing, Effects: start},
		{From: phase.Reviewing, Trigger: TriggerConflict, Guard: allow, To: phase.Implementing, Effects: start},
		{From: phase.Merging, Trigger: TriggerConflict, Guard: allow, To: phase.Implementing, Effects: start},
		{From: phase.Merging, Trigger: TriggerPullRequestMerged, Guard: allow, To: phase.Done, Effects: merged},
		{From: phase.Reviewing, Trigger: TriggerBackward, Guard: allow, To: phase.Implementing, Effects: start},
		{From: phase.Merging, Trigger: TriggerBackward, Guard: allow, To: phase.Implementing, Effects: start},
		{From: phase.Merging, Trigger: TriggerBackward, Guard: allow, To: phase.Reviewing, Effects: start},
	}
	rows := TableOf(light)
	if why := sameTables(rows, want); why != "" {
		t.Fatal(why)
	}
	for _, absent := range []phase.Phase{phase.Planning, phase.Testing, phase.Accepting, phase.Retro} {
		for _, row := range rows {
			if row.From == absent || row.To == absent {
				t.Errorf("light's table holds %s/%s -> %s, a row of %s, which light does not walk", row.From, row.Trigger, row.To, absent)
			}
		}
	}
}

// no-planner is today's line without its planner: the gate's opening starts the implementer, and
// every other row is today's but the ones that leave planning or move back into it.
func TestTheNoPlannerSequenceGeneratesTodaysRowsWithoutPlanning(t *testing.T) {
	noPlanner, ok := phase.SequenceFor("no-planner")
	if !ok {
		t.Fatal("the library holds no no-planner sequence")
	}
	var want []Row
	for _, row := range todaysTable() {
		if row.From == phase.Planning || row.To == phase.Planning && row.From != phase.Admitted {
			continue
		}
		if row.From == phase.Admitted {
			row.To = phase.Implementing
		}
		want = append(want, row)
	}
	if why := sameTables(TableOf(noPlanner), want); why != "" {
		t.Fatal(why)
	}
	if row, ok := firstRow(TableOf(noPlanner), phase.Admitted, TriggerGateOpened, "", Snapshot{}); !ok || row.To != phase.Implementing {
		t.Fatalf("no-planner's gate opening = %s, %t, want implementing", row.To, ok)
	}
}

// Every sequence of the library has a row for each trigger its own phases declare and none
// elsewhere: a sequence's table is held to that sequence's phases, not to Order's.
func TestEveryPhaseAndTriggerHasARowOrNamedIgnore(t *testing.T) {
	for _, sequence := range phase.Sequences() {
		t.Run(sequence.Name, func(t *testing.T) {
			if err := validateTable(sequence, TableOf(sequence)); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// Deleting any one row of any sequence's table is caught, so the coverage above is no check that
// passes whatever the table holds.
func TestDeletingAnyTransitionRowBreaksCoverage(t *testing.T) {
	for _, sequence := range phase.Sequences() {
		t.Run(sequence.Name, func(t *testing.T) {
			table := TableOf(sequence)
			for index := range table {
				rows := append([]Row{}, table[:index]...)
				rows = append(rows, table[index+1:]...)
				if err := validateTable(sequence, rows); err == nil {
					t.Fatalf("deleting %s/%s -> %s left %s's table covered", table[index].From, table[index].Trigger, table[index].To, sequence.Name)
				}
			}
		})
	}
}

// The daemon counts a round for every row that moves an issue back (Engine.transition reads
// phase.MovesBack), and the config loader refuses a hook written for a pair outside
// phase.BackwardPairs. So every move back any sequence's table makes is a pair of that list: a
// row moving back on a pair outside it would be a move the daemon counts and no hook can name.
//
// The default sequence's moves back are that list exactly, as the one table's were before the
// table was generated: a pair of the list the default does not move on would be a hook that never
// runs on any issue. A shorter sequence moves on fewer pairs, and that is right: a hook of the
// deployment written for a pair such a sequence never walks — merging back to retro, on a light
// issue — simply never fires on that issue, as a hook for a move nobody asks for never fires,
// while internal/record/outbox.go and the config's hook loader still refuse a pair that is no
// move back at all.
func TestBackwardRowsMatchPhaseBackwardPairs(t *testing.T) {
	fromPhase := map[string]bool{}
	for _, pair := range phase.BackwardPairs() {
		fromPhase[pair] = true
	}
	for _, sequence := range phase.Sequences() {
		t.Run(sequence.Name, func(t *testing.T) {
			fromTable := map[string]bool{}
			fromTrigger := map[string]bool{}
			for _, row := range TableOf(sequence) {
				if !phase.MovesBack(row.From, row.To) {
					continue
				}
				fromTable[phase.Pair(row.From, row.To)] = true
				if row.Trigger == TriggerBackward {
					fromTrigger[phase.Pair(row.From, row.To)] = true
				}
			}
			for pair := range fromTable {
				if !fromPhase[pair] {
					t.Errorf("%s's table moves back on %s, which phase.BackwardPairs does not hold", sequence.Name, pair)
				}
			}
			// The backward trigger carries every pair the sequence's table moves back on: a worker
			// may ask for any move back its line allows, and the daemon's own move for a
			// conflicting head is a trigger of its own (TriggerConflict) over pairs that trigger
			// carries too, rather than a pair the backward trigger is missing.
			if !maps.Equal(fromTrigger, fromTable) {
				t.Errorf("%s's %s rows = %v, want every move back of its table %v", sequence.Name, TriggerBackward, sortedPairs(fromTrigger), sortedPairs(fromTable))
			}
			if sequence.Name == phase.DefaultSequenceName && !maps.Equal(fromTable, fromPhase) {
				t.Errorf("the default's backward moves = %v, phase.BackwardPairs = %v", sortedPairs(fromTable), sortedPairs(fromPhase))
			}
		})
	}
}

func sortedPairs(pairs map[string]bool) []string {
	return slices.Sorted(maps.Keys(pairs))
}

// validateTable is the coverage one sequence's table is held to: a complete row for every trigger
// a phase of the sequence declares (workflowTransitionRequired), a backward row to every earlier
// phase of the sequence, and no row anywhere else.
func validateTable(sequence phase.Sequence, rows []Row) error {
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
			}
			if workflowTransitionRequired(sequence, current, trigger) {
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
		for _, to := range requiredBackwardTargets(sequence, from) {
			if !hasBackwardRow(rows, from, to) {
				return fmt.Errorf("%s/backward -> %s is missing a transition row", from, to)
			}
		}
	}
	for _, row := range rows {
		if !workflowTransitionRequired(sequence, row.From, row.Trigger) {
			return fmt.Errorf("%s/%s is not a declared transition", row.From, row.Trigger)
		}
		if row.Trigger == TriggerBackward && !containsPhase(requiredBackwardTargets(sequence, row.From), row.To) {
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

// workflowTransitionRequired says whether a phase of sequence declares trigger: the gate's
// opening at admitted, each phase's own forward trigger, the tester's and the reviewer's failure,
// and a conflicting head in every phase after implementing — each only for a phase the sequence
// walks.
func workflowTransitionRequired(sequence phase.Sequence, from phase.Phase, trigger TriggerKind) bool {
	walked := sequence.Holds(from)
	switch trigger {
	case TriggerGateOpened:
		return from == phase.Admitted
	case TriggerPlannerCompleted:
		return walked && from == phase.Planning
	case TriggerImplementationReady:
		return walked && from == phase.Implementing
	case TriggerTesterPassed, TriggerTesterFailed:
		return walked && from == phase.Testing
	case TriggerReviewApproved, TriggerReviewRejected:
		return walked && from == phase.Reviewing
	case TriggerConflict:
		return walked && (from == phase.Testing || from == phase.Reviewing || from == phase.Accepting || from == phase.Retro || from == phase.Merging)
	case TriggerRetroCompleted:
		return walked && from == phase.Retro
	case TriggerPullRequestMerged:
		return walked && from == phase.Merging
	case TriggerSignOff:
		return walked && from == phase.Accepting
	case TriggerBackward:
		return len(requiredBackwardTargets(sequence, from)) != 0
	default:
		return false
	}
}

// requiredBackwardTargets is every phase of sequence before from, which a backward row of from
// serves; none for a phase the sequence does not walk.
func requiredBackwardTargets(sequence phase.Sequence, from phase.Phase) []phase.Phase {
	ordered := []phase.Phase{phase.Planning, phase.Implementing, phase.Testing, phase.Reviewing, phase.Accepting, phase.Retro, phase.Merging}
	if !sequence.Holds(from) {
		return nil
	}
	var targets []phase.Phase
	for _, candidate := range ordered[:slices.Index(ordered, from)] {
		if sequence.Holds(candidate) {
			targets = append(targets, candidate)
		}
	}
	return targets
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
