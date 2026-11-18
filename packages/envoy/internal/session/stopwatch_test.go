package session

import (
	"context"
	"log/slog"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/logtest"
	"github.com/sjawhar/envoy/internal/testnats"
	"github.com/sjawhar/envoy/internal/testwait"
)

// openWatchedRegistry opens a session registry whose KV watcher has finished its initial scan.
func openWatchedRegistry(t *testing.T, uri string) *SessionRegistry {
	t.Helper()
	conn := testnats.Connect(t, uri)
	t.Cleanup(conn.Close)
	registry, err := OpenSessionRegistry(conn, WithSessionReplicas(1))
	if err != nil {
		t.Fatalf("open session registry: %v", err)
	}
	if err := registry.WaitForCacheReady(testwait.Context(t)); err != nil {
		t.Fatalf("wait for the session cache: %v", err)
	}
	return registry
}

// StopWatch runs in the listener's shutdown before the NATS drain and outside its deadline, so it
// makes no request of the server: it sends nothing on the connection, which is why it returns with
// the server unresponsive. The count of messages the connection sent, not a span of time, is what
// says so.
func TestStopWatchMakesNoRequestOfTheServer(t *testing.T) {
	_, uri := testnats.Start(t)
	conn := testnats.Connect(t, uri)
	t.Cleanup(conn.Close)
	registry, err := OpenSessionRegistry(conn, WithSessionReplicas(1))
	if err != nil {
		t.Fatalf("open session registry: %v", err)
	}
	if err := registry.WaitForCacheReady(testwait.Context(t)); err != nil {
		t.Fatalf("wait for the session cache: %v", err)
	}

	before := conn.Stats().OutMsgs
	registry.StopWatch()
	if sent := conn.Stats().OutMsgs - before; sent != 0 {
		t.Fatalf("StopWatch sent %d messages on the connection, want none: it makes no request of the server", sent)
	}
}

// StopWatch is final: a Rewatch after it, from a recovery's reconnect hook or a self-health rebuild
// still running at shutdown, arms no watcher, so the drain that follows ends nothing the registry
// still expects and logs no error.
func TestStopWatchIsFinal(t *testing.T) {
	_, uri := testnats.Start(t)
	conn := testnats.Connect(t, uri)
	registry, err := OpenSessionRegistry(conn, WithSessionReplicas(1))
	if err != nil {
		t.Fatalf("open session registry: %v", err)
	}
	if err := registry.WaitForCacheReady(testwait.Context(t)); err != nil {
		t.Fatalf("wait for the session cache: %v", err)
	}

	var logs lockedWriter
	logtest.SetDefault(t, slog.NewTextHandler(&logs, nil))

	registry.StopWatch()
	if err := registry.Rewatch(context.Background(), conn); err != nil {
		t.Fatalf("Rewatch after StopWatch: %v", err)
	}
	if err := conn.Drain(); err != nil {
		t.Fatalf("drain: %v", err)
	}
	// A Rewatch after StopWatch arms no watcher, so once the drain has closed the connection
	// there is nothing left that could end and log: the closed connection is the last event.
	testwait.Eventually(t, "the drain to close the connection", conn.IsClosed)

	if err := registry.WatchErr(); err != nil {
		t.Fatalf("the registry reports a failed watcher after StopWatch: %v", err)
	}
	if strings.Contains(logs.String(), "level=ERROR") {
		t.Fatalf("a watcher armed after StopWatch logged an error when the drain ended it:\n%s", logs.String())
	}
}

type lockedWriter = logtest.Buffer
