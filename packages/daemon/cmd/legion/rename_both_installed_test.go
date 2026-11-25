package main

import (
	"context"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/appauth"
	legionclaim "github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/ghconfig"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/leakguard/leakguardtest"
)

// installedOnBoth is namedTokens with the App installed on both accounts of the configured pair:
// cybermonklabs, which holds the repository sjawhar/legion moved to, and sjawhar, which holds
// every other repository of that account. That is this deployment's own case: the implement App's
// sjawhar installation holds 151 repositories and not cybermonklabs/legion.
type installedOnBoth struct{ namedTokens }

func (installedOnBoth) InstalledOwners(context.Context, appauth.AppRole) ([]string, error) {
	return []string{"cybermonklabs", "sjawhar"}, nil
}

// bothInstalledBundle writes the bundle appauth.GitHubCredential builds for an implementer whose
// issue is recorded on sjawhar/legion, in a project that serves cybermonklabs/legion with the pair
// sjawhar/legion -> cybermonklabs/legion configured and the App installed on both owners.
func bothInstalledBundle(t *testing.T) (root, defaultDir string) {
	t.Helper()
	pair := ghrepo.MustRenames(ghrepo.MustRename("sjawhar/legion", "cybermonklabs/legion"))
	served := servesOneOf([]ghrepo.Repository{ghrepo.MustParse("cybermonklabs/legion")})
	credential := appauth.GitHubCredential(installedOnBoth{}, served, pair, nil, nil, slog.Default())
	bundle, err := credential(context.Background(), legionclaim.RoleImplementer, ghrepo.MustParse("sjawhar/legion"))
	if err != nil {
		t.Fatalf("build the bundle: %v", err)
	}
	root = filepath.Join(t.TempDir(), "gh")
	if _, err := ghconfig.WriteBundle(root, bundle); err != nil {
		t.Fatal(err)
	}
	return root, ghconfig.Dir(root, bundle.Default)
}

// A repository a configured pair moved stays writable under the name it moved away from while the
// App is installed on both accounts: for a configured pair the routing key is the repository, not
// the owner. A tree recorded on sjawhar/legion pushes to the origin its clone names,
// https://github.com/sjawhar/legion, which GitHub serves as cybermonklabs/legion, so `legion
// git-credential` and `legion gh` must hand that request the lease of cybermonklabs, whose
// installation holds the repository; the sjawhar installation does not hold it, and its token
// cannot push there. Every other repository of sjawhar is reached by sjawhar's own lease, since
// the pair moved one repository and not the account. R2: a role can push to any repository its
// App is installed on.
func TestARenamedRepositoryKeepsItsNewOwnersLeaseWhileTheAppIsInstalledOnBothOwners(t *testing.T) {
	for _, tc := range []struct{ path, token string }{
		{"/sjawhar/legion.git", "ghs_implement_write_cybermonklabs"},
		{"/cybermonklabs/legion.git", "ghs_implement_write_cybermonklabs"},
		{"/sjawhar/ygo.git", "ghs_implement_write_sjawhar"},
	} {
		t.Run("git-credential "+tc.path, func(t *testing.T) {
			root, defaultDir := bothInstalledBundle(t)
			t.Setenv("GH_CONFIG_DIR", defaultDir)
			t.Setenv("LEGION_GH_OWNERS_DIR", root)
			code, stdout, stderr := runGitCredentialHere(t, "protocol=https\nhost=github.com\npath="+tc.path+"\n\n", "get")
			want := "username=x-access-token\npassword=" + tc.token + "\n"
			if code != 0 || stdout != want || stderr != "" {
				t.Fatalf("legion git-credential get for %s = exit %d, stdout %q, stderr %q; want exit 0 and %q", tc.path, code, stdout, stderr, want)
			}
		})
	}

	run := bothInstalledGh(t)
	for _, tc := range []struct{ repo, token string }{
		{"sjawhar/legion", "ghs_implement_write_cybermonklabs"},
		{"sjawhar/ygo", "ghs_implement_write_sjawhar"},
	} {
		t.Run("gh api repos/"+tc.repo, func(t *testing.T) {
			code, out := run(t, "api", "repos/"+tc.repo)
			if code != 0 || lastLine(out) != tc.token {
				t.Fatalf("legion gh api repos/%s = exit %d, %q; want gh run with the directory holding %s", tc.repo, code, out, tc.token)
			}
		})
	}
}

