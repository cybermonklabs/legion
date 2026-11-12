package appauth

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
)

// A bundle holds a lease minted on every owner the role's App is installed on, whatever a
// configured pair moved away from one of them. The pair moves one repository, not the account,
// so the owner's other repositories are reached by that owner's own installation token — a
// token reaches every repository its installation holds (INFRA-2543) — and the repository the
// pair moved is reached by the write lease the recorded owner's directory holds.
//
// The case: the project serves cybermonklabs/legion, sjawhar/oh-my-pi and sjawhar/tools, the pair
// sjawhar/legion -> cybermonklabs/legion resolves, the App is installed on both accounts, and the
// issue is recorded on sjawhar/oh-my-pi. The sjawhar directory then holds the write lease, minted
// on sjawhar, which reaches sjawhar/tools too. The fault this test caught: the bundle's leases
// were cut for the owner mintOwner answered for the owner serving the role's repository, which
// for an owner a pair moved a repository away from was the owner it moved to even where that
// account had an installation of its own, so `legion gh` on sjawhar/tools picked the sjawhar
// directory, which was minted on another owner's installation, and GitHub refused it.
func TestTheBundleReachesEveryOwnerTheAppIsInstalledOn(t *testing.T) {
	repos := projectRepos("cybermonklabs/legion", "sjawhar/oh-my-pi", "sjawhar/tools")
	credential := GitHubCredential(ownerTokens{}, installedOnOwners("cybermonklabs", "sjawhar"), repos,
		ghrepo.MustRenames(legionRename), nil, nil, quietBundleLog())
	bundle, err := credential(context.Background(), claim.RoleImplementer, ghrepo.MustParse("sjawhar/oh-my-pi"))
	if err != nil {
		t.Fatalf("credential: %v", err)
	}
	tokens := bundleTokens(t, bundle)
	minted := map[string]bool{}
	for _, token := range tokens {
		owner, ok := leaseOwner(token)
		if !ok {
			t.Fatalf("the lease %q names no owner: the fakes render every lease as <app>-token-for-<owner> or <app>-read-for-<owner>", token)
		}
		minted[owner] = true
	}
	for _, want := range []string{"cybermonklabs/legion", "sjawhar/oh-my-pi", "sjawhar/tools"} {
		if owner := strings.ToLower(ghrepo.MustParse(want).Owner()); !minted[owner] {
			t.Errorf("no lease of the bundle is minted on %s, which serves %s; the bundle holds %v minted on %v",
				owner, want, slices.Sorted(slices.Values(dirNames(tokens))), slices.Sorted(slices.Values(dirNames(minted))))
		}
	}
	if want := "implement-token-for-sjawhar"; tokens["sjawhar"] != want {
		t.Errorf("the sjawhar directory holds %q, want %q: the pair moved one repository, not the account",
			tokens["sjawhar"], want)
	}
}

// leaseOwner is the owner a fake lease token is minted on.
func leaseOwner(token string) (string, bool) {
	for _, mark := range []string{"-token-for-", "-read-for-"} {
		_, owner, found := strings.Cut(token, mark)
		if !found || owner == "" {
			continue
		}
		return owner, true
	}
	return "", false
}

// dirNames is the keys of m, for a failure that says what the bundle held.
func dirNames[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for key := range m {
		out = append(out, key)
	}
	return out
}
