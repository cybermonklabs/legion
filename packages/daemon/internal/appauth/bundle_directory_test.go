package appauth

import (
	"context"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
)

// A repository directory holds the lease of the owner that serves the repository its own name
// resolves to, and of no other owner, so no directory named for one owner hands out a token
// minted on another owner's installation — which is what a `gh` command spelling that name
// would be refused for. The name resolves through the configured pair exactly as a recorded
// repository does (ghrepo.Renames.Serving over the repositories the project serves), which is
// why the directory `old-owner/repo` legitimately holds a lease minted on `new-owner`: that
// name is the one the rename moved a repository away from, and a `gh` command spelling it means
// that repository.
//
// The case that names this rule: the project serves cybermonklabs/legion, cybermonklabs/tools
// and sjawhar/tools, both owners holding a repository called tools, the pair
// sjawhar/legion -> cybermonklabs/legion resolves, and the issue is recorded on sjawhar/tools.
// The bundle's repository directories are cut for the owner the pair redirects the recorded
// owner to, so the directory sjawhar/tools carries a lease minted on cybermonklabs and a `gh`
// command spelling sjawhar/tools is answered with a token that cannot reach it.
func TestEveryRepositoryDirectoryHoldsTheLeaseOfTheRepositoryItsNameResolvesTo(t *testing.T) {
	served := []string{"cybermonklabs/legion", "cybermonklabs/tools", "sjawhar/tools"}
	repos := projectRepos(served...)
	renames := ghrepo.MustRenames(legionRename)
	serves := func(candidate ghrepo.Repository) bool { return ghrepo.Serves(repos(), candidate) }
	for _, rec := range []string{"sjawhar/tools", "sjawhar/legion", "cybermonklabs/tools", "cybermonklabs/legion"} {
		t.Run("an issue recorded on "+rec, func(t *testing.T) {
			credential := GitHubCredential(ownerTokens{}, repos, renames, nil, quietBundleLog())
			bundle, err := credential(context.Background(), claim.RoleImplementer, ghrepo.MustParse(rec))
			if err != nil {
				t.Fatalf("credential: %v", err)
			}
			for dir, token := range bundleTokens(t, bundle) {
				if !strings.Contains(dir, "/") {
					continue
				}
				named := renames.Serving(ghrepo.MustParse(dir), serves)
				owner, ok := leaseOwner(token)
				if !ok {
					t.Fatalf("the lease of %s names no owner: %q", dir, token)
				}
				if want := strings.ToLower(named.Owner()); owner != want {
					t.Errorf("the directory %s holds a lease minted on %s, want %s: a repository directory holds the lease of the owner that serves the repository its own name resolves to",
						dir, owner, want)
				}
			}
		})
	}
}
