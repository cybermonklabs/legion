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
	repos := func() []ghrepo.Repository { return []ghrepo.Repository{ghrepo.MustParse("cybermonklabs/legion")} }
	credential := appauth.GitHubCredential(installedOnBoth{}, installedOnBoth{}, repos, pair, nil, nil, slog.Default())
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

	gh := realGHForCapture(t)
	root, defaultDir := bothInstalledBundle(t)
	bin := t.TempDir()
	write := func(name, script string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(bin, name), []byte(script), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	write("legion", "#!/bin/sh\n"+testMainEnv+"=1 exec '"+os.Args[0]+"' \"$@\"\n")
	write("gh", "#!/bin/sh\nexec '"+filepath.Join(bin, "legion")+"' gh \"$@\"\n")
	// The gh `legion gh` runs: the real gh for its help, which the wrapper reads every command
	// with; the leak guard's visibility read answered private; and every other call printing the
	// token of the directory GH_CONFIG_DIR names, which is the pick this test reads.
	write("real-gh", "#!/bin/sh\nfor a in \"$@\"; do case \"$a\" in --help|-h|help|--version) exec '"+gh+"' \"$@\";; esac; done\n"+
		"case \"$*\" in *'--jq .visibility'*) echo private; exit 0;; esac\n"+
		"sed -n 's/^    oauth_token: //p' \"$GH_CONFIG_DIR/hosts.yml\"\n")
	home := t.TempDir()
	for _, tc := range []struct{ repo, token string }{
		{"sjawhar/legion", "ghs_implement_write_cybermonklabs"},
		{"sjawhar/ygo", "ghs_implement_write_sjawhar"},
	} {
		t.Run("gh api repos/"+tc.repo, func(t *testing.T) {
			cmd := exec.Command("gh", "api", "repos/"+tc.repo)
			cmd.Dir = home
			cmd.Env = []string{
				"PATH=" + bin + ":/usr/bin:/bin", "HOME=" + home,
				"XDG_CONFIG_HOME=" + filepath.Join(home, ".config"), "XDG_STATE_HOME=" + filepath.Join(home, ".state"),
				"GH_TOKEN=", "GITHUB_TOKEN=", "GH_HOST=", "GH_NO_UPDATE_NOTIFIER=1", "GH_PROMPT_DISABLED=1",
				"GH_CONFIG_DIR=" + defaultDir, "LEGION_GH_OWNERS_DIR=" + root, "LEGION_GH_PATH=" + filepath.Join(bin, "real-gh"),
				"PUBLIC_LEAK_CHECK=" + leakguardtest.Write(t, t.TempDir(), ghCheckPasses),
			}
			out, err := cmd.CombinedOutput()
			lines := strings.Split(strings.TrimSpace(string(out)), "\n")
			if err != nil || lines[len(lines)-1] != tc.token {
				t.Fatalf("legion gh api repos/%s = %v, %q; want gh run with the directory holding %s", tc.repo, err, out, tc.token)
			}
		})
	}
}
