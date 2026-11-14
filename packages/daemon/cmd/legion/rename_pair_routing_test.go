package main

import (
	"context"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/appauth"
	legionclaim "github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/ghconfig"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
)

// bothOwnersInstalled is namedTokens answering that the App is installed on both accounts of the
// configured pair, the case one directory per owner cannot answer alone: cybermonklabs, whose
// installation holds the repository the pair moved, and sjawhar, whose holds every other
// repository of that account.
type bothOwnersInstalled struct{ namedTokens }

func (bothOwnersInstalled) InstalledOwners(context.Context, appauth.AppRole) ([]string, error) {
	return []string{"cybermonklabs", "sjawhar"}, nil
}

// pairRoutedBundle writes the bundle appauth.GitHubCredential builds for an implementer whose
// issue is recorded on sjawhar/legion, in a project that serves cybermonklabs/legion with the
// pair sjawhar/legion -> cybermonklabs/legion configured and the App installed on both owners,
// and answers its root.
func pairRoutedBundle(t *testing.T) string {
	t.Helper()
	pair := ghrepo.MustRenames(ghrepo.MustRename("sjawhar/legion", "cybermonklabs/legion"))
	repos := func() []ghrepo.Repository { return []ghrepo.Repository{ghrepo.MustParse("cybermonklabs/legion")} }
	credential := appauth.GitHubCredential(bothOwnersInstalled{}, bothOwnersInstalled{}, repos, pair, nil, nil, slog.Default())
	bundle, err := credential(context.Background(), legionclaim.RoleImplementer, ghrepo.MustParse("sjawhar/legion"))
	if err != nil {
		t.Fatalf("build the bundle: %v", err)
	}
	root := filepath.Join(t.TempDir(), "gh")
	if _, err := ghconfig.WriteBundle(root, bundle); err != nil {
		t.Fatal(err)
	}
	return root
}

// For a configured pair the routing key is the repository, and ghConfigDirFor is the one pick
// both wrappers make by it (runGitCredential, runGh): the pair's `from` repository is answered
// from the directory of the owner it moved to, whose installation holds it, and every other
// repository of the `from` owner, and that owner named with no repository at all, from its own
// directory, which is the only lease in the bundle that reaches the account's other
// repositories. The pairs come from the bundle's own file and nowhere else
// (ghconfig.PairsFile), so the two wrappers and the daemon that wrote the directories cannot
// disagree about what moved.
func TestThePairsRepositoryTakesTheDirectoryOfTheOwnerHoldingItAndItsSiblingsTheirOwn(t *testing.T) {
	root := pairRoutedBundle(t)
	for _, tc := range []struct{ name, owner, repository, dir string }{
		{"the pair's from repository", "sjawhar", "legion", "cybermonklabs"},
		{"the pair's to repository", "cybermonklabs", "legion", "cybermonklabs"},
		{"a sibling of the from owner", "sjawhar", "ygo", "sjawhar"},
		{"the from owner with no repository named", "sjawhar", "", "sjawhar"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir, err := ghConfigDirFor(root, tc.owner, tc.repository)
			if err != nil || dir != ghconfig.Dir(root, tc.dir) {
				t.Fatalf("ghConfigDirFor(%q, %q) = %q, %v; want %s's directory", tc.owner, tc.repository, dir, err, tc.dir)
			}
		})
	}
	// The same routing through the real `legion git-credential`, which is what a `jj git push`
	// of a tree cloned from the name the record names presents a token for.
	for _, tc := range []struct{ path, token string }{
		{"/sjawhar/legion.git", "ghs_implement_write_cybermonklabs"},
		{"/sjawhar/ygo.git", "ghs_implement_write_sjawhar"},
	} {
		t.Run("git-credential "+tc.path, func(t *testing.T) {
			t.Setenv("GH_CONFIG_DIR", ghconfig.Dir(root, "sjawhar"))
			t.Setenv("LEGION_GH_OWNERS_DIR", root)
			code, stdout, stderr := runGitCredentialHere(t, "protocol=https\nhost=github.com\npath="+tc.path+"\n\n", "get")
			want := "username=x-access-token\npassword=" + tc.token + "\n"
			if code != 0 || stdout != want || stderr != "" {
				t.Fatalf("legion git-credential get for %s = exit %d, stdout %q, stderr %q; want exit 0 and %q", tc.path, code, stdout, stderr, want)
			}
		})
	}
}

// A pair whose `to` owner the bundle holds no directory for — the App is installed on no such
// account, or GitHub refused that owner's lease and the mint left the directory out — refuses the
// request, naming the pair and the directory it has not. No token of the bundle reaches the
// repository that pair moved, and the `from` owner's, which routing by owner would hand out, is
// a push GitHub refuses at the remote and a read it answers 404: failing closed is what says so
// where the request is made.
func TestAPairWhoseNewOwnerHasNoDirectoryRefusesNamingThePair(t *testing.T) {
	root := filepath.Join(t.TempDir(), "gh")
	if _, err := ghconfig.WriteBundle(root, ghconfig.Bundle{
		Default: "sjawhar",
		Dirs:    map[string]ghconfig.Rendered{"sjawhar": ghconfig.Render("ghs_implement_write_sjawhar", "implement", time.Now().Add(time.Hour))},
		Pairs:   []ghrepo.Rename{ghrepo.MustRename("sjawhar/legion", "cybermonklabs/legion")},
	}); err != nil {
		t.Fatal(err)
	}
	if dir, err := ghConfigDirFor(root, "sjawhar", "legion"); err == nil {
		t.Fatalf("ghConfigDirFor answered %q, want a refusal: the bundle holds no cybermonklabs directory", dir)
	} else {
		for _, want := range []string{"sjawhar/legion -> cybermonklabs/legion", "cybermonklabs", "holds no gh directory"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("ghConfigDirFor refused with %q, want it to name %q", err, want)
			}
		}
	}
	t.Setenv("GH_CONFIG_DIR", ghconfig.Dir(root, "sjawhar"))
	t.Setenv("LEGION_GH_OWNERS_DIR", root)
	code, stdout, stderr := runGitCredentialHere(t, "protocol=https\nhost=github.com\npath=/sjawhar/legion.git\n\n", "get")
	if code != 1 || stdout != "" || !strings.Contains(stderr, "sjawhar/legion -> cybermonklabs/legion") {
		t.Fatalf("legion git-credential get for /sjawhar/legion.git = exit %d, stdout %q, stderr %q; want exit 1, nothing answered and the pair named", code, stdout, stderr)
	}
	// The owner's own repositories are served as ever: the pair moved one repository, and a
	// refusal over it is no refusal of the account.
	if code, stdout, _ := runGitCredentialHere(t, "protocol=https\nhost=github.com\npath=/sjawhar/ygo.git\n\n", "get"); code != 0 ||
		stdout != "username=x-access-token\npassword=ghs_implement_write_sjawhar\n" {
		t.Fatalf("legion git-credential get for /sjawhar/ygo.git = exit %d, stdout %q; want sjawhar's own token", code, stdout)
	}
}
