package appauth

import (
	"context"
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
// cybermonklabs and sjawhar both. Each of the two directories holds its own owner's lease
// whichever repository the issue is recorded on, the pair's `from` among them: the App's
// installation on sjawhar makes that account an owner in its own right, and the repository the
// pair moved is reached through the directory of the owner GitHub holds it under.
func TestEveryDirectoryHoldsTheLeaseOfTheOwnerItsNameMeans(t *testing.T) {
	served := []string{"cybermonklabs/legion", "cybermonklabs/tools", "sjawhar/tools"}
	repos := projectRepos(served...)
	renames := ghrepo.MustRenames(legionRename)
	for _, rec := range []string{"sjawhar/tools", "sjawhar/legion", "cybermonklabs/tools", "cybermonklabs/legion"} {
		t.Run("an issue recorded on "+rec, func(t *testing.T) {
			credential := GitHubCredential(ownerTokens{}, installedOnOwners("cybermonklabs", "sjawhar"), repos,
				renames, nil, nil, quietBundleLog())
			bundle, err := credential(context.Background(), claim.RoleImplementer, ghrepo.MustParse(rec))
			if err != nil {
				t.Fatalf("credential: %v", err)
			}
			for dir, token := range bundleTokens(t, bundle) {
				if strings.Contains(dir, "/") {
					t.Fatalf("the bundle holds the repository directory %s: a directory is an owner account", dir)
				}
				owner, ok := leaseOwner(token)
				if !ok {
					t.Fatalf("the lease of %s names no owner: %q", dir, token)
				}
				if owner != dir {
					t.Errorf("the directory %s holds a lease minted on %s, want %s: a directory holds the lease of the owner its own name means",
						dir, owner, dir)
				}
			}
		})
	}
}
