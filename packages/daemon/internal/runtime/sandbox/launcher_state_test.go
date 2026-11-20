package sandbox

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"net"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/leakguard"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/shimwire"
)

func TestLauncherWithoutInitialStateIsUncertain(t *testing.T) {
	launchers := testLaunchers()
	launchers.accept(workerToken, testTree, rigNow, shimwire.LauncherHello{PodUID: "pod", LauncherID: "launcher"})
	if state, connected := launchers.state(workerToken, "pod"); connected {
		t.Fatalf("launcher with no initial state is connected: %+v", state)
	}
}

func TestLauncherExitBeforeStartResultStaysExited(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	launchers := testLaunchers()
	session := launchers.accept(workerToken, testTree, rigNow, shimwire.LauncherHello{PodUID: "pod", LauncherID: "launcher"})
	server, peer := net.Pipe()
	t.Cleanup(func() { _ = server.Close(); _ = peer.Close() })
	go session.ServeLauncher(server, bufio.NewReader(server), shimwire.NewWriter(server))
	writer := shimwire.NewWriter(peer)
	if err := writer.WriteFrame(shimwire.LauncherState{}); err != nil {
		t.Fatal(err)
	}
	started := make(chan error, 1)
	go func() {
		started <- launchers.start(ctx, workerToken, "pod", session, shimwire.LauncherStart{ID: "start-7", Generation: 7, Argv: []string{"true"}})
	}()
	if _, err := shimwire.NewReader(peer).ReadLine(); err != nil {
		t.Fatal(err)
	}
	if err := writer.WriteFrame(shimwire.LauncherState{LastExit: &shimwire.LauncherExit{Generation: 7, Code: 1}}); err != nil {
		t.Fatal(err)
	}
	if err := writer.WriteFrame(shimwire.LauncherStartResult{ID: "start-7", OK: true, RunningGeneration: 7}); err != nil {
		t.Fatal(err)
	}
	if err := <-started; err != nil {
		t.Fatal(err)
	}
	state, connected := launchers.state(workerToken, "pod")
	if !connected || state.Child != nil || state.LastExit == nil || state.LastExit.Generation != 7 {
		t.Fatalf("start acknowledgement resurrected exited generation: connected=%t, state=%+v", connected, state)
	}
}

// A launcher that never connects is the launch's own failure, not a leak-check refusal: the claim
// carries no leakCheckCause, on a deployment that names no check as on one that names one, and the
// error is still the deadline it ran out on, which relaunch's boundRanOut notes as the try's bound.
// No digest or record is prepared before the exact launcher session that will receive the first
// slice exists: a caller that decided one before that session connected fails the configured row.
func TestServeLeakCheckWithoutALauncherIsNoLeakCheckRefusal(t *testing.T) {
	for name, tc := range map[string]struct {
		setting leakguard.Setting
		source  LeakCheckReader
	}{
		"a deployment that names no check": {setting: leakguard.Setting{}},
		"a deployment that names one":      {setting: testLeakCheck, source: servedLeakCheck()},
	} {
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithDeadline(context.Background(), time.Now())
			defer cancel()
			launchers := testLaunchers()
			launchers.leakCheckSetting, launchers.leakCheck = tc.setting, tc.source
			prepared := false
			session, err := launchers.serveLeakCheck(ctx, workerToken, "pod", func(_ *launcherSession, served leakguard.Served) (string, error) {
				prepared = true
				return served.ManifestSHA256, nil
			})
			if session != nil || !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("serveLeakCheck with no launcher = (%p, %v), want no session and the deadline it ran out on", session, err)
			}
			if cause := runtime.LeakCheckCause(err); cause != "" {
				t.Fatalf("serveLeakCheck with no launcher failed with %v carrying the leak-check cause %q, want none", err, cause)
			}
			if prepared {
				t.Fatal("serveLeakCheck prepared a record before a launcher session existed")
			}
		})
	}
}