// `legion gh` decides by the directory each repository a command names is routed to, and never
// by the owner alone. `gh search prs --repo sjawhar/legion --repo sjawhar/ygo` names one owner and
// two directories: the pair moved sjawhar/legion to cybermonklabs, whose installation alone holds
// it, so run under sjawhar's token the search would read that half as nothing and answer fewer
// results with no error at all. It is refused, exit 2, naming both repositories and the directory
// each routes to, and gh never runs. Naming the moved repository by both of its names is one
// repository and one directory, and runs with cybermonklabs's. A command naming one repository,
// moved or not, and one naming the owner alone run as they did before.
func TestLegionGhRefusesACommandWhoseRepositoriesRouteToTwoDirectories(t *testing.T) {
	run := bothInstalledGh(t)
	for _, tc := range []struct {
		name  string
		args  []string
		token string   // the token of the directory gh ran with; "" where the command is refused
		named []string // what the refusal names
	}{
		{"the moved repository beside a sibling", []string{"search", "prs", "--repo", "sjawhar/legion", "--repo", "sjawhar/ygo"}, "",
			[]string{"more than one owner", "cybermonklabs (--repo sjawhar/legion)", "sjawhar (--repo sjawhar/ygo)", "sends no token"}},
		{"the moved repository by both of its names", []string{"search", "prs", "--repo", "cybermonklabs/legion", "--repo", "sjawhar/legion"}, "ghs_implement_write_cybermonklabs", nil},
		{"the moved repository alone", []string{"search", "prs", "--repo", "sjawhar/legion"}, "ghs_implement_write_cybermonklabs", nil},
		{"a sibling alone", []string{"search", "prs", "--repo", "sjawhar/ygo"}, "ghs_implement_write_sjawhar", nil},
		{"the owner alone", []string{"search", "prs", "--owner", "sjawhar"}, "ghs_implement_write_sjawhar", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			code, out := run(t, tc.args...)
			if tc.token != "" {
				if code != 0 || lastLine(out) != tc.token {
					t.Fatalf("legion gh %s = exit %d, %q; want gh run with the directory holding %s", strings.Join(tc.args, " "), code, out, tc.token)
				}
				return
			}
			// The stand-in gh prints the token it was run with, so a refusal that printed none
			// is one where gh never ran.
			if code != 2 || strings.Contains(out, "ghs_") {
				t.Fatalf("legion gh %s = exit %d, %q; want exit 2 and gh never run", strings.Join(tc.args, " "), code, out)
			}
			for _, want := range tc.named {
				if !strings.Contains(out, want) {
					t.Errorf("legion gh %s refused with %q, want it to name %q", strings.Join(tc.args, " "), out, want)
				}
			}
		})
	}
}

// bothInstalledGh runs the real `legion gh` over bothInstalledBundle through a `gh` script it
// writes, started by that script's absolute path (the call below says why), with the child's
// PATH holding that script first, as a pane's does; it answers its exit status and everything
// it printed. The gh `legion gh` runs answers its help as the real gh, which the wrapper reads
// every command with; the leak guard's visibility read as private; and every other call by
// printing the token of the directory GH_CONFIG_DIR names, which is the pick these tests read.
func bothInstalledGh(t *testing.T) func(t *testing.T, args ...string) (int, string) {
	t.Helper()
	gh := realGHForCapture(t)
	root, defaultDir := bothInstalledBundle(t)
	bin := t.TempDir()
	write := func(name, script string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(bin, name), []byte(script), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	write("legion", "#!/bin/sh\n"+testMainEnv+"=1 exec '"+legionCopy(t)+"' \"$@\"\n")
	write("gh", "#!/bin/sh\nexec '"+filepath.Join(bin, "legion")+"' gh \"$@\"\n")
	write("real-gh", "#!/bin/sh\nfor a in \"$@\"; do case \"$a\" in --help|-h|help|--version) exec '"+gh+"' \"$@\";; esac; done\n"+
		"case \"$*\" in *'--jq .visibility'*) echo private; exit 0;; esac\n"+
		"sed -n 's/^    oauth_token: //p' \"$GH_CONFIG_DIR/hosts.yml\"\n")
	home := t.TempDir()
	check := leakguardtest.Write(t, t.TempDir(), ghCheckPasses)
	return func(t *testing.T, args ...string) (int, string) {
		t.Helper()
		// By the absolute path of the `gh` this helper wrote, never the bare name: Go resolves a
		// bare name against the PATH of the process making the call and never against the one the
		// child's Env holds (docs/testing.md), and realGHForCapture has put a `gh` of its own
		// first on this process's PATH (ghWrapperFirstOnPath), so a bare name ran that one and
		// the script written here never ran at all. The child's PATH still holds bin first, so
		// every gh the wrapper resolves for itself is this helper's.
		cmd := exec.Command(filepath.Join(bin, "gh"), args...)
		cmd.Dir = home
		cmd.Env = []string{
			"PATH=" + bin + ":/usr/bin:/bin", "HOME=" + home,
			"XDG_CONFIG_HOME=" + filepath.Join(home, ".config"), "XDG_STATE_HOME=" + filepath.Join(home, ".state"),
			"GH_TOKEN=", "GITHUB_TOKEN=", "GH_HOST=", "GH_NO_UPDATE_NOTIFIER=1", "GH_PROMPT_DISABLED=1",
			"GH_CONFIG_DIR=" + defaultDir, "LEGION_GH_OWNERS_DIR=" + root, "LEGION_GH_PATH=" + filepath.Join(bin, "real-gh"),
			"PUBLIC_LEAK_CHECK=" + check,
		}
		// This binary's deadline, which bounds what the `legion gh` the wrapper runs waits for
		// (boundByTheDeadline).
		cmd.Env = append(cmd.Env, testDeadlineEnviron()...)
		out, err := cmd.CombinedOutput()
		if exit, ok := err.(*exec.ExitError); ok {
			return exit.ExitCode(), string(out)
		}
		if err != nil {
			t.Fatalf("gh %q: %v", args, err)
		}
		return 0, string(out)
	}
}

// lastLine is the last line out prints.
func lastLine(out string) string {
	lines := strings.Split(strings.TrimSpace(out), "\n")
	return lines[len(lines)-1]
}
