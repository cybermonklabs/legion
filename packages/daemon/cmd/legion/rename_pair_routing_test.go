package main

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/appauth"
	legionclaim "github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/ghconfig"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/ghroute"
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
	served := servesOneOf([]ghrepo.Repository{ghrepo.MustParse("cybermonklabs/legion")})
	credential := appauth.GitHubCredential(bothOwnersInstalled{}, served, pair, nil, nil, slog.Default())
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

// For a configured pair the routing key is the repository, and ghroute.DirFor is the one pick
// both wrappers make by it (runGitCredential, runGh): the pair's `from` repository is answered
// from the directory of the owner it moved to, whose installation holds it, and every other
// repository of the `from` owner, and that owner named with no repository at all, from its own
// directory, which is the only lease in the bundle that reaches the account's other
// repositories. The pairs come from the bundle's own file and nowhere else
// (ghconfig.PairsFile), so the two wrappers and the daemon that wrote the directories cannot
// disagree about what moved.
func TestThePairsRepositoryTakesTheDirectoryOfTheOwnerHoldingItAndItsSiblingsTheirOwn(t *testing.T) {
	// Not parallel: it sets environment variables of the whole process; it sets the package variable gitCredentialStdin (runGitCredentialHere).
	root := pairRoutedBundle(t)
	for _, tc := range []struct{ name, owner, repository, dir string }{
		{"the pair's from repository", "sjawhar", "legion", "cybermonklabs"},
		{"the pair's to repository", "cybermonklabs", "legion", "cybermonklabs"},
		{"a sibling of the from owner", "sjawhar", "ygo", "sjawhar"},
		{"the from owner with no repository named", "sjawhar", "", "sjawhar"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir, err := ghroute.DirFor(root, tc.owner, tc.repository)
			if err != nil || dir != ghconfig.Dir(root, tc.dir) {
				t.Fatalf("ghroute.DirFor(%q, %q) = %q, %v; want %s's directory", tc.owner, tc.repository, dir, err, tc.dir)
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

// A gh command is routed by every repository it names, each through ghroute.DirFor, and by every
// owner a selector names alone, to that owner's own directory, and it runs only where they all
// route to one directory (ghroute.TargetOf). The pair's `from` repository beside a sibling of the
// owner it moved from names one owner and two directories, and is refused naming both, as is the
// moved repository beside its old owner named alone; the moved repository named by both of its
// names is one directory, cybermonklabs's. A command naming one repository, two siblings, or the
// owner alone runs with the directory it ran with before. The refusals the routing itself makes
// stay: an owner the bundle holds no directory for, and a pair whose `to` owner it holds none
// for, which names the pair.
func TestAGhCommandRunsWithTheOneDirectoryEveryRepositoryItNamesRoutesTo(t *testing.T) {
	t.Parallel()
	root := pairRoutedBundle(t)
	routed := func(owner, name string) (string, error) { return ghroute.DirFor(root, owner, name) }
	for _, tc := range []struct {
		argv        []string
		dir         string   // the owner whose directory the command runs with; "" where it is refused
		owner, repo string   // the repository the leak guard is told the call acts on
		refuse      []string // what the refusal names
	}{
		{argv: []string{"search", "issues", "--repo", "sjawhar/legion"}, dir: "cybermonklabs", owner: "sjawhar", repo: "legion"},
		{argv: []string{"api", "repos/sjawhar/legion/pulls"}, dir: "cybermonklabs", owner: "sjawhar", repo: "legion"},
		{argv: []string{"search", "issues", "--repo", "sjawhar/ygo"}, dir: "sjawhar", owner: "sjawhar", repo: "ygo"},
		{argv: []string{"search", "issues", "--owner", "sjawhar"}, dir: "sjawhar", owner: "sjawhar"},
		{argv: []string{"search", "issues", "--repo", "sjawhar/ygo", "--repo", "sjawhar/other"}, dir: "sjawhar", owner: "sjawhar"},
		{argv: []string{"search", "issues", "--repo", "cybermonklabs/legion", "--repo", "sjawhar/legion"}, dir: "cybermonklabs"},
		{argv: []string{"search", "issues", "--owner", "cybermonklabs", "--repo", "sjawhar/legion"}, dir: "cybermonklabs"},
		{argv: []string{"search", "issues", "--repo", "sjawhar/legion", "--repo", "sjawhar/ygo"},
			refuse: []string{"more than one owner", "cybermonklabs (a -R value naming sjawhar/legion)", "sjawhar (a -R value naming sjawhar/ygo)", "sends no token"}},
		{argv: []string{"search", "issues", "repo:sjawhar/ygo", "repo:sjawhar/legion"},
			refuse: []string{"cybermonklabs (a repo: qualifier naming sjawhar/legion)", "sjawhar (a repo: qualifier naming sjawhar/ygo)"}},
		{argv: []string{"search", "issues", "--owner", "sjawhar", "--repo", "sjawhar/legion"},
			refuse: []string{"cybermonklabs (a -R value naming sjawhar/legion)", "sjawhar (a --owner value naming sjawhar)"}},
		{argv: []string{"search", "issues", "--repo", "sjawhar/ygo", "--repo", "other/thing"}, refuse: []string{`the role's GitHub App is not installed on "other"`}},
	} {
		target, err := ghTargetOfKept(t, tc.argv, nil, savedGhHelp, noGitRepository, routed)
		if tc.refuse != nil {
			if err == nil {
				t.Errorf("ghroute.TargetOf(%q) = %+v, want a refusal", tc.argv, target)
				continue
			}
			for _, want := range tc.refuse {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("ghroute.TargetOf(%q) refused with %q, want it to name %q", tc.argv, err, want)
				}
			}
			continue
		}
		if err != nil || target.Dir != ghconfig.Dir(root, tc.dir) || target.Owner != tc.owner || target.Repo != tc.repo {
			t.Errorf("ghroute.TargetOf(%q) = %+v, %v; want %s's directory and the repository %q of %q", tc.argv, target, err, tc.dir, tc.repo, tc.owner)
		}
	}

	// With no cybermonklabs directory, the pair routes its repository nowhere, and a command
	// naming it beside a sibling is refused for the pair before the two are compared; the
	// siblings alone still run with sjawhar's directory.
	missing := filepath.Join(t.TempDir(), "gh")
	if _, err := ghconfig.WriteBundle(missing, ghconfig.Bundle{
		Default: "sjawhar",
		Dirs:    map[string]ghconfig.Rendered{"sjawhar": ghconfig.Render("ghs_implement_write_sjawhar", "implement", time.Now().Add(time.Hour))},
		Pairs:   []ghrepo.Rename{ghrepo.MustRename("sjawhar/legion", "cybermonklabs/legion")},
	}); err != nil {
		t.Fatal(err)
	}
	routedMissing := func(owner, name string) (string, error) { return ghroute.DirFor(missing, owner, name) }
	argv := []string{"search", "issues", "--repo", "sjawhar/ygo", "--repo", "sjawhar/legion"}
	if target, err := ghTargetOfKept(t, argv, nil, savedGhHelp, noGitRepository, routedMissing); err == nil || !strings.Contains(err.Error(), "sjawhar/legion -> cybermonklabs/legion") {
		t.Errorf("ghroute.TargetOf(%q) = %+v, %v; want a refusal naming the pair", argv, target, err)
	}
	argv = []string{"search", "issues", "--repo", "sjawhar/ygo", "--repo", "sjawhar/other"}
	if target, err := ghTargetOfKept(t, argv, nil, savedGhHelp, noGitRepository, routedMissing); err != nil || target.Dir != ghconfig.Dir(missing, "sjawhar") {
		t.Errorf("ghroute.TargetOf(%q) = %+v, %v; want sjawhar's directory", argv, target, err)
	}
}

// A pair whose `to` owner the bundle holds no directory for — the App is installed on no such
// account, or GitHub refused that owner's lease and the mint left the directory out — refuses the
// request, naming the pair and the directory it has not. No token of the bundle reaches the
// repository that pair moved, and the `from` owner's, which routing by owner would hand out, is
// a push GitHub refuses at the remote and a read it answers 404: failing closed is what says so
// where the request is made.
func TestAPairWhoseNewOwnerHasNoDirectoryRefusesNamingThePair(t *testing.T) {
	// Not parallel: it sets environment variables of the whole process; it sets the package variable gitCredentialStdin (runGitCredentialHere).
	root := filepath.Join(t.TempDir(), "gh")
	if _, err := ghconfig.WriteBundle(root, ghconfig.Bundle{
		Default: "sjawhar",
		Dirs:    map[string]ghconfig.Rendered{"sjawhar": ghconfig.Render("ghs_implement_write_sjawhar", "implement", time.Now().Add(time.Hour))},
		Pairs:   []ghrepo.Rename{ghrepo.MustRename("sjawhar/legion", "cybermonklabs/legion")},
	}); err != nil {
		t.Fatal(err)
	}
	if dir, err := ghroute.DirFor(root, "sjawhar", "legion"); err == nil {
		t.Fatalf("ghroute.DirFor answered %q, want a refusal: the bundle holds no cybermonklabs directory", dir)
	} else {
		for _, want := range []string{"sjawhar/legion -> cybermonklabs/legion", "cybermonklabs", "holds no gh directory"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("ghroute.DirFor refused with %q, want it to name %q", err, want)
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

// A bundle root with no pairs file routes by owner, which is exactly what the daemon before this
// change did: the file arrives in a running pod only with the pod, since the gh volume is an
// itemised Secret projection (runtime/sandbox's manifest.go), so a pod launched by the daemon
// before it holds none. Refusing there would take every `gh` call and every push of such a pod
// for as long as the roll lasted, and the drift check replaces that pod on its own
// (sandbox's movedGhDirs reads the file's presence).
func TestABundleWithNoPairsFileRoutesByOwnerAsTheDaemonBeforeItWrote(t *testing.T) {
	// Not parallel: it sets environment variables of the whole process; it sets the package variable gitCredentialStdin (runGitCredentialHere).
	root := pairRoutedBundle(t)
	if err := os.Remove(filepath.Join(root, ghconfig.PairsFile)); err != nil {
		t.Fatal(err)
	}
	if dir, err := ghroute.DirFor(root, "sjawhar", "legion"); err != nil || dir != ghconfig.Dir(root, "sjawhar") {
		t.Fatalf("ghroute.DirFor with no %s = %q, %v; want sjawhar's own directory", ghconfig.PairsFile, dir, err)
	}
	t.Setenv("GH_CONFIG_DIR", ghconfig.Dir(root, "sjawhar"))
	t.Setenv("LEGION_GH_OWNERS_DIR", root)
	code, stdout, stderr := runGitCredentialHere(t, "protocol=https\nhost=github.com\npath=/sjawhar/legion.git\n\n", "get")
	want := "username=x-access-token\npassword=ghs_implement_write_sjawhar\n"
	if code != 0 || stdout != want || stderr != "" {
		t.Fatalf("legion git-credential get with no %s = exit %d, stdout %q, stderr %q; want exit 0 and %q", ghconfig.PairsFile, code, stdout, stderr, want)
	}
}

// A pairs file that is present and holds a line that is no pair refuses, naming the line: the
// file is the daemon's own writing, so a line it cannot read means the bundle is not the one the
// daemon wrote, and routing by owner there would hand a request for a moved repository the lease
// that cannot reach it. The absent file above is the older daemon's bundle; this is a damaged
// one, which is why the two answer differently.
func TestAMalformedPairsFileRefusesNamingTheLine(t *testing.T) {
	// Not parallel: it sets environment variables of the whole process; it sets the package variable gitCredentialStdin (runGitCredentialHere).
	root := pairRoutedBundle(t)
	if err := os.WriteFile(filepath.Join(root, ghconfig.PairsFile), []byte("sjawhar/legion cybermonklabs/legion\nsjawhar/ygo\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if dir, err := ghroute.DirFor(root, "sjawhar", "legion"); err == nil {
		t.Fatalf("ghroute.DirFor answered %q, want a refusal: line 2 of %s is no pair", dir, ghconfig.PairsFile)
	} else {
		for _, want := range []string{"line 2", ghconfig.PairsFile, "sjawhar/ygo"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("ghroute.DirFor refused with %q, want it to name %q", err, want)
			}
		}
	}
	t.Setenv("GH_CONFIG_DIR", ghconfig.Dir(root, "sjawhar"))
	t.Setenv("LEGION_GH_OWNERS_DIR", root)
	code, stdout, stderr := runGitCredentialHere(t, "protocol=https\nhost=github.com\npath=/sjawhar/legion.git\n\n", "get")
	if code != 1 || stdout != "" || !strings.Contains(stderr, "line 2") {
		t.Fatalf("legion git-credential get over a damaged %s = exit %d, stdout %q, stderr %q; want exit 1, nothing answered and the line named", ghconfig.PairsFile, code, stdout, stderr)
	}
}
