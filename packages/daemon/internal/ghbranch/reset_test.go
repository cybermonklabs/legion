package ghbranch

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/githubrest"
	"github.com/sjawhar/legion/daemon/internal/leakguard"
	"github.com/sjawhar/legion/daemon/internal/leakguard/leakguardtest"
)

// The commits of a next change's branch: mergedCommit is the head the last pull request merged
// (O), baseHead the base's head after its squash (M), laterCommit a commit pushed on the branch
// after the merge, and holdingCommit a commit that descends from the base's head.
const (
	mergedCommit  = "0000000000000000000000000000000000000001"
	baseHead      = "0000000000000000000000000000000000000002"
	laterCommit   = "0000000000000000000000000000000000000003"
	holdingCommit = "0000000000000000000000000000000000000004"
)

const resetBranch = "legion/ACME-1"

// reset is Reset of legion/ACME-1 on acme/widgets from base, with fallback, from the merged head,
// with no private-name rules.
func (g *github) reset(t *testing.T, base, fallback string) (Result, error) {
	t.Helper()
	api := g.serve(t)
	return Reset(context.Background(), widgets(api), graphQL(api), ghrepo.MustParse("acme/widgets"), resetBranch, base, fallback, mergedCommit, nil)
}

// graphQL is GitHub's GraphQL endpoint under api, called with the installation token.
func graphQL(api string) githubrest.Client {
	return githubrest.Client{Token: "installation-token", API: githubrest.GraphQLEndpoint(api)}
}

// wantNoWrite fails the test for any create or move the fake answered.
func (g *github) wantNoWrite(t *testing.T) {
	t.Helper()
	if len(g.creates) != 0 || len(g.patches) != 0 {
		t.Fatalf("creates %v and moves %v, want neither", g.creates, g.patches)
	}
}

// A branch that stands at the merged head moves to the base's head: one move of its own ref, with
// force, since a squash merge leaves the base on a commit that does not descend from the merged
// head, and made only while the branch still points at the merged head (updateRefs's beforeOid).
// Nothing is created and nothing is compared.
func TestResetMovesABranchAtTheMergedHeadToTheBase(t *testing.T) {
	g := &github{refs: map[string]string{"main": baseHead, resetBranch: mergedCommit}}
	got, err := g.reset(t, "main", "main")
	if err != nil {
		t.Fatalf("Reset: %v", err)
	}
	if want := (Result{Outcome: Moved, Base: "main", Head: baseHead, Tip: mergedCommit}); got != want {
		t.Fatalf("Reset = %+v, want %+v", got, want)
	}
	if len(g.patches) != 1 || g.patches[0].branch != resetBranch || g.patches[0].body["sha"] != baseHead ||
		g.patches[0].body["before"] != mergedCommit || g.patches[0].body["force"] != true {
		t.Fatalf("moves = %+v, want one of %s from %s to %s with force", g.patches, resetBranch, mergedCommit, baseHead)
	}
	if len(g.creates) != 0 || len(g.compares) != 0 {
		t.Fatalf("creates %v and compares %v, want neither", g.creates, g.compares)
	}
	if want := []string{"/repos/acme/widgets/git/ref/heads/main", "/repos/acme/widgets/git/ref/heads/" + resetBranch, "/repos/acme/widgets"}; !slices.Equal(g.reads(), want) {
		t.Fatalf("reads %v, want %v", g.reads(), want)
	}
}

// A move GitHub refuses while the branch still points at the merged head is an error naming the
// ref, both commits and GitHub's answer.
func TestResetNamesTheMoveOfTheBranchGitHubRefused(t *testing.T) {
	g := &github{refs: map[string]string{"main": baseHead, resetBranch: mergedCommit}, refuse: "Object does not exist"}
	_, err := g.reset(t, "main", "main")
	want := "move refs/heads/" + resetBranch + " on acme/widgets from " + mergedCommit + " to " + baseHead +
		`: GitHub answered the GraphQL query with errors: Object does not exist`
	if errorText(err) != want {
		t.Fatalf("Reset error = %q, want %q", errorText(err), want)
	}
	if g.refs[resetBranch] != mergedCommit {
		t.Fatalf("the branch is at %s after the refused move, want it left at %s", g.refs[resetBranch], mergedCommit)
	}
}

// A push that lands on the branch between Reset's read of its tip and its move is not overwritten:
// GitHub refuses the move, which names the merged head as the commit the branch must point at, and
// Reset reads the branch again and judges its new tip as any other. A tip that holds commits the
// base does not is left (Left), naming that tip, and one that holds the base stands (Stands).
func TestResetKeepsAPushBetweenItsReadAndItsMove(t *testing.T) {
	for _, tc := range []struct {
		name, pushed, status string
		outcome              Outcome
	}{
		{name: "a push of more work", pushed: laterCommit, status: "diverged", outcome: Left},
		{name: "a push of the base merged in", pushed: holdingCommit, status: "ahead", outcome: Stands},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := &github{refs: map[string]string{"main": baseHead, resetBranch: mergedCommit},
				pushAfterRead: map[string]string{resetBranch: tc.pushed}, compared: map[string]string{baseHead + "..." + tc.pushed: tc.status}}
			got, err := g.reset(t, "main", "main")
			if err != nil {
				t.Fatalf("Reset: %v", err)
			}
			if want := (Result{Outcome: tc.outcome, Base: "main", Head: baseHead, Tip: tc.pushed}); got != want {
				t.Fatalf("Reset = %+v, want %+v", got, want)
			}
			if len(g.patches) != 0 || g.refs[resetBranch] != tc.pushed {
				t.Fatalf("moves %v, the branch at %s; want the push %s kept", g.patches, g.refs[resetBranch], tc.pushed)
			}
		})
	}
}

