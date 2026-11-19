package daemon

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/ghconfig"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// claimSecrets writes what a tmux launch leaves under the secrets directory for token: the boot
// token file, the grant file, and the gh directory with its two files.
func claimSecrets(t *testing.T, stateDir string, token claim.Token) {
	t.Helper()
	for _, name := range []string{string(token), string(token) + "-grant"} {
		writeFile(t, runtime.SecretFilePath(stateDir, name))
	}
	for _, name := range []string{ghconfig.HostsFile, ghconfig.ConfigFile} {
		writeFile(t, filepath.Join(runtime.GHConfigDir(stateDir, token), name))
	}
}

// secretEntries are the names under the secrets directory, directories included, sorted.
func secretEntries(t *testing.T, stateDir string) []string {
	t.Helper()
	entries, err := os.ReadDir(runtime.SecretsDir(stateDir))
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	slices.Sort(names)
	return names
}

// A claim's gh directory (runtime.GHConfigDir) is one of its secret files: it goes, whole, when the
// claim is written with no process, with the boot token and grant files — while the daemon's own
// provider-env directory and another claim's gh directory, which no write of this claim names,
// stay. The provider-env directory is the one subdirectory that is nobody's claim's, and a prune
// must never take it.
func TestTheClaimPruneTakesTheGhDirectoryOfAClaimWrittenWithNoProcess(t *testing.T) {
	stateDir := t.TempDir()
	project, _ := claim.ProjectToken("omp")
	ended, _ := claim.NewToken(project, "LEGION-1", claim.RoleTester)
	live, _ := claim.NewToken(project, "LEGION-2", claim.RoleImplementer)
	claimSecrets(t, stateDir, ended)
	claimSecrets(t, stateDir, live)
	writeFile(t, filepath.Join(config.ProviderEnvDir(stateDir), "ANTHROPIC_API_KEY"))
	writeFile(t, runtime.SecretFilePath(stateDir, runtime.DispatchTokenFileName))

	store := pruningStore{dir: runtime.SecretsDir(stateDir), log: quietLogger()}
	store.written(supervise.Claim{Token: ended})

	want := []string{runtime.DispatchTokenFileName, string(live), string(live) + "-gh", string(live) + "-grant", "provider-env"}
	if got := secretEntries(t, stateDir); !slices.Equal(got, want) {
		t.Fatalf("after the claim's write the secrets directory holds %v, want %v", got, want)
	}
	if _, err := os.Stat(runtime.GHConfigDir(stateDir, ended)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the ended claim's gh directory is still there (%v)", err)
	}
	for _, name := range []string{ghconfig.HostsFile, ghconfig.ConfigFile} {
		if _, err := os.Stat(filepath.Join(runtime.GHConfigDir(stateDir, live), name)); err != nil {
			t.Errorf("the live claim's %s: %v", name, err)
		}
	}
	if _, err := os.Stat(filepath.Join(config.ProviderEnvDir(stateDir), "ANTHROPIC_API_KEY")); err != nil {
		t.Errorf("the provider key: %v", err)
	}

	// A claim written with a process keeps everything.
	store.written(supervise.Claim{Token: live, Locator: &runtime.Locator{
		Runtime: runtime.RuntimeTmux, Claim: live, Incarnation: "4242:1", Tmux: &runtime.TmuxLocator{Window: "@1", Pane: "%1"},
	}})
	if got := secretEntries(t, stateDir); !slices.Equal(got, want) {
		t.Fatalf("after the live claim's write the secrets directory holds %v, want %v", got, want)
	}
}

// releaseStore is a store whose one claim write is an operator's release (supervise.Store.
// ReleaseHolds), which answers err, or takes the write when err is nil.
type releaseStore struct {
	supervise.Store
	err error
}

func (s releaseStore) ReleaseHolds(context.Context, supervise.Claim, []supervise.Hold) ([]supervise.Hold, error) {
	return nil, s.err
}

