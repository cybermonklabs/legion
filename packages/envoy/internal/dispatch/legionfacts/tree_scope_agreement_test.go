package legionfacts

import (
	"fmt"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/legion"
)

// TestEveryIssueComputedAloneHoldsTheFactsTheProjectComputedWholeWhenADaemonNamedATree is
// TestEveryIssueComputedAloneHoldsTheFactsTheProjectComputedWhole over a history that holds the
// one fact the rig never writes: a `tree` observation, the field the daemon names an issue's top
// issue with (dispatch://INFRA-980, legion.FieldTree).
//
// The tree rule reads that field first and the parent chain only where no daemon named a tree
// (legion.TreeChain). A scope that climbs the parent chain alone therefore leaves out the tree a
// `tree` observation names, and every rule that reads a tree — the root, the children of a root,
// the hours a root waited while a child worked — then answers from a part of it. The observation
// here names a root outside the child's parent chain, which is what makes the two climbs reach
// different issues: the whole-project pass holds both trees, and a pass over the child alone
// holds only what its own climb reached.
//
// The check is the same invariant: the project computed whole, each issue computed on its own,
// and the two rows of that issue compared field by field.
func TestEveryIssueComputedAloneHoldsTheFactsTheProjectComputedWholeWhenADaemonNamedATree(t *testing.T) {
	seeded := seed(t, 6)
	project := seeded.fixture.Project
	pool := seeded.database.Pool

	// The second tree's reviewed child, given the first tree's root as its tree. The two issues
	// share no parent link, so the daemon's tree is the only thing that puts them in one tree.
	child, tree := seeded.fixture.Keys[8], seeded.fixture.Roots[0]
	_, _, parents := issueRows(t, seeded)
	if parents[child] != seeded.fixture.Roots[1] || tree == seeded.fixture.Roots[1] {
		t.Fatalf("%s has the parent %q and is given the tree %s, want a child of %s given a root outside its parent chain",
			child, parents[child], tree, seeded.fixture.Roots[1])
	}
	seedTree(t, seeded, child, tree)

	whole, err := ComputeProject(t.Context(), pool, project, seeded.now)
	if err != nil {
		t.Fatalf("compute the project whole: %v", err)
	}
	wholeRows := make(map[string]Row, len(whole))
	for _, row := range whole {
		wholeRows[row.Issue] = row
	}
	// The observation has to have moved the child's root, or the history states nothing and the
	// comparison below asks the parent chain over again.
	if wholeRows[child].RootKey != tree {
		t.Fatalf("the project computed whole puts %s in the tree of %q and not of %s, so this test never made a rule read a reported tree",
			child, wholeRows[child].RootKey, tree)
	}

	differed, first := 0, ""
	note := func(difference string) {
		differed++
		if first == "" {
			first = difference
		}
	}
	for _, key := range seeded.fixture.Keys {
		alone, err := Compute(t.Context(), pool, project, []string{key}, seeded.now)
		if err != nil {
			t.Fatalf("compute %s on its own: %v", key, err)
		}
		if len(alone) > 1 {
			t.Fatalf("computing %s on its own answered %d rows, want the one row of that issue alone", key, len(alone))
		}
		row, has := wholeRows[key]
		switch {
		case len(alone) == 0 && has:
			note(fmt.Sprintf("%s has no row computed on its own and this row computed with its project: %s", key, show(row)))
		case len(alone) == 1 && !has:
			note(fmt.Sprintf("%s has no row computed with its project and this row computed on its own: %s", key, show(alone[0])))
		case len(alone) == 1:
			if difference := rowDifference(alone[0], row); difference != "" {
				note(fmt.Sprintf("%s computed on its own against the same issue computed with its project: %s", key, difference))
			}
		}
	}

	if len(wholeRows) == 0 || len(seeded.fixture.Keys) == 0 {
		t.Fatalf("the project holds %d issues and the whole-project computation answered %d rows, so this test examined nothing",
			len(seeded.fixture.Keys), len(wholeRows))
	}
	if differed > 0 {
		t.Errorf("%d of the %d issues hold different facts computed alone and computed with their project; the first is %s. The child %s reports the tree %s",
			differed, len(seeded.fixture.Keys), first, child, tree)
	}
	t.Logf("%d issues compared field by field, each computed alone against the project computed whole; %d differed. The child %s reports the tree %s, whose parent chain holds %s instead",
		len(seeded.fixture.Keys), differed, child, tree, parents[child])
}

// seedTree writes one `tree` observation of an issue as the sampler writes it: the issue's top
// issue as the daemon named it, which the tree rule reads before any parent link. The moment is
// the computation's own, so the row is the newest the daemon reports of the issue.
func seedTree(t *testing.T, seeded history, issue, tree string) {
	t.Helper()
	if _, err := seeded.database.Pool.Exec(t.Context(), `
		insert into legion_observations (project, issue_key, field, value, observed_at)
		values ($1, $2, $3, $4::jsonb, $5)
	`, seeded.fixture.Project, issue, legion.FieldTree, fmt.Sprintf("%q", tree), seeded.now); err != nil {
		t.Fatalf("seed the tree %s of %s: %v", tree, issue, err)
	}
}