// A branch GitHub does not have is created at the base's head, and nothing is moved.
func TestResetMakesAMissingBranchAtTheBase(t *testing.T) {
	g := &github{refs: map[string]string{"main": baseHead}, create: func() (int, string) { return http.StatusCreated, `{}` }}
	got, err := g.reset(t, "main", "main")
	if err != nil {
		t.Fatalf("Reset: %v", err)
	}
	if want := (Result{Outcome: Made, Base: "main", Head: baseHead}); got != want {
		t.Fatalf("Reset = %+v, want %+v", got, want)
	}
	if len(g.creates) != 1 || g.creates[0]["ref"] != "refs/heads/"+resetBranch || g.creates[0]["sha"] != baseHead || len(g.patches) != 0 {
		t.Fatalf("creates %v and moves %v, want one create at %s and no move", g.creates, g.patches, baseHead)
	}
}

// A branch whose tip is not the merged head and does not hold the base's head is left as it
// stands: it holds commits after the merge (GitHub's compare answers `diverged`), or it stands
// behind the base. Nothing is written, and the answer names the tip, so the caller can tell whoever
// must merge the base forward.
func TestResetLeavesABranchWithCommitsAfterTheMerge(t *testing.T) {
	for _, status := range []string{"diverged", "behind"} {
		t.Run(status, func(t *testing.T) {
			g := &github{refs: map[string]string{"main": baseHead, resetBranch: laterCommit},
				compared: map[string]string{baseHead + "..." + laterCommit: status}}
			got, err := g.reset(t, "main", "main")
			if err != nil {
				t.Fatalf("Reset: %v", err)
			}
			if want := (Result{Outcome: Left, Base: "main", Head: baseHead, Tip: laterCommit}); got != want {
				t.Fatalf("Reset = %+v, want %+v", got, want)
			}
			g.wantNoWrite(t)
			if want := []string{baseHead + "..." + laterCommit + "?per_page=1"}; !slices.Equal(g.compares, want) {
				t.Fatalf("compares %v, want %v", g.compares, want)
			}
		})
	}
}

// A branch already at the base's head, or at a commit that holds it (GitHub's compare answers
// `ahead`), is left as it stands, so a second reset of the same change writes nothing.
func TestResetLeavesABranchAtOrHoldingTheBase(t *testing.T) {
	for _, tc := range []struct {
		name, tip string
		compared  map[string]string
	}{
		{name: "at the base's head", tip: baseHead},
		{name: "holding the base's head", tip: holdingCommit, compared: map[string]string{baseHead + "..." + holdingCommit: "ahead"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := &github{refs: map[string]string{"main": baseHead, resetBranch: tc.tip}, compared: tc.compared}
			got, err := g.reset(t, "main", "main")
			if err != nil {
				t.Fatalf("Reset: %v", err)
			}
			if want := (Result{Outcome: Stands, Base: "main", Head: baseHead, Tip: tc.tip}); got != want {
				t.Fatalf("Reset = %+v, want %+v", got, want)
			}
			g.wantNoWrite(t)
		})
	}
}

// A base GitHub no longer has is replaced by the recorded default branch, and the branch moves to
// that branch's head. Where neither is on GitHub, the refusal names the fallback, and nothing is
// read of the branch or written.
func TestResetFallsBackToTheDefaultBranchForAGoneBase(t *testing.T) {
	g := &github{refs: map[string]string{"main": baseHead, resetBranch: mergedCommit}}
	got, err := g.reset(t, "release/v7.1", "main")
	if err != nil {
		t.Fatalf("Reset: %v", err)
	}
	if want := (Result{Outcome: Moved, Base: "main", Head: baseHead, Tip: mergedCommit}); got != want {
		t.Fatalf("Reset = %+v, want %+v", got, want)
	}
	if len(g.patches) != 1 || g.patches[0].body["sha"] != baseHead {
		t.Fatalf("moves = %+v, want one to %s", g.patches, baseHead)
	}

	gone := &github{refs: map[string]string{resetBranch: mergedCommit}}
	_, err = gone.reset(t, "release/v7.1", "main")
	if want := "read refs/heads/main of acme/widgets: GitHub has no such branch"; errorText(err) != want {
		t.Fatalf("Reset with no base = %q, want %q", errorText(err), want)
	}
	gone.wantNoWrite(t)
	if want := []string{"/repos/acme/widgets/git/ref/heads/release/v7.1", "/repos/acme/widgets/git/ref/heads/main"}; !slices.Equal(gone.reads(), want) {
		t.Fatalf("reads %v, want the base and the fallback alone", gone.reads())
	}
}

// A branch name the deployment's rules match is refused before anything is sent, as Create
// refuses it.
func TestResetRefusesABranchNameTheRulesMatch(t *testing.T) {
	rules, err := leakguard.LoadRules([]byte(leakguardtest.Rules))
	if err != nil {
		t.Fatalf("compile the fixture rules: %v", err)
	}
	g := &github{refs: map[string]string{"main": baseHead}}
	api := g.serve(t)
	_, err = Reset(context.Background(), widgets(api), graphQL(api), ghrepo.MustParse("acme/widgets"), "legion/"+leakguardtest.RulesHost, "main", "main", mergedCommit, rules)
	var refused *leakguard.Refusal
	if !errors.As(err, &refused) {
		t.Fatalf("Reset of a branch the rules match = %v, want a leak guard refusal", err)
	}
	if len(g.reads()) != 0 {
		t.Fatalf("GitHub was asked for %v, want nothing", g.reads())
	}
	g.wantNoWrite(t)
}