// An operator's release writes the claim (supervise.Store.ReleaseHolds) and so is one of the
// claim writes the prune follows: a release of a claim with no process that the store took takes
// its secret files, as a PutClaim of it would, and one the store refused takes nothing, since the
// claim was not written (review of #4355).
//
// The control (the pull request records the run): pruningStore without its ReleaseHolds, so the
// embedded store's is promoted and the prune is skipped: the files are still there after a
// release the store took.
func TestAReleaseOfAClaimWithNoProcessPrunesItsSecretFiles(t *testing.T) {
	stateDir := t.TempDir()
	project, _ := claim.ProjectToken("omp")
	ended, _ := claim.NewToken(project, "LEGION-1", claim.RoleTester)
	claimSecrets(t, stateDir, ended)
	held := secretEntries(t, stateDir)
	ctx := context.Background()

	refused := pruningStore{Store: releaseStore{err: errors.New("the database refused the claim write")}, dir: runtime.SecretsDir(stateDir), log: quietLogger()}
	if _, err := refused.ReleaseHolds(ctx, supervise.Claim{Token: ended}, nil); err == nil {
		t.Fatal("the refused release answered no error")
	}
	if got := secretEntries(t, stateDir); !slices.Equal(got, held) {
		t.Fatalf("a release the store refused left the secrets directory holding %v, want %v: the claim was not written", got, held)
	}

	taken := pruningStore{Store: releaseStore{}, dir: runtime.SecretsDir(stateDir), log: quietLogger()}
	if _, err := taken.ReleaseHolds(ctx, supervise.Claim{Token: ended}, nil); err != nil {
		t.Fatalf("the release = %v", err)
	}
	if got := secretEntries(t, stateDir); len(got) != 0 {
		t.Fatalf("after a release the store took, the secrets directory holds %v, want nothing of %s: the claim was written with no process", got, ended)
	}
}

// Boot's prune takes every file and gh directory that no claim with a process owns — what a daemon
// killed between clearing a locator and removing its files left — and keeps a live claim's, the
// Dispatch token file, and the provider-env directory.
func TestBootsPruneTakesTheGhDirectoriesOfClaimsWithNoProcess(t *testing.T) {
	stateDir := t.TempDir()
	project, _ := claim.ProjectToken("omp")
	crashed, _ := claim.NewToken(project, "LEGION-1", claim.RoleTester)
	suspended, _ := claim.NewToken(project, "LEGION-3", claim.RoleArchitect)
	live, _ := claim.NewToken(project, "LEGION-2", claim.RoleImplementer)
	for _, token := range []claim.Token{crashed, suspended, live} {
		claimSecrets(t, stateDir, token)
	}
	writeFile(t, filepath.Join(config.ProviderEnvDir(stateDir), "ANTHROPIC_API_KEY"))
	writeFile(t, runtime.SecretFilePath(stateDir, runtime.DispatchTokenFileName))

	pruneAllBut(runtime.SecretsDir(stateDir), []supervise.Claim{
		{Token: suspended},
		{Token: live, Locator: &runtime.Locator{
			Runtime: runtime.RuntimeTmux, Claim: live, Incarnation: "4242:1", Tmux: &runtime.TmuxLocator{Window: "@1", Pane: "%1"},
		}},
	}, quietLogger())

	want := []string{runtime.DispatchTokenFileName, string(live), string(live) + "-gh", string(live) + "-grant", "provider-env"}
	if got := secretEntries(t, stateDir); !slices.Equal(got, want) {
		t.Fatalf("after boot's prune the secrets directory holds %v, want %v", got, want)
	}
	for _, name := range []string{ghconfig.HostsFile, ghconfig.ConfigFile} {
		if _, err := os.Stat(filepath.Join(runtime.GHConfigDir(stateDir, live), name)); err != nil {
			t.Errorf("the live claim's %s: %v", name, err)
		}
	}
	if _, err := os.Stat(filepath.Join(config.ProviderEnvDir(stateDir), "ANTHROPIC_API_KEY")); err != nil {
		t.Errorf("the provider key: %v", err)
	}
}

