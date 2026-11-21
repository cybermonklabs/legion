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
// (dispatch://INFRA-4374 R0, R4): a fact that moves the head of an issue in implementing through
// merging, and a push to the base of an issue in testing through merging. A head move before
// implementing, a base push in implementing or before it, and a fact that moves neither the head
// nor the base ask nothing. A held issue is read for as the phase it is held from. The third
// reason a read is owed, a review round that waits on GitHub's answer whatever the fact, is not
// the gate's: workflow's askMergeStateFor adds it, so a review in reviewing asks nothing here. The
// daemon's start read reads GitHub itself, and the gate owes it no read in any phase.
func TestTheMergeStateReadGateAsksForEachReasonAlone(t *testing.T) {
	for _, tc := range []struct {
		name  string
		issue record.Issue
		fact  MergeStateFact
		want  bool
	}{
		{"a review in reviewing", in(phase.Reviewing), MergeStateFactOther, false},
		{"a settlement in merging", in(phase.Merging), MergeStateFactOther, false},

		{"a head move in planning", in(phase.Planning), MergeStateFactHeadMove, false},
		{"a head move in implementing", in(phase.Implementing), MergeStateFactHeadMove, true},
		{"a head move in testing", in(phase.Testing), MergeStateFactHeadMove, true},
		{"a head move in reviewing", in(phase.Reviewing), MergeStateFactHeadMove, true},
		{"a head move in accepting", in(phase.Accepting), MergeStateFactHeadMove, true},
		{"a head move in retro", in(phase.Retro), MergeStateFactHeadMove, true},
		{"a head move in merging", in(phase.Merging), MergeStateFactHeadMove, true},
		{"a head move of an issue held from implementing", heldFrom(phase.Implementing), MergeStateFactHeadMove, true},
		{"a head move of an issue that left the workflow", in(phase.Done), MergeStateFactHeadMove, false},

		{"a base push in planning", in(phase.Planning), MergeStateFactBasePush, false},
		{"a base push in implementing", in(phase.Implementing), MergeStateFactBasePush, false},
		{"a base push of an issue held from implementing", heldFrom(phase.Implementing), MergeStateFactBasePush, false},
		{"a base push in testing", in(phase.Testing), MergeStateFactBasePush, true},
		{"a base push in reviewing", in(phase.Reviewing), MergeStateFactBasePush, true},
		{"a base push in accepting", in(phase.Accepting), MergeStateFactBasePush, true},
		{"a base push in retro", in(phase.Retro), MergeStateFactBasePush, true},
		{"a base push in merging", in(phase.Merging), MergeStateFactBasePush, true},
		{"a base push of an issue held from accepting", heldFrom(phase.Accepting), MergeStateFactBasePush, true},

		{"the start read in merging", in(phase.Merging), MergeStateFactStartRead, false},
		{"the start read in reviewing", in(phase.Reviewing), MergeStateFactStartRead, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := MergeStateReadOwed(tc.issue, tc.fact); got != tc.want {
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
