package ghbranch

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/githubtest"
	"github.com/sjawhar/legion/daemon/internal/leakguard"
)

// OpenPullRequest opens a pull request of a branch against a base and answers its number; a second
// open of the same branch, which GitHub refuses with 422 because a pull request of it is open,
// answers the number of the one that is open, and opens none.
func TestOpenPullRequestAnswersTheOneThatIsOpen(t *testing.T) {
	g := githubtest.GitRepository(t, "acme", "widgets", "main")
	api, _ := serveRepo(t, g)
	repo := ghrepo.MustParse("acme/widgets")
	g.Commit(t, "legion/ACME-1.e", map[string]string{"e.txt": "e\n"}, "e")
	ctx := context.Background()

	first, err := OpenPullRequest(ctx, widgets(api), repo, "legion/ACME-1.e", "main", "ACME-1: e", "Dispatch: ACME-1")
	if err != nil || first != 1 {
		t.Fatalf("OpenPullRequest = %d, %v; want pull request 1", first, err)
	}
	second, err := OpenPullRequest(ctx, widgets(api), repo, "legion/ACME-1.e", "main", "ACME-1: e", "Dispatch: ACME-1")
	if err != nil || second != first {
		t.Fatalf("OpenPullRequest again = %d, %v; want the open one, %d", second, err, first)
	}
	if pulls := g.PullRequests(); len(pulls) != 1 || pulls[0].Head != "legion/ACME-1.e" || pulls[0].Base != "main" ||
		pulls[0].Title != "ACME-1: e" || pulls[0].Body != "Dispatch: ACME-1" {
		t.Fatalf("the pull requests GitHub holds = %+v, want the one of legion/ACME-1.e against main", pulls)
	}
}

// A pull request's title and body are published on GitHub as a created ref's name is, so a text
// this repository's own private-name rules match is refused before anything is sent.
func TestOpenPullRequestRefusesATextTheRulesMatch(t *testing.T) {
	g := githubtest.GitRepository(t, "acme", "widgets", "main")
	api, paths := serveRepo(t, g)
	g.Commit(t, "legion/ACME-1.e", map[string]string{"e.txt": "e\n"}, "e")

	_, err := OpenPullRequest(context.Background(), widgets(api), ghrepo.MustParse("acme/widgets"), "legion/ACME-1.e", "main", "ACME-1: e",
		"Built on "+theHost)

	var refused *leakguard.Refusal
	if !errors.As(err, &refused) {
		t.Fatalf("OpenPullRequest with a body naming a host = %v, want a leak guard refusal", err)
	}
	if want := []string{"the pull request's body: " + theClass}; !slices.Equal(refused.Lines, want) {
		t.Errorf("the report is %q, want %q", refused.Lines, want)
	}
	if sent := paths(); len(sent) != 0 {
		t.Errorf("GitHub was asked for %v, want nothing", sent)
	}
}
