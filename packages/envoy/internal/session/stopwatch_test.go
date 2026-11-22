package session

import (
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/logtest"
	"github.com/sjawhar/envoy/internal/testnats"
)

// openWatchedRegistry opens a session registry on conn whose KV watcher has finished its initial
// scan.
func openWatchedRegistry(t *testing.T, conn *natsgo.Conn) *SessionRegistry {
	t.Helper()
	registry, err := OpenSessionRegistry(t.Context(), conn, WithSessionReplicas(1))
	if err != nil {
		t.Fatalf("open session registry: %v", err)
	}
	cacheReady(t, registry)
	return registry
}

// StopWatch runs in the listener's shutdown before the NATS drain and outside its deadline, so it
// makes no request of the server: with the server unresponsive it still returns at once. The row
// holds nats.go's connection lock with a write that cannot finish while the server does not
// answer (testnats.HandshakeHold.HoldWriteLock), so a request, any other write StopWatch made, and
// any read of the connection's state (Status, IsConnected) would wait for that lock and never
// return while it is held: StopWatch returning while the lock is still held is the claim, however
// long a loaded machine takes to run it. The connection has no flusher timeout: nats.go gives up a
// write it cannot finish after that timeout, a minute by default, which would let the lock go with
// the server still silent; with none, the held write ends only when the row releases it.
//
// That the lock was held throughout is shown by a read of the connection's status begun before
// StopWatch and still unanswered once StopWatch has returned: a read that answered says the lock
// was free at some point, and the row is then no evidence either way. That guard can fail a
// correct StopWatch on a heavily loaded runner: HoldWriteLock decides the lock is held when a read
// of the status takes longer than its 200 ms probe, which a free lock can on such a runner, and the
// row's own read then finds the lock still free and answers. A read that has not answered is
// waiting for the lock, with one exception the row cannot see: a read whose goroutine has signalled
// but not yet asked for the status when StopWatch returns.
func TestStopWatchMakesNoRequestOfTheServer(t *testing.T) {
	_, uri := testnats.Start(t)
	hold := testnats.StartHandshakeHold(t, uri)
	// The first connection waits for JetStream to answer through the hold (testnats.Connect).
	testnats.Connect(t, hold.URL()).Close()
	conn, err := natsgo.Connect(hold.URL(), natsgo.FlusherTimeout(0))
	if err != nil {
		t.Fatalf("connect through the hold: %v", err)
	}
	t.Cleanup(conn.Close)
	registry := openWatchedRegistry(t, conn)

	release := hold.HoldWriteLock(t, conn)
	reading := make(chan struct{})
	read := make(chan natsgo.Status, 1)
	go func() {
		close(reading)
		read <- conn.Status()
	}()
	waitClosed(t, reading, "the read of the connection's status to begin")
	stopped := make(chan struct{})
	go func() {
		registry.StopWatch()
		close(stopped)
	}()
	waitClosed(t, stopped, "StopWatch to return while the connection's lock is held")
	select {
	case status := <-read:
		t.Fatalf("inconclusive: the read of the connection's status begun before StopWatch answered %v before StopWatch had returned, so the connection's lock was free while StopWatch ran", status)
	default:
	}
	release()
}

// StopWatch is final: a Rewatch after it, from a recovery's reconnect hook or a self-health rebuild
// still running at shutdown, arms no watcher, so the drain that follows ends nothing the registry
// still expects and logs no error.
func TestStopWatchIsFinal(t *testing.T) {
	_, uri := testnats.Start(t)
	conn := testnats.Connect(t, uri)
	registry, err := OpenSessionRegistry(t.Context(), conn, WithSessionReplicas(1))
	if err != nil {
		t.Fatalf("open session registry: %v", err)
	}
	cacheReady(t, registry)

	var logs lockedWriter
	logtest.SetDefault(t, slog.NewTextHandler(&logs, nil))

	registry.StopWatch()
	if err := registry.Rewatch(context.Background(), conn); err != nil {
		t.Fatalf("Rewatch after StopWatch: %v", err)
	}
	if err := conn.Drain(); err != nil {
		t.Fatalf("drain: %v", err)
	}
	waitFor(t, "the drain to close the connection", conn.IsClosed)
	// No event marks the moment after which an ERROR could no longer come: under the claim no
	// watcher exists to log one, and the watcher StopWatch retired, which the same drain ends,
	// reports its end to nothing this package can read. So this settle stays a fixed span; a
	// machine slower than it can only pass the checks below falsely, never fail them.
	time.Sleep(500 * time.Millisecond)

	if err := registry.WatchErr(); err != nil {
		t.Fatalf("the registry reports a failed watcher after StopWatch: %v", err)
	}
	if strings.Contains(logs.String(), "level=ERROR") {
		t.Fatalf("a watcher armed after StopWatch logged an error when the drain ended it:\n%s", logs.String())
	}
}

type lockedWriter = logtest.Buffer
