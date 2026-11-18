package sandbox

import (
	"bufio"
	"bytes"
	"context"
	"net"
	"testing"
	"testing/synctest"
	"time"

	"github.com/sjawhar/legion/daemon/internal/shimwire"
	"github.com/sjawhar/legion/daemon/internal/testwait"
)

func TestLauncherWithoutInitialStateIsUncertain(t *testing.T) {
	launchers := testLaunchers()
	launchers.accept(workerToken, testTree, rigNow, shimwire.LauncherHello{PodUID: "pod", LauncherID: "launcher"})
	if state, connected := launchers.state(workerToken, "pod"); connected {
		t.Fatalf("launcher with no initial state is connected: %+v", state)
	}
}

func TestLauncherExitBeforeStartResultStaysExited(t *testing.T) {
	ctx := testwait.Context(t)
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
		started <- launchers.start(ctx, workerToken, "pod", shimwire.LauncherStart{ID: "start-7", Generation: 7, Argv: []string{"true"}})
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
	testwait.Receive(t, "the launcher to close the connection an oversized frame with no newline arrived on", done)
}

// The await below runs in a synctest bubble, read once every goroutine of the bubble has blocked
// (synctest.Wait): an await that routed the replacement's launch to the old pod's launcher has
// returned by then, and one that waits for the replacement's own is still waiting.
func TestLauncherFromPreviousPodCannotAnswerForReplacement(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		launchers := testLaunchers()
		old := launchers.accept(workerToken, testTree, rigNow, shimwire.LauncherHello{PodUID: "old-pod", LauncherID: "old-launcher"})
		old.state.Child = &shimwire.LauncherChild{Generation: 7}
		close(old.ready)
		if state, connected := launchers.state(workerToken, "new-pod"); connected {
			t.Fatalf("replacement pod inherited old launcher state: %+v", state)
		}
		type awaited struct {
			session *launcherSession
			err     error
		}
		ctx, cancel := context.WithCancel(context.Background())
		result := make(chan awaited, 1)
		go func() {
			session, err := launchers.await(ctx, workerToken, "new-pod")
			result <- awaited{session, err}
		}()
		synctest.Wait()
		select {
		case got := <-result:
			t.Fatalf("replacement launch was routed to the old pod: session=%p, err=%v", got.session, got.err)
		default:
		}
		cancel()
		if got := <-result; got.err == nil || got.session != nil {
			t.Fatalf("replacement launch was routed to the old pod: session=%p, err=%v", got.session, got.err)
		}
		current := launchers.accept(workerToken, testTree, rigNow.Add(time.Minute), shimwire.LauncherHello{PodUID: "new-pod", LauncherID: "new-launcher"})
		close(current.ready)
		if got, err := launchers.await(context.Background(), workerToken, "new-pod"); err != nil || got != current {
			t.Fatalf("replacement launcher not selected: session=%p, err=%v", got, err)
		}
	})
}

// The write's deadline is the request's context's, which is the input here, so the test runs in a
// synctest bubble whose clock is the test's own: the 20 ms pass on it, and a launcher that never
// reads its command leaves every goroutine blocked with no timer pending, which the bubble fails
// as a deadlock, should the write not end at its deadline.
func TestLauncherCommandWriteHonorsItsDeadline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
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
			done <- launchers.start(ctx, workerToken, "pod", shimwire.LauncherStart{ID: "start-1", Generation: 1, Argv: []string{"true"}})
		}()
		if err := <-done; err == nil {
			t.Fatal("a launcher that reads no commands acknowledged a start")
		}
	})
}
