package classify

import (
	"testing"

	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// The read of GitHub's merge state is asked for by each of three reasons, and by nothing else
// (dispatch://INFRA-4374 R0, R4): a review round that waits on GitHub's answer, whatever the fact;
// a fact that moves the head of an issue in implementing through merging; and a push to the base
// of an issue in testing through merging. A head move before implementing, a base push in
// implementing or before it, and a fact that moves neither the head nor the base while no round
// waits ask nothing. A held issue is read for as the phase it is held from.
func TestTheMergeStateReadGateAsksForEachReasonAlone(t *testing.T) {
	in := func(at phase.Phase) record.Issue { return record.Issue{Key: "LEGION-208", Phase: at} }
	heldFrom := func(at phase.Phase) record.Issue {
		return record.Issue{Key: "LEGION-208", Phase: phase.Held, Hold: &record.Hold{From: at}}
	}
	for _, tc := range []struct {
		name       string
		issue      record.Issue
		roundWaits bool
		fact       MergeStateFact
		want       bool
	}{
		{"a review round that waits, on a review", in(phase.Reviewing), true, MergeStateFactOther, true},
		{"a review round that waits, on a base push", in(phase.Reviewing), true, MergeStateFactBasePush, true},
		{"a review in reviewing with no round waiting", in(phase.Reviewing), false, MergeStateFactOther, false},
		{"a settlement in merging", in(phase.Merging), false, MergeStateFactOther, false},

		{"a head move in planning", in(phase.Planning), false, MergeStateFactHeadMove, false},
		{"a head move in implementing", in(phase.Implementing), false, MergeStateFactHeadMove, true},
		{"a head move in testing", in(phase.Testing), false, MergeStateFactHeadMove, true},
		{"a head move in reviewing with no round waiting", in(phase.Reviewing), false, MergeStateFactHeadMove, true},
		{"a head move in accepting", in(phase.Accepting), false, MergeStateFactHeadMove, true},
		{"a head move in retro", in(phase.Retro), false, MergeStateFactHeadMove, true},
		{"a head move in merging", in(phase.Merging), false, MergeStateFactHeadMove, true},
		{"a head move of an issue held from implementing", heldFrom(phase.Implementing), false, MergeStateFactHeadMove, true},
		{"a head move of an issue that left the workflow", in(phase.Done), false, MergeStateFactHeadMove, false},

		{"a base push in planning", in(phase.Planning), false, MergeStateFactBasePush, false},
		{"a base push in implementing", in(phase.Implementing), false, MergeStateFactBasePush, false},
		{"a base push of an issue held from implementing", heldFrom(phase.Implementing), false, MergeStateFactBasePush, false},
		{"a base push in testing", in(phase.Testing), false, MergeStateFactBasePush, true},
		{"a base push in reviewing with no round waiting", in(phase.Reviewing), false, MergeStateFactBasePush, true},
		{"a base push in accepting", in(phase.Accepting), false, MergeStateFactBasePush, true},
		{"a base push in retro", in(phase.Retro), false, MergeStateFactBasePush, true},
		{"a base push in merging", in(phase.Merging), false, MergeStateFactBasePush, true},
		{"a base push of an issue held from accepting", heldFrom(phase.Accepting), false, MergeStateFactBasePush, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := MergeStateReadOwed(tc.issue, tc.roundWaits, tc.fact); got != tc.want {
				t.Fatalf("MergeStateReadOwed = %v, want %v", got, tc.want)
			}
		})
	}
}
