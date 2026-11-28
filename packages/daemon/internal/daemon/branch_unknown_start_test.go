package daemon

import (
	"testing"

	"github.com/sjawhar/legion/daemon/internal/record"
)

// A next change of an issue whose merge snapshot names only its pull request knows neither the
// base that pull request merged into nor the head it merged, and its branch row leaves both
// unknown (record.IssueBranchOf). The row starts at the recorded default branch and moves no
// branch GitHub has: a missing branch is made at the default branch's head, and a branch GitHub
// has is left where it stands and the owning architect told, since no merged head says its tip is
// the work that merged.
func TestABranchRowThatKnowsNeitherItsBaseNorItsMergedHeadStartsAtTheDefaultBranchAndMovesNoBranch(t *testing.T) {
	t.Parallel()
	t.Run("a branch GitHub does not have", func(t *testing.T) {
		t.Parallel()
		r := newNextBranchRig(t, map[string]string{"main": nextBaseHead}, nil)
		r.run(&record.BranchFrom{})
		creates, moves, tip := r.github.writes()
		if len(creates) != 1 || creates[0]["ref"] != "refs/heads/"+nextBranch || creates[0]["sha"] != nextBaseHead || len(moves) != 0 || tip != nextBaseHead {
			t.Fatalf("creates %v and moves %v, the branch at %s; want one create at the head %s of the default branch", creates, moves, tip, nextBaseHead)
		}
	})
	t.Run("a branch at the head the last pull request merged", func(t *testing.T) {
		t.Parallel()
		r := newNextBranchRig(t, map[string]string{"main": nextBaseHead, nextBranch: nextMergedHead},
			map[string]string{nextBaseHead + "..." + nextMergedHead: "diverged"})
		r.run(&record.BranchFrom{})
		creates, moves, tip := r.github.writes()
		if len(creates) != 0 || len(moves) != 0 || tip != nextMergedHead {
			t.Fatalf("creates %v and moves %v, the branch at %s; want it left at %s", creates, moves, tip, nextMergedHead)
		}
		if told := r.told(); len(told) != 1 || told[0][0] != "next-change-workspace" {
			t.Fatalf("told %v, want one next-change-workspace notice", told)
		}
	})
}
