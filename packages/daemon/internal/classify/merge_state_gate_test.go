package classify

import (
	"testing"

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
// GitHub's answer whatever the fact, is not the gate's: workflow's askMergeStateFor adds it, so a
// review in reviewing asks nothing here.
func TestTheMergeStateReadGateAsksForEachReasonAlone(t *testing.T) {
	step := func(n int) record.TaskPlace { return record.TaskPlace{Kind: record.PlaceStep, Position: n} }
	retro, merge, none := record.TaskPlace{Kind: record.PlaceRetro}, record.TaskPlace{Kind: record.PlaceMerge}, record.TaskPlace{}
	// The places of today's line, phase.Order.
	planning, implementing, tested, reviewing, accepting := step(1), step(2), step(3), step(4), step(5)
	for _, tc := range []struct {
		name string
		implementation record.TaskPlace
		place record.TaskPlace
		fact MergeStateFact
		want bool
	}{
		{"a review in reviewing", implementing, reviewing, MergeStateFactOther, false},
		{"a settlement in merging", implementing, merge, MergeStateFactOther, false},

		{"a head move in planning", implementing, planning, MergeStateFactHeadMove, false},
		{"a head move in implementing", implementing, implementing, MergeStateFactHeadMove, true},
		{"a head move in testing", implementing, tested, MergeStateFactHeadMove, true},
		{"a head move in reviewing", implementing, reviewing, MergeStateFactHeadMove, true},
		{"a head move in accepting", implementing, accepting, MergeStateFactHeadMove, true},
		{"a head move in retro", implementing, retro, MergeStateFactHeadMove, true},
		{"a head move in merging", implementing, merge, MergeStateFactHeadMove, true},
		{"a head move of a task at no place", implementing, none, MergeStateFactHeadMove, false},
		{"a head move in a planning after the implementer", step(1), step(2), MergeStateFactHeadMove, true},
		{"a head move at a step after the implementer", step(2), step(3), MergeStateFactHeadMove, true},
		{"a head move at a step before the implementer", step(2), step(1), MergeStateFactHeadMove, false},

		{"a base push in planning", implementing, planning, MergeStateFactBasePush, false},
		{"a base push in implementing", implementing, implementing, MergeStateFactBasePush, false},
		{"a base push in testing", implementing, tested, MergeStateFactBasePush, true},
		{"a base push in reviewing", implementing, reviewing, MergeStateFactBasePush, true},
		{"a base push in accepting", implementing, accepting, MergeStateFactBasePush, true},
		{"a base push in retro", implementing, retro, MergeStateFactBasePush, true},
		{"a base push in merging", implementing, merge, MergeStateFactBasePush, true},
		{"a base push of a task at no place", implementing, none, MergeStateFactBasePush, false},
		{"a base push in a planning after the implementer", step(1), step(2), MergeStateFactBasePush, true},
		{"a base push in implementing before a planner", step(1), step(1), MergeStateFactBasePush, false},
		{"a base push at a step after the implementer", step(2), step(3), MergeStateFactBasePush, true},
		{"a base push at a step before the implementer", step(2), step(1), MergeStateFactBasePush, false},
		{"a base push at the implementer's place between two steps", step(2), step(2), MergeStateFactBasePush, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := MergeStateReadOwed(tc.implementation, tc.place, tc.fact); got != tc.want {
				t.Fatalf("MergeStateReadOwed = %v, want %v", got, tc.want)
			}
		})
	}
}
