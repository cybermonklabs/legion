package classify

import (
	"testing"

	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// The gate of the read of GitHub's merge state asks for each of two reasons, and for nothing else
// (dispatch://INFRA-4374 R0, R4): a fact that moves the head of a task at the implementer's place
// or a place after it on its line, and a push to the base of a task at a place after the
// implementer's. On the default steps (phase.Order) that is implementing through merging, and
// testing through merging. A head move before the implementer, a base push at the implementer's
// place or before it, a task at no place (an issue in admitted, or out of the workflow), and a
// fact that moves neither the head nor the base ask nothing. A planner step after the implementer
// (`implementer planner reviewer`) works on a branch whose pull request is open, so its planning
// is read for as testing is; a planner before it is read for nothing. The gate reads the place,
// never the phase: `checker implementer linter` reads step at place 1 and at place 3, and only the
// linter's place 3 stands after the implementer. A held issue is read for at the place it is held
// from, which its caller hands over. The third reason a read is owed, a review round that waits on
// GitHub's answer whatever the fact, is not the gate's: workflow's mergeStateOwed adds it, so a
// review in reviewing asks nothing here.
func TestTheMergeStateReadGateAsksForEachReasonAlone(t *testing.T) {
	step := func(n int) record.TaskPlace { return record.TaskPlace{Kind: record.PlaceStep, Position: n} }
	retro, merge, none := record.TaskPlace{Kind: record.PlaceRetro}, record.TaskPlace{Kind: record.PlaceMerge}, record.TaskPlace{}
	// The places of today's line, phase.Order.
	planning, implementing, tested, reviewing, accepting := step(1), step(2), step(3), step(4), step(5)
	plannedAfter := []phase.Phase{phase.Implementing, phase.Planning, phase.Reviewing, phase.Retro, phase.Merging}
	checkedAround := []phase.Phase{phase.Step, phase.Implementing, phase.Step, phase.Retro, phase.Merging}
	for _, tc := range []struct {
		name  string
		line  []phase.Phase
		place record.TaskPlace
		fact  MergeStateFact
		want  bool
	}{
		{"a review in reviewing", phase.Order, reviewing, MergeStateFactOther, false},
		{"a settlement in merging", phase.Order, merge, MergeStateFactOther, false},

		{"a head move in planning", phase.Order, planning, MergeStateFactHeadMove, false},
		{"a head move in implementing", phase.Order, implementing, MergeStateFactHeadMove, true},
		{"a head move in testing", phase.Order, tested, MergeStateFactHeadMove, true},
		{"a head move in reviewing", phase.Order, reviewing, MergeStateFactHeadMove, true},
		{"a head move in accepting", phase.Order, accepting, MergeStateFactHeadMove, true},
		{"a head move in retro", phase.Order, retro, MergeStateFactHeadMove, true},
		{"a head move in merging", phase.Order, merge, MergeStateFactHeadMove, true},
		{"a head move of a task at no place", phase.Order, none, MergeStateFactHeadMove, false},
		{"a head move in a planning after the implementer", plannedAfter, step(2), MergeStateFactHeadMove, true},
		{"a head move at a step after the implementer", checkedAround, step(3), MergeStateFactHeadMove, true},
		{"a head move at a step before the implementer", checkedAround, step(1), MergeStateFactHeadMove, false},

		{"a base push in planning", phase.Order, planning, MergeStateFactBasePush, false},
		{"a base push in implementing", phase.Order, implementing, MergeStateFactBasePush, false},
		{"a base push in testing", phase.Order, tested, MergeStateFactBasePush, true},
		{"a base push in reviewing", phase.Order, reviewing, MergeStateFactBasePush, true},
		{"a base push in accepting", phase.Order, accepting, MergeStateFactBasePush, true},
		{"a base push in retro", phase.Order, retro, MergeStateFactBasePush, true},
		{"a base push in merging", phase.Order, merge, MergeStateFactBasePush, true},
		{"a base push of a task at no place", phase.Order, none, MergeStateFactBasePush, false},
		{"a base push in a planning after the implementer", plannedAfter, step(2), MergeStateFactBasePush, true},
		{"a base push in implementing before a planner", plannedAfter, step(1), MergeStateFactBasePush, false},
		{"a base push at a step after the implementer", checkedAround, step(3), MergeStateFactBasePush, true},
		{"a base push at a step before the implementer", checkedAround, step(1), MergeStateFactBasePush, false},
		{"a base push at the implementer's place between two steps", checkedAround, step(2), MergeStateFactBasePush, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := MergeStateReadOwed(implementerPlace(tc.line), tc.place, false, tc.fact); got != tc.want {
				t.Fatalf("MergeStateReadOwed = %v, want %v", got, tc.want)
			}
		})
	}
	// Once the task's run has reached its implementer, a move back to a step before it keeps the
	// merge forward standing there, so a head push and a base push at the checker's place 1 each
	// ask the read; the cases above, at that same place before the run reached the implementer,
	// ask none.
	for _, fact := range []MergeStateFact{MergeStateFactHeadMove, MergeStateFactBasePush} {
		if !MergeStateReadOwed(implementerPlace(checkedAround), step(1), true, fact) {
			t.Errorf("MergeStateReadOwed(the checker's place 1, after the run reached the implementer, %v) = false, want true", fact)
		}
	}
}

// implementerPlace is the place of the implementer's step on line, the first step of the type
// implementation of a task whose roles are Legion's (the workflow's Walk.implementationStart), and
// no place where line holds none.
func implementerPlace(line []phase.Phase) record.TaskPlace {
	for at, p := range line {
		if p == phase.Implementing {
			return record.TaskPlace{Kind: record.PlaceStep, Position: at + 1}
		}
	}
	return record.TaskPlace{}
}