// Under the tmux runtime a claim's gh directory is one its pane's own user can write, so the prune
// removes it without following a link the pane planted in it, wherever the secrets directory is
// reached through a link: under a state directory that is a link, or as a link itself. A link at
// the gh directory's top to a directory outside the state directory is unlinked, and the file in
// what it names stays. A link under a subdirectory the pane made 0555, which the daemon, holding no
// capability, cannot unlink it from, is left as it is: the prune logs the removal's refusal and
// leaves the gh directory holding that subdirectory alone, while the claim's other files go.
// os.RemoveAll opens such an entry with O_DIRECTORY|O_NOFOLLOW, which a gVisor release follows a
// link through (rolefile.OpenDirectory), so in such a pod it deletes the file the link names;
// Linux refuses that open, so only such a gVisor shows that control fail. Without the secrets
// directory's resolution, the second row's removal is refused ("… is a symlink"), and its gh
// directory keeps every file in it.
func TestThePruneTakesAGhDirectoryWithoutFollowingALinkInIt(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("as root a 0555 directory does not stop the unlink, so the removal never opens the link")
	}
	for _, tc := range []struct {
		name string
		// stateDir is a state directory whose secrets directory is reached through a link.
		stateDir func(t *testing.T) string
	}{
		{"under a state directory that is a link", func(t *testing.T) string {
			stateDir := filepath.Join(t.TempDir(), "state")
			if err := os.Symlink(t.TempDir(), stateDir); err != nil {
				t.Fatal(err)
			}
			return stateDir
		}},
		{"a secrets directory that is itself a link", func(t *testing.T) string {
			stateDir := t.TempDir()
			if err := os.Symlink(t.TempDir(), runtime.SecretsDir(stateDir)); err != nil {
				t.Fatal(err)
			}
			return stateDir
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			outside := t.TempDir()
			precious := filepath.Join(outside, "precious")
			if err := os.WriteFile(precious, []byte("kept\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			stateDir := tc.stateDir(t)
			project, _ := claim.ProjectToken("omp")
			ended, _ := claim.NewToken(project, "LEGION-1", claim.RoleTester)
			claimSecrets(t, stateDir, ended)
			gh := runtime.GHConfigDir(stateDir, ended)
			if err := os.Symlink(outside, filepath.Join(gh, "outside")); err != nil {
				t.Fatal(err)
			}
			locked := filepath.Join(gh, "locked")
			if err := os.Mkdir(locked, 0o755); err != nil {
				t.Fatal(err)
			}
			link := filepath.Join(locked, "link")
			if err := os.Symlink(outside, link); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(locked, 0o555); err != nil {
				t.Fatal(err)
			}
			// The temporary directory's own cleanup could not remove the locked directory at 0555.
			t.Cleanup(func() { _ = os.Chmod(locked, 0o755) })

			var logged bytes.Buffer
			store := pruningStore{dir: runtime.SecretsDir(stateDir), log: bufferedLogger(&logged)}
			store.written(supervise.Claim{Token: ended})

			if body, err := os.ReadFile(precious); err != nil || string(body) != "kept\n" {
				t.Fatalf("%s, in the directory the planted links name, is %q (%v), want it kept", precious, body, err)
			}
			if got, want := secretEntries(t, stateDir), []string{string(ended) + "-gh"}; !slices.Equal(got, want) {
				t.Fatalf("after the claim's write the secrets directory holds %v, want %v: the claim's other files go", got, want)
			}
			if got, want := secretFiles(t, gh), []string{"locked"}; !slices.Equal(got, want) {
				t.Fatalf("after the claim's write its gh directory holds %v, want %v: all it can remove goes", got, want)
			}
			if info, err := os.Lstat(link); err != nil || info.Mode()&os.ModeSymlink == 0 {
				t.Fatalf("the planted link %s is %v (%v), want it left as it was", link, info, err)
			}
			if !strings.Contains(logged.String(), "secrets: remove a pane's gh directory") || !strings.Contains(logged.String(), "permission denied") {
				t.Errorf("the prune logged %q, want the gh directory's removal refused (permission denied)", logged.String())
			}
		})
	}
}
