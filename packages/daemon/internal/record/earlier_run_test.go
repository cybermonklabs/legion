package record

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
)

// An earlier run of an issue is one that ran a step, which leaves a phases row of a role other
// than the architect's that a new generation keeps: a root at its first generation, whose architect
// alone has a row, ran none; a root at generation 3 whose implementer ran at an earlier generation
// did. A child the workflow recorded at its root's generation 3 has run nothing, and a child
// entered again at generation 4 whose tester ran at an earlier generation has.
//
// The fault: the gate of a run's first step read from the generation, which takes a child recorded
// at its root's generation 3 for an issue that ran, and closes the pull requests it holds. The
// control: EarlierRunOf answering issue.Generation > 1, which fails the child recorded at
// generation 3.
func TestAnEarlierRunIsOneThatRanAStep(t *testing.T) {
	ctx := context.Background()
	st := migratedStore(t)
	records := NewStore()
	root := func(key string, generation uint64) Issue {
		return Issue{Key: key, Tree: key, Project: "LEGION", Title: key, Phase: phase.Admitted, Generation: generation, Status: "todo",
			Rank: "U", Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets"}
	}
	child := func(key, tree string, generation uint64) Issue {
		issue := root(key, generation)
		issue.Tree, issue.Parent = tree, &tree
		return issue
	}
	for _, tc := range []struct {
		name  string
		issue Issue
		ran   []claim.Role
		want  bool
	}{
		{"a root at its first generation, whose architect alone has a row", root("LEGION-401", 1), []claim.Role{claim.RoleArchitect}, false},
		{"a root at generation 3 whose implementer ran", root("LEGION-402", 3), []claim.Role{claim.RoleArchitect, claim.RoleImplementer}, true},
		{"a child recorded at its root's generation 3", child("LEGION-403", "LEGION-402", 3), nil, false},
		{"a child entered again at generation 4 whose tester ran", child("LEGION-404", "LEGION-402", 4), []claim.Role{claim.RoleTester}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			inTx(t, st, func(tx pgx.Tx) {
				must(t, records.PutIssue(ctx, tx, tc.issue))
				for _, role := range tc.ran {
					must(t, records.PutPhase(ctx, tx, PhaseRow{Issue: tc.issue.Key, Role: role, Claim: claim.Token("claim-of-" + string(role)), Entry: 2}))
				}
				// A new generation clears each row and keeps it (ClearGeneration).
				_, err := records.ClearGeneration(ctx, tx, tc.issue, forgetNothing)
				must(t, err)
				got, err := records.EarlierRunOf(ctx, tx, tc.issue)
				must(t, err)
				if got != tc.want {
					t.Fatalf("an earlier run of %s at generation %d with rows of %v = %t, want %t", tc.issue.Key, tc.issue.Generation, tc.ran, got, tc.want)
				}
			})
		})
	}
}
