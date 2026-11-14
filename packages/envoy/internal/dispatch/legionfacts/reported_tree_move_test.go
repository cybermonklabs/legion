package legionfacts

import (
	"slices"
	"testing"
)

// TestATreeObservationMovesAChildAndRecomputesBothRoots: a pass over a `tree` observation that
// moves a child from one root to another computes both roots' rows again, and leaves each one
// holding what a computation of the whole project answers for it.
//
// Dispatch's parent link and the daemon's `tree` move an issue between trees in the same way:
// the rule that places an issue in a tree reads the `tree` first and the parent link only where
// no daemon reported one (legion.TreeChain). A move therefore states two trees. The tree the
// issue joined is the tree the links now give, and the tree it left is the tree the moved
// issue's stored root_key still records, which is the only record of where the issue was
// (leftTrees).
//
// The root an issue leaves goes on holding the hours it waited while that issue worked
// (legion/metrics/segments.go), so its stored row is wrong from the move on. The sweep
// recomputes the open issues alone, and both roots here are closed, so a pass that stated the
// new tree alone would leave the old root's row wrong until a whole-project pass computed it
// again.
//
// The history is the rig's, with one `tree` observation written over it. The first pass computes
// the project whole, so every stored row is right before the observation, and the clock stands
// still over both passes, so nothing moves of itself.
func TestATreeObservationMovesAChildAndRecomputesBothRoots(t *testing.T) {
	run := newConsumer(t, 8)

	// The first tree's open child, reported under the root of the seventh. The two share no
	// parent link, so the observation is the only thing that moves the child between the trees.
	// The pair is chosen for its hours as well: each tree opens an hour before the tree before
	// it, and a root's own children stop working before the root closes, so this child is still
	// working through hours the seventh root's own children leave it. Reported under a root
	// whose own children already cover those hours, the child would move no row and this check
	// could not tell a recomputed row from a stale one.
	child, joined := run.fixture.Open[0], run.fixture.Roots[6]
	_, _, parents := issueRows(t, run.history)
	left := parents[child]
	if left != run.fixture.Roots[0] || joined == left {
		t.Fatalf("%s has the parent %q and is reported under %s, want a child of %s reported under a root outside its parent chain",
			child, left, joined, run.fixture.Roots[0])
	}

	// The seventh tree's own open child, closed before the first pass. Each pass recomputes
	// every open issue's stored row and climbs from it (openStoredRows), so a tree that holds
	// an open issue has its root computed again on every pass whatever this observation does,
	// and the checks below would read a fresh row either way. The first tree's one open issue
	// is the child itself, which the observation takes out of that tree, so no open issue
	// climbs to either root once the observation stands. The child closes at the moment of its
	// last observation, which is where it stopped working, so its root waits on it for the
	// hours it waited on it before.
	run.exec(t, "close the seventh tree's open child", `
		update issues set status = 'done',
			closed_at = (select max(observed_at) from legion_observations where issue_key = issues.key)
		where key = $1
	`, run.fixture.Open[6])

	run.pass(t)
	before := run.rows(t)
	for _, root := range []string{left, joined} {
		if before[root].Issue == "" {
			t.Fatalf("%s has no stored row after the first pass, so this check has nothing to compare", root)
		}
		if before[root].ClosedAt == nil {
			t.Fatalf("%s is open, so the sweep recomputes its row whatever the observation states and this check proves nothing", root)
		}
	}
	if before[child].RootKey != left {
		t.Fatalf("%s stands under %q before the observation, want %s", child, before[child].RootKey, left)
	}

	// The observation is the only change this pass reads: no event is written on the child, so
	// the move states its two trees or nothing states them.
	seedTree(t, run.history, child, joined)

	// What the move states, read from the same database at the same moment the pass about to
	// run reads it: the whole set (rowsToCompute) and the rows of the tree the child leaves,
	// which are the rows the two reads of leftTrees add to it.
	moves := map[string]bool{child: true}
	stated := statedSet(t, run, []string{child}, moves)
	leaving, err := leftTrees(t.Context(), run.database.Pool, run.fixture.Project, []string{child}, moves)
	if err != nil {
		t.Fatalf("read the rows of the tree %s leaves: %v", child, err)
	}

	changed := run.pass(t)
	stored := run.rows(t)
	strayed := outside(changed, stated)

	whole, _, err := ComputeProject(t.Context(), run.database.Pool, run.fixture.Project, run.at)
	if err != nil {
		t.Fatalf("compute the project whole: %v", err)
	}
	want := map[string]Row{}
	for _, row := range whole {
		want[row.Issue] = row
	}
	// The observation has to have moved the child between the two roots and moved the hours of
	// each, or the history states nothing and the checks below ask the parent chain over again.
	if stored[child].RootKey != joined {
		t.Fatalf("%s reports the tree %s and its stored root is %q, so this test never moved it between trees",
			child, joined, stored[child].RootKey)
	}
	for _, root := range []string{left, joined} {
		if want[root].Issue == "" {
			t.Fatalf("the project computed whole answered no row for %s, so this check has nothing to compare", root)
		}
		if sameRow(before[root], want[root]) {
			t.Fatalf("%s holds one row before the observation and after it (%s), so this check cannot tell a recomputed row from a stale one",
				root, show(want[root]))
		}
	}

	for _, root := range []struct{ key, moved string }{{left, "left"}, {joined, "joined"}} {
		if difference := rowDifference(stored[root.key], want[root.key]); difference != "" {
			t.Errorf("%s is the root %s %s, and its stored row is not the row the computation answers for it: %s",
				root.key, child, root.moved, difference)
		}
		if !slices.Contains(changed, root.key) {
			t.Errorf("%s is the root %s %s and the pass left its row alone; it changed %v",
				root.key, child, root.moved, changed)
		}
	}
	if !slices.Contains(stated, left) {
		t.Errorf("the set the pass stated is %v, which does not hold the root %s left", stated, left)
	}
	if len(strayed) > 0 {
		t.Errorf("the pass changed %d rows outside the set it stated: %v", len(strayed), strayed)
	}
	t.Logf("%s moved from %s to %s on one `tree` observation, both roots closed: the move stated %d keys (%v), %d of them the rows of the tree it left (%v); the pass changed %d rows (%v), %d of them outside the stated set; the root it left holds %v against the %v a computation of the whole project answers, and the root it joined %v against %v",
		child, left, joined, len(stated), stated, len(leaving), leaving, len(changed), changed, len(strayed),
		stored[left].HoursByPhase, want[left].HoursByPhase,
		stored[joined].HoursByPhase, want[joined].HoursByPhase)
}
