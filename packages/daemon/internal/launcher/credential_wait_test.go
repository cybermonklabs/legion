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
// context ends first starts nothing and says why, and so does a start whose credential never
// comes, at the bound it names, while the container lives on. A start that names no credential
// starts at once.
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
	credential := &shimwire.LauncherGitHubCredential{HostsFile: "sjawhar/" + ghconfig.HostsFile, TokenDigest: hex.EncodeToString(sum[:]), WaitMs: 60_000}
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

	t.Run("a start whose credential never comes", func(t *testing.T) {
		m := &manager{role: "controller", private: t.TempDir(), requests: map[string]request{}}
		t.Cleanup(m.stopActive)
		// The container lives on, so only the start's own bound can end its wait. The context's
		// end, long after the bound, only lets a start that has no bound leave once this case has
		// failed.
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		bounded := *credential
		bounded.WaitMs = 1000
		done := make(chan shimwire.LauncherStartResult, 1)
		go func() { done <- m.start(ctx, startOf(4, &bounded)) }()
		select {
		case got := <-done:
			if got.OK || !strings.Contains(got.Error, "within its bound of 1s") {
				t.Fatalf("the start = %+v, want the refusal naming its bound of 1s", got)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("the start had not answered 5s after it began, past its bound of 1s: its wait for the slot's gh files ends with the container alone")
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

// A stop of the generation whose start waits for its slot's gh files ends that wait: the launcher
// reads its daemon connection while the start waits, so the stop the daemon sends after a start
// it gave up on is not queued behind the wait for as long as the start's bound runs. The start
// answers its refusal, naming the stop, then the stop finds no child, and no child ever ran. The
// start's bound is a minute, so only the stop can end the wait inside the five seconds the case
// gives the answer.
func TestAStopEndsTheWaitOfItsGenerationsStartForItsCredential(t *testing.T) {
	g := newRig(t)
	marker := filepath.Join(t.TempDir(), "marker")
	start := g.childStart(1, marker, nil)
	start.Env = append(start.Env, "LEGION_GH_OWNERS_DIR="+t.TempDir())
	sum := sha256.Sum256([]byte("ghs_never_written"))
	start.GitHubCredential = &shimwire.LauncherGitHubCredential{HostsFile: "sjawhar/" + ghconfig.HostsFile, TokenDigest: hex.EncodeToString(sum[:]), WaitMs: 60_000}
	g.send(start)
	stop := shimwire.LauncherStop{ID: "stop-1", Generation: 1, GraceMs: 1000}
	g.send(stop)
	answer, err := replyWithin(g.roleConn, 5*time.Second)
	if err != nil {
		t.Fatalf("no answer to the start within 5s of the stop of its generation (%v): the stop is queued behind the start's wait for its credential", err)
	}
	if got, ok := answer.(shimwire.LauncherStartResult); !ok || got.OK || got.ID != start.ID || !strings.Contains(got.Error, "stop of generation 1") {
		t.Fatalf("the first answer = %#v, want the start's refusal naming the stop of generation 1", answer)
	}
	if stopped, ok := g.reply().(shimwire.LauncherStopResult); !ok || !stopped.OK || stopped.ID != stop.ID {
		t.Fatalf("stop result = %#v, want the stop obeyed with no child to end", stopped)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("the stopped start ran its child")
	}
}

// replyWithin is the next answer the launcher sends on c within limit, skipping the frames it
// reports of its own accord, and the read's error where none comes in time.
func replyWithin(c *roleConn, limit time.Duration) (shimwire.Frame, error) {
	deadline := time.Now().Add(limit)
	for {
		if err := c.conn.SetReadDeadline(deadline); err != nil {
			return nil, err
		}
		line, err := c.reader.ReadBytes('\n')
		if err != nil {
			return nil, err
		}
		frame, err := shimwire.Decode(line)
		if err != nil {
			return nil, err
		}
		switch frame.(type) {
		case shimwire.LauncherState, shimwire.LauncherRestored:
			continue
		}
		return frame, nil
	}
}
