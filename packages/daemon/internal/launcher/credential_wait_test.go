package launcher

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/ghconfig"
	"github.com/sjawhar/legion/daemon/internal/shimwire"
)

// A start names the credential its slot's gh files must hold, a review App token, while the files
// still hold the implement App's: no child starts until they hold it, and once the kubelet's
// projection writes the review App's hosts.yml the child starts within two seconds. A start whose
// context ends first starts nothing and says why. A start that names no credential starts at once.
func TestALauncherStartsAChildOnlyOnceItsGhFilesHoldTheStartsCredential(t *testing.T) {
	expiry := time.Date(2026, 10, 8, 13, 0, 0, 0, time.UTC)
	root := t.TempDir()
	hosts := filepath.Join(root, "sjawhar", ghconfig.HostsFile)
	write := func(token, app string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(hosts), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(hosts, []byte(ghconfig.Render(token, app, expiry).Hosts), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("ghs_implement", "implement")
	sum := sha256.Sum256([]byte("ghs_review"))
	credential := &shimwire.LauncherGitHubCredential{HostsFile: "sjawhar/" + ghconfig.HostsFile, TokenDigest: hex.EncodeToString(sum[:])}
	marker := filepath.Join(t.TempDir(), "started")
	startOf := func(generation uint64, credential *shimwire.LauncherGitHubCredential) shimwire.LauncherStart {
		return shimwire.LauncherStart{ID: "start-" + string(rune('0'+generation)), Generation: generation,
			Argv: []string{"/bin/sh", "-c", "touch " + marker + "; sleep 30"}, Env: []string{"LEGION_GH_OWNERS_DIR=" + root},
			GitHubCredential: credential}
	}
	started := func() bool { _, err := os.Stat(marker); return err == nil }

	t.Run("a start whose context ends first", func(t *testing.T) {
		m := &manager{role: "controller", private: t.TempDir(), requests: map[string]request{}}
		t.Cleanup(m.stopActive)
		ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
		defer cancel()
		got := m.start(ctx, startOf(1, credential))
		if got.OK || !strings.Contains(got.Error, "the slot's gh files do not hold the credential of this start yet") {
			t.Fatalf("the start = %+v, want the refusal naming the slot's gh files", got)
		}
		if started() || m.child != nil {
			t.Fatal("a child ran on the earlier App's gh files")
		}
	})

	t.Run("the files come to hold the credential", func(t *testing.T) {
		m := &manager{role: "controller", private: t.TempDir(), requests: map[string]request{}}
		t.Cleanup(m.stopActive)
		done := make(chan shimwire.LauncherStartResult, 1)
		go func() { done <- m.start(context.Background(), startOf(2, credential)) }()
		time.Sleep(1500 * time.Millisecond)
		if started() {
			t.Fatal("the child started while the gh files held the implement App's token")
		}
		write("ghs_review", "review")
		select {
		case got := <-done:
			if !got.OK {
				t.Fatalf("the start = %+v, want it started", got)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("the child did not start within two seconds of the gh files holding the credential")
		}
		// The start answers once the child runs; its first command may still be on its way.
		deadline := time.Now().Add(2 * time.Second)
		for !started() && time.Now().Before(deadline) {
			time.Sleep(20 * time.Millisecond)
		}
		if !started() {
			t.Fatal("the start answered and its child never ran")
		}
	})

	t.Run("a start that names no credential", func(t *testing.T) {
		if err := os.Remove(marker); err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
		write("ghs_implement", "implement")
		m := &manager{role: "controller", private: t.TempDir(), requests: map[string]request{}}
		t.Cleanup(m.stopActive)
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if got := m.start(ctx, startOf(3, nil)); !got.OK {
			t.Fatalf("the start with no credential = %+v, want it started at once", got)
		}
	})
}
