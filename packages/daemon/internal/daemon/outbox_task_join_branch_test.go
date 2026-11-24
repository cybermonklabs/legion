package daemon

import (
	"slices"
	"testing"
)

// joinBranchPlan is a plan whose task d needs tasks a and c, in that order, and whose task e needs
// d, so d is no task that merges and its pull request is based on its join branch.
var joinBranchPlan = []plannedTask{{id: "a"}, {id: "c"}, {id: "d", needs: []string{"a", "c"}}, {id: "e", needs: []string{"d"}}}

// joinBranchOfD is the join branch of task d of joinBranchPlan: its branch and
// claim.JoinBranchSuffix.
const joinBranchOfD = "legion/LEGION-208.d.join"

// A task that needs several tasks and does not merge joins their work on its join branch: the join
// branch starts at the first need's head, in the order of its needs, and each other need is merged
// into it, and the task's branch starts at the join branch's head, so the task's pull request,
// based on the join branch, shows the task's own work alone. A row that runs again moves no ref. A
// start again, where GitHub has the task's branch with work of its own on it and a need moved since,
// joins the need's new work on the join branch and merges the join branch into the task's branch.
//
// The fault it catches is d's pull request against a's branch, which shows c's work as d's (its
// control: record.StoredPlan.BaseOf answering the first need's branch, so no join branch is made).
func TestATaskWithSeveralNeedsStartsFromItsJoinBranch(t *testing.T) {
	t.Parallel()
	rig := newTaskBranchRig(t, joinBranchPlan...)
	a := rig.github.Commit(t, rig.branch(t, "a"), map[string]string{"a.txt": "a\n"}, "a")
	c := rig.github.Commit(t, rig.branch(t, "c"), map[string]string{"c.txt": "c\n"}, "c")

	rig.run(t, "d")

	join := rig.github.Head(t, joinBranchOfD)
	if join == "" {
		t.Fatalf("GitHub has no branch %s, want task d's join branch there; its refs:\n%s", joinBranchOfD, rig.refs(t))
	}
	if parents := rig.parents(t, join); !slices.Equal(parents, []string{a, c}) {
		t.Fatalf("the parents of d's join branch head %q = %v, want task a's head %s and task c's head %s", join, parents, a, c)
	}
	branch := rig.branch(t, "d")
	if branch != "legion/LEGION-208.d" {
		t.Fatalf("task d's branch = %s, want legion/LEGION-208.d", branch)
	}
	if head := rig.github.Head(t, branch); head != join {
		t.Fatalf("task d's branch stands at %q, want its join branch's head %s", head, join)
	}
	if conflicts := rig.task(t, "d").JoinConflicts; len(conflicts) != 0 {
		t.Fatalf("task d's join conflicts = %v, want none", conflicts)
	}

	before := rig.refs(t)
	rig.run(t, "d")
	if after := rig.refs(t); after != before {
		t.Fatalf("the refs after the row ran again:\n%s\nwant them as they were:\n%s", after, before)
	}

	own := rig.github.Commit(t, branch, map[string]string{"d.txt": "d\n"}, "d")
	moved := rig.github.Commit(t, rig.branch(t, "c"), map[string]string{"c.txt": "c again\n"}, "c again")
	rig.run(t, "d")

	joinAgain := rig.github.Head(t, joinBranchOfD)
	if parents := rig.parents(t, joinAgain); !slices.Equal(parents, []string{join, moved}) {
		t.Fatalf("the parents of d's join branch head %q after c moved = %v, want the join before %s and c's new head %s", joinAgain, parents, join, moved)
	}
	head := rig.github.Head(t, branch)
	if parents := rig.parents(t, head); !slices.Equal(parents, []string{own, joinAgain}) {
		t.Fatalf("the parents of task d's head %q = %v, want d's own work %s and its join branch's head %s", head, parents, own, joinAgain)
	}
}

// A need whose changes conflict with the needs joined before it on the join branch is no refusal:
// GitHub's 409 is recorded as a join conflict of that need, the join branch stands on the needs that
// joined, and the task's branch starts there.
func TestAJoinBranchThatConflictsStandsOnTheNeedsThatJoined(t *testing.T) {
	t.Parallel()
	rig := newTaskBranchRig(t, joinBranchPlan...)
	a := rig.github.Commit(t, rig.branch(t, "a"), map[string]string{"x.txt": "from a\n"}, "a")
	rig.github.Commit(t, rig.branch(t, "c"), map[string]string{"x.txt": "from c\n"}, "c")

	rig.run(t, "d")

	if head := rig.github.Head(t, joinBranchOfD); head != a {
		t.Fatalf("d's join branch stands at %q, want task a's head %s", head, a)
	}
	if head := rig.github.Head(t, rig.branch(t, "d")); head != a {
		t.Fatalf("task d's branch stands at %q, want task a's head %s", head, a)
	}
	if conflicts := rig.task(t, "d").JoinConflicts; !slices.Equal(conflicts, []string{"c"}) {
		t.Fatalf("task d's join conflicts = %v, want [c]", conflicts)
	}
}
