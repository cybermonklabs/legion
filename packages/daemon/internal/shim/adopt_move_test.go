package shim_test

import (
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/shimwire"
)

// A move to a commit the task's working copy already holds keeps the copy where it stands. The fix
// ends at Y, a commit of another working copy of the clone over the task's head, and the adoption
// before the next step moves the copy to Y. That step commits over Y in the copy, and the adoption
// before the step after it names Y again. The copy stands on that commit, whose parent is Y, so it
// stays there, with the commit's file, and the answer names Y, which the copy holds. The control,
// in the same run: the first adoption, to a Y the copy does not hold, moves it.
func TestAMoveToACommitTheCopyHoldsKeepsTheCommitsOverIt(t *testing.T) {
	r := newCopyRig(t)
	// The fix's end push, from another working copy of the clone, over the task's head.
	r.jjIn(r.clone, "new", r.head)
	end := r.commit(r.clone, "fix.txt", "fix: the failure")

	if got := r.adopt("way", shimwire.AdoptWorkingCopy{MoveTo: end}); !got.OK || got.MovedTo != end {
		t.Fatalf("the adoption before the way's step answered %#v, want ok with movedTo %s", got, end)
	}
	if parent := r.commitOf(r.workspace, "@-"); parent != end {
		t.Fatalf("after the way's adoption the task's working copy stands on %s, want the fix's end, %s: the control, a move to a commit the copy does not hold, moved nothing", parent, end)
	}
	tested := r.commit(r.workspace, "red_test.txt", "test: the red test")

	before := len(r.commands())
	got := r.adopt("next", shimwire.AdoptWorkingCopy{MoveTo: end})
	if !got.OK || got.MovedTo != end {
		t.Fatalf("the adoption before the next step answered %#v, want ok with movedTo %s", got, end)
	}
	if parent := r.commitOf(r.workspace, "@-"); parent != tested {
		t.Errorf("the task's working copy stands on %s, want %s, the step's commit over the fix's end", parent, tested)
	}
	if files := r.filesOf(r.workspace, "@-"); !slices.Contains(files, "red_test.txt") || !slices.Contains(files, "fix.txt") {
		t.Errorf("the commit under the task's working copy holds %v, want red_test.txt and fix.txt", files)
	}
	if ran := r.commands()[before:]; slices.Contains(verbs(ran), "new") {
		t.Errorf("the adoption ran jj\n%s\nwant no jj new: the copy holds the commit it names", strings.Join(ran, "\n"))
	}
}