// A replacement launcher cannot receive a start that was prepared for its predecessor. This is
// the restart control for serveLeakCheck: the predecessor's served checker was either installed
// before it died, or the launch refuses and the next attempt prepares and pushes for the new
// session. The replacement is connected and ready, so a start routed by token rather than by the
// prepared session would reach it; the row fails if it receives any frame.
func TestStartPreparedForAReplacedLauncherSessionRefuses(t *testing.T) {
	launchers := testLaunchers()
	old := launchers.accept(workerToken, testTree, rigNow, shimwire.LauncherHello{PodUID: "pod", LauncherID: "old"})
	replacement := launchers.accept(workerToken, testTree, rigNow, shimwire.LauncherHello{PodUID: "pod", LauncherID: "replacement"})
	if replacement == old {
		t.Fatal("the replacement launcher reused the previous session")
	}
	server, peer := net.Pipe()
	t.Cleanup(func() { _ = server.Close(); _ = peer.Close() })
	go replacement.ServeLauncher(server, bufio.NewReader(server), shimwire.NewWriter(server))
	if err := shimwire.NewWriter(peer).WriteFrame(shimwire.LauncherState{}); err != nil {
		t.Fatal(err)
	}
	received := make(chan []byte, 1)
	go func() {
		line, err := shimwire.NewReader(peer).ReadLine()
		if err == nil {
			received <- line
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := launchers.start(ctx, workerToken, "pod", old,
		shimwire.LauncherStart{ID: "start-1", Generation: 1, Argv: []string{"true"}}); err == nil {
		t.Fatal("a start prepared for a replaced launcher session succeeded")
	}
	select {
	case line := <-received:
		t.Fatalf("the replacement launcher received %q, a start prepared for its predecessor", line)
	case <-time.After(100 * time.Millisecond):
	}
}

func TestLauncherClosesAnOversizedUnterminatedFrame(t *testing.T) {
	launchers := testLaunchers()
	session := launchers.accept(workerToken, testTree, rigNow, shimwire.LauncherHello{PodUID: "pod", LauncherID: "launcher"})
	server, peer := net.Pipe()
	t.Cleanup(func() { _ = server.Close(); _ = peer.Close() })
	done := make(chan struct{})
	go func() {
		defer close(done)
		session.ServeLauncher(server, bufio.NewReader(server), shimwire.NewWriter(server))
	}()
	go func() { _, _ = peer.Write(bytes.Repeat([]byte{'x'}, shimwire.MaxFrameBytes+1)) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("launcher buffered an oversized frame waiting for a newline")
	}
}

func TestLauncherFromPreviousPodCannotAnswerForReplacement(t *testing.T) {
	launchers := testLaunchers()
	old := launchers.accept(workerToken, testTree, rigNow, shimwire.LauncherHello{PodUID: "old-pod", LauncherID: "old-launcher"})
	old.state.Child = &shimwire.LauncherChild{Generation: 7}
	close(old.ready)
	if state, connected := launchers.state(workerToken, "new-pod"); connected {
		t.Fatalf("replacement pod inherited old launcher state: %+v", state)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if got, err := launchers.await(ctx, workerToken, "new-pod"); err == nil || got != nil {
		t.Fatalf("replacement launch was routed to the old pod: session=%p, err=%v", got, err)
	}
	current := launchers.accept(workerToken, testTree, rigNow.Add(time.Minute), shimwire.LauncherHello{PodUID: "new-pod", LauncherID: "new-launcher"})
	close(current.ready)
	if got, err := launchers.await(context.Background(), workerToken, "new-pod"); err != nil || got != current {
		t.Fatalf("replacement launcher not selected: session=%p, err=%v", got, err)
	}
}

func TestLauncherCommandWriteHonorsItsDeadline(t *testing.T) {
	launchers := testLaunchers()
	session := launchers.accept(workerToken, testTree, rigNow, shimwire.LauncherHello{PodUID: "pod", LauncherID: "launcher"})
	server, peer := net.Pipe()
	t.Cleanup(func() { _ = server.Close(); _ = peer.Close() })
	go session.ServeLauncher(server, bufio.NewReader(server), shimwire.NewWriter(server))
	if err := shimwire.NewWriter(peer).WriteFrame(shimwire.LauncherState{}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- launchers.start(ctx, workerToken, "pod", session, shimwire.LauncherStart{ID: "start-1", Generation: 1, Argv: []string{"true"}})
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("a launcher that reads no commands acknowledged a start")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a launcher that does not read its command held the daemon past the request deadline")
	}
}
