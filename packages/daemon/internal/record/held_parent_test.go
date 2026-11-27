package record

import (
	"slices"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/phase"
)

// A parent held before its retro for the issues beneath it (HoldForChildren) waits for them to
// run to done, so the daemon has accepted no head of it: Accepted is false, a child beneath it at
// admitted is not held under it (HeldUnderAccepted) and so gets the slot its first step needs, and
// OpenBeneath names that child, so the hold waits for it. Read as accepted, as Accepted read every
// hold from retro, the parent would hold that child at admitted until the parent closed, leave it
// out of the wait, and start its retro and merge without the child's work. The controls are the
// issues whose head the daemon has accepted: a parent in retro, and one held from retro by a failed
// claim or a stopped task, each of which holds the child at admitted and leaves it out of the wait.
func TestAParentHeldForItsChildrenIsNotAccepted(t *testing.T) {
	parentKey := "LEGION-208"
	child := Issue{Key: "LEGION-209", Tree: parentKey, Parent: &parentKey, Project: "LEGION", Phase: phase.Admitted, Generation: 1, Status: "todo"}
	for _, tc := range []struct {
		name     string
		phase    phase.Phase
		hold     *Hold
		accepted bool
	}{
		{"held before its retro for its children", phase.Held, &Hold{From: phase.Retro, Reason: HoldForChildren}, false},
		{"in its retro", phase.Retro, nil, true},
		{"held from its retro by a failed claim", phase.Held, &Hold{From: phase.Retro}, true},
		{"held from its retro for a stopped task", phase.Held, &Hold{From: phase.Retro, Reason: HoldTaskStopped}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			parent := Issue{Key: parentKey, Tree: parentKey, Project: "LEGION", Phase: tc.phase, Hold: tc.hold, Generation: 1, Status: "retro"}
			members := []Issue{parent, child}
			if got := Accepted(parent); got != tc.accepted {
				t.Fatalf("Accepted = %v, want %v", got, tc.accepted)
			}
			ancestor, want := AcceptedAncestor(child, byKey(members)), ""
			if tc.accepted {
				want = parentKey
			}
			if ancestor != want {
				t.Fatalf("the accepted ancestor of %s = %q, want %q", child.Key, ancestor, want)
			}
			if held := HeldUnderAccepted(child, byKey(members)); held != tc.accepted {
				t.Fatalf("HeldUnderAccepted(%s) = %v, want %v", child.Key, held, tc.accepted)
			}
			open, wantOpen := OpenBeneath(parent, members), []string{child.Key}
			if tc.accepted {
				wantOpen = []string{}
			}
			if !slices.Equal(open, wantOpen) {
				t.Fatalf("OpenBeneath = %v, want %v", open, wantOpen)
			}
		})
	}
}
