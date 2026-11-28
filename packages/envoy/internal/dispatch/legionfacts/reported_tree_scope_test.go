package legionfacts

import (
	"slices"
	"testing"
	"time"
)

// TestAChangeToAChildRecomputesTheRootItReportsAsItsTree: a pass over a change to a child whose
// daemon reported a `tree` outside the child's parent chain computes that root's row again, and
// leaves it holding what a computation of the whole project answers for it.
//
// The rule that places an issue in a tree reads the daemon's `tree` first and the parent link
// only where no daemon reported one (legion.TreeChain). A root therefore waits on a child no
// parent link ties to it, and a root's hours drop the time it waited while a child worked
// (legion/metrics/segments.go). A pass that climbed the parent links alone would state the
// child and its parent chain, never this root, and the root's row would stand wrong until a
// whole-project pass computed it again, since no pass computes a row its feeds did not name.
//
// The history is the rig's, with one `tree` observation written over it. The first pass
// computes the project whole, so every stored row is right before the change.
func TestAChangeToAChildRecomputesTheRootItReportsAsItsTree(t *testing.T) {
	run := newConsumer(t, 8)

	// The first tree's open child, reported under the root of the seventh. The two share no
	// parent link, so the observation is the only thing that puts them in one tree. The pair is
	// chosen for its hours as well: each tree opens an hour before the tree before it, and a
	// root's own children stop working before the root closes, so this child is still working
	// through hours the seventh root's own children leave it. Reported under a root whose own
	// children already cover those hours, the child would move no row and this check could not
	// tell a recomputed row from a stale one.
	child, tree := run.fixture.Open[0], run.fixture.Roots[6]
	_, _, parents := issueRows(t, run.history)
	if parents[child] != run.fixture.Roots[0] || tree == run.fixture.Roots[0] {
		t.Fatalf("%s has the parent %q and is reported under %s, want a child of %s reported under a root outside its parent chain",
			child, parents[child], tree, run.fixture.Roots[0])
	}

	// The seventh tree's own open child, closed before the first pass, so the root and every
	// issue below it stand closed and the check below reads a row only the change's stated set
	// can reach. It closes at the moment of its last observation, which is where it stopped
	// working, so the root waits on it for the hours it waited on it before.
	run.exec(t, "close the seventh tree's open child", `
		update issues set status = 'done',
			closed_at = (select max(observed_at) from legion_observations where issue_key = issues.key)
		where key = $1
	`, run.fixture.Open[6])

	run.pass(t)
	before := run.rows(t)
	if before[tree].Issue == "" {
		t.Fatalf("%s has no stored row after the first pass, so this check has nothing to compare", tree)
	}

	seedTree(t, run.history, child, tree)
	run.writeEvent(t, child, "implementer", "comment.created", `{}`, run.at.Add(-time.Hour))

	changed := run.pass(t)
	stored := run.rows(t)

	whole, _, err := ComputeProject(t.Context(), run.database.Pool, run.fixture.Project, run.at)
	if err != nil {
		t.Fatalf("compute the project whole: %v", err)
	}
	want := Row{}
	for _, row := range whole {
		if row.Issue == tree {
			want = row
		}
	}
	if want.Issue == "" {
		t.Fatalf("the project computed whole answered no row for %s, so this check has nothing to compare", tree)
	}
	// The observation has to have moved the child's root and the root's own hours, or the
	// history states nothing and the check below asks the parent chain over again.
	if stored[child].Tree != tree {
		t.Fatalf("%s reports the tree %s and its stored root is %q, so this test never made a rule read a reported tree",
			child, tree, stored[child].Tree)
	}
	if sameRow(before[tree], want) {
		t.Fatalf("%s holds one row before the observation and after it (%s), so this check cannot tell a recomputed row from a stale one",
			tree, show(want))
	}

	// This pass runs at the first pass's moment, and a rewrite at the moment the row was last
	// written stores updated_at a microsecond past it (upsert), so the row's moment is held to move
	// from the row before rather than to equal the computation's.
	fresh := want
	fresh.UpdatedAt = stored[tree].UpdatedAt
	if difference := rowDifference(stored[tree], fresh); difference != "" {
		t.Errorf("%s is the root %s reports as its tree, and its stored row is not the row the computation answers for it: %s",
			tree, child, difference)
	}
	if !stored[tree].UpdatedAt.After(before[tree].UpdatedAt) {
		t.Errorf("%s is the root %s reports as its tree, and its row was rewritten at %s, no later than the row before it, written at %s",
			tree, child, stored[tree].UpdatedAt.UTC(), before[tree].UpdatedAt.UTC())
	}
	if !slices.Contains(changed, tree) {
		t.Errorf("%s is the root %s reports as its tree and the pass left its row alone; it changed %v", tree, child, changed)
	}
	t.Logf("%s reports the tree %s, whose parent chain holds %s instead: the pass changed %d rows, and the root's hours by part went %v to %v, against the %v a computation of the whole project answers",
		child, tree, parents[child], len(changed), before[tree].PhaseHours,
		stored[tree].PhaseHours, want.PhaseHours)
}
