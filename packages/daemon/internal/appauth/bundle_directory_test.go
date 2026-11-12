package appauth

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
)

// Every directory of a bundle holds the lease of the owner its own name means, and of no other,
// so no directory named for one owner hands out a token minted on another owner's
// installation — which is what a `gh` command spelling that name would be refused for. A
// directory is an owner and never a repository: a lease reaches every repository its
// installation holds, so there is nothing narrower for a name to stand for (INFRA-2543).
//
// The one name that means another owner's installation is an owner a resolving rename moved a
// repository away from while the App is installed on no account of that name: it means the
// repository the pair moved, which GitHub redirects to the owner it moved to, and the lease it
// holds is that owner's.
//
// The case: the project serves cybermonklabs/legion, cybermonklabs/tools and sjawhar/tools, the
// pair sjawhar/legion -> cybermonklabs/legion resolves, and the App is installed on
// cybermonklabs and sjawhar both. Each of the two directories then holds its own owner's lease,
// whichever repository the issue is recorded on, bar the two a write lease stands in: the
// issue's recorded owner's directory and that of the owner whose installation serves the
// repository it works.
func TestEveryDirectoryHoldsTheLeaseOfTheOwnerItsNameMeans(t *testing.T) {
	served := []string{"cybermonklabs/legion", "cybermonklabs/tools", "sjawhar/tools"}
	repos := projectRepos(served...)
	renames := ghrepo.MustRenames(legionRename)
	serves := func(candidate ghrepo.Repository) bool { return ghrepo.Serves(repos(), candidate) }
	for _, rec := range []string{"sjawhar/tools", "sjawhar/legion", "cybermonklabs/tools", "cybermonklabs/legion"} {
		t.Run("an issue recorded on "+rec, func(t *testing.T) {
			recorded := ghrepo.MustParse(rec)
			credential := GitHubCredential(ownerTokens{}, installedOnOwners("cybermonklabs", "sjawhar"), repos,
				renames, nil, nil, quietBundleLog())
			bundle, err := credential(context.Background(), claim.RoleImplementer, recorded)
			if err != nil {
				t.Fatalf("credential: %v", err)
			}
			// The write lease is minted on the installation that serves the issue's own
			// repository and stands in two directories: the recorded owner's and that owner's.
			serving := renames.Serving(recorded, serves)
			writeAt := []string{strings.ToLower(recorded.Owner()), strings.ToLower(serving.Owner())}
			for dir, token := range bundleTokens(t, bundle) {
				if strings.Contains(dir, "/") {
					t.Fatalf("the bundle holds the repository directory %s: a directory is an owner account", dir)
				}
				owner, ok := leaseOwner(token)
				if !ok {
					t.Fatalf("the lease of %s names no owner: %q", dir, token)
				}
				want := dir
				if slices.Contains(writeAt, dir) {
					want = strings.ToLower(serving.Owner())
				}
				if owner != want {
					t.Errorf("the directory %s holds a lease minted on %s, want %s: a directory holds the lease of the owner its own name means",
						dir, owner, want)
				}
			}
		})
	}
}
