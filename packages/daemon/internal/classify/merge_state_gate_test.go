package classify

import (
	"testing"

	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// in is LEGION-208 standing in at.
func in(at phase.Phase) record.Issue { return record.Issue{Key: "LEGION-208", Phase: at} }

// heldFrom is LEGION-208 held from at.
func heldFrom(at phase.Phase) record.Issue {
	return record.Issue{Key: "LEGION-208", Phase: phase.Held, Hold: &record.Hold{From: at}}
}

// The gate of the read of GitHub's merge state asks for each of two reasons, and for nothing else
// (dispatch://INFRA-4374 R0, R4): a fact that moves the head of an issue in implementing or a phase
// after it on its task's line, and a push to the base of an issue in a phase after implementing on
// that line. On the default steps (phase.Order) that is implementing through merging, and testing
// through merging. A head move before implementing, a base push in implementing or before it, and a
// fact that moves neither the head nor the base ask nothing. A planner step after the implementer
// (`implementer planner reviewer`) works on a branch whose pull request is open, so its planning
// is read for as testing is; a planner before it is read for nothing. A held issue is read for as
// the phase it is held from. The third reason a read is owed, a review round that waits on
// GitHub's answer whatever the fact, is not the gate's: workflow's mergeStateOwed adds it, so a
// review in reviewing asks nothing here. The daemon's start read reads GitHub itself, and the gate
// owes it no read in any phase.
func TestTheMergeStateReadGateAsksForEachReasonAlone(t *testing.T) {
	plannedAfter := []phase.Phase{phase.Implementing, phase.Planning, phase.Reviewing, phase.Retro, phase.Merging}
	for _, tc := range []struct {
		name  string
		issue record.Issue
		line  []phase.Phase
		fact  MergeStateFact
		want  bool
	}{
		{"a review in reviewing", in(phase.Reviewing), phase.Order, MergeStateFactOther, false},
		{"a settlement in merging", in(phase.Merging), phase.Order, MergeStateFactOther, false},

		{"a head move in planning", in(phase.Planning), phase.Order, MergeStateFactHeadMove, false},
		{"a head move in implementing", in(phase.Implementing), phase.Order, MergeStateFactHeadMove, true},
		{"a head move in testing", in(phase.Testing), phase.Order, MergeStateFactHeadMove, true},
		{"a head move in reviewing", in(phase.Reviewing), phase.Order, MergeStateFactHeadMove, true},
		{"a head move in accepting", in(phase.Accepting), phase.Order, MergeStateFactHeadMove, true},
		{"a head move in retro", in(phase.Retro), phase.Order, MergeStateFactHeadMove, true},
		{"a head move in merging", in(phase.Merging), phase.Order, MergeStateFactHeadMove, true},
		{"a head move of an issue held from implementing", heldFrom(phase.Implementing), phase.Order, MergeStateFactHeadMove, true},
		{"a head move of an issue that left the workflow", in(phase.Done), phase.Order, MergeStateFactHeadMove, false},
		{"a head move in admitted", in(phase.Admitted), phase.Order, MergeStateFactHeadMove, false},
		{"a head move in a planning after the implementer", in(phase.Planning), plannedAfter, MergeStateFactHeadMove, true},
		{"a head move of an issue held from a planning after the implementer", heldFrom(phase.Planning), plannedAfter, MergeStateFactHeadMove, true},
		{"a head move in testing on a line with no tester", in(phase.Testing), plannedAfter, MergeStateFactHeadMove, false},

		{"a base push in planning", in(phase.Planning), phase.Order, MergeStateFactBasePush, false},
		{"a base push in implementing", in(phase.Implementing), phase.Order, MergeStateFactBasePush, false},
		{"a base push of an issue held from implementing", heldFrom(phase.Implementing), phase.Order, MergeStateFactBasePush, false},
		{"a base push in testing", in(phase.Testing), phase.Order, MergeStateFactBasePush, true},
		{"a base push in reviewing", in(phase.Reviewing), phase.Order, MergeStateFactBasePush, true},
		{"a base push in accepting", in(phase.Accepting), phase.Order, MergeStateFactBasePush, true},
		{"a base push in retro", in(phase.Retro), phase.Order, MergeStateFactBasePush, true},
		{"a base push in merging", in(phase.Merging), phase.Order, MergeStateFactBasePush, true},
		{"a base push of an issue held from accepting", heldFrom(phase.Accepting), phase.Order, MergeStateFactBasePush, true},
		{"a base push in a planning after the implementer", in(phase.Planning), plannedAfter, MergeStateFactBasePush, true},
		{"a base push in implementing before a planner", in(phase.Implementing), plannedAfter, MergeStateFactBasePush, false},
		{"a base push in testing on a line with no tester", in(phase.Testing), plannedAfter, MergeStateFactBasePush, false},

		{"the start read in merging", in(phase.Merging), phase.Order, MergeStateFactStartRead, false},
		{"the start read in reviewing", in(phase.Reviewing), phase.Order, MergeStateFactStartRead, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := MergeStateReadOwed(tc.issue, tc.line, tc.fact); got != tc.want {
				t.Fatalf("MergeStateReadOwed = %v, want %v", got, tc.want)
			}
		})
	}
}

// The read a base push owes an issue that stands in merging is one read (MergeStateReadOnce), and
// so is the daemon's start read of such an issue, whose own read is that one: its pull request
// sits in GitHub's merge queue, where GitHub answers `unknown` for the head. Every other read is a
// ladder: a base push or a start read in testing through retro, and a head move in merging too. A
// hold from merging is read as merging.
func TestTheMergeStateReadIsOneReadInMergingForABasePushOrTheStartRead(t *testing.T) {
	for _, tc := range []struct {
		name  string
		issue record.Issue
		fact  MergeStateFact
		want  bool
	}{
		{"a base push in merging", in(phase.Merging), MergeStateFactBasePush, true},
		{"a base push of an issue held from merging", heldFrom(phase.Merging), MergeStateFactBasePush, true},
		{"a base push in testing", in(phase.Testing), MergeStateFactBasePush, false},
		{"a base push in reviewing", in(phase.Reviewing), MergeStateFactBasePush, false},
		{"a base push in accepting", in(phase.Accepting), MergeStateFactBasePush, false},
		{"a base push in retro", in(phase.Retro), MergeStateFactBasePush, false},
		{"the start read in merging", in(phase.Merging), MergeStateFactStartRead, true},
		{"the start read of an issue held from merging", heldFrom(phase.Merging), MergeStateFactStartRead, true},
		{"the start read in testing", in(phase.Testing), MergeStateFactStartRead, false},
		{"the start read in reviewing", in(phase.Reviewing), MergeStateFactStartRead, false},
		{"the start read in retro", in(phase.Retro), MergeStateFactStartRead, false},
		{"a head move in merging", in(phase.Merging), MergeStateFactHeadMove, false},
		{"a settlement in merging", in(phase.Merging), MergeStateFactOther, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := MergeStateReadOnce(tc.issue, tc.fact); got != tc.want {
				t.Fatalf("MergeStateReadOnce = %v, want %v", got, tc.want)
			}
		})
	}
}
