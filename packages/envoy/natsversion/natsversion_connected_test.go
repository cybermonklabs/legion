package natsversion

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/testnats"
)

// A connection that is reconnecting reports no server version ((*nats.Conn).ConnectedServerVersion
// answers "" while it is not connected), and that is not an unknown server: ConnectedRefusal waits
// for the reconnect and reads the version of the server it reached, so a daemon whose connection
// drops at the moment of the check boots once the server is back rather than ending its boot on
// "a version that cannot be read". The control is a connection that does not reconnect within the
// wait: it is refused for that, never for the version it could not read.
func TestConnectedRefusalWaitsOutAReconnect(t *testing.T) {
	server, uri := testnats.Start(t)
	conn, err := nats.Connect(uri, nats.Name(t.Name()), nats.MaxReconnects(-1), nats.ReconnectWait(50*time.Millisecond))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(conn.Close)

	server.Stop(t)
	waitDisconnected(t, conn)
	if version := conn.ConnectedServerVersion(); version != "" {
		t.Fatalf("a reconnecting connection reports version %q, want none: this test needs the reading the boot met", version)
	}

	t.Run("a reconnect within the wait reads the server it reached", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		// ConnectedRefusal runs off the test goroutine and the server restarts on it, so a
		// failed restart fails the test where t.Fatal may be called.
		answered := make(chan error, 1)
		go func() { answered <- ConnectedRefusal(ctx, conn) }()
		time.Sleep(300 * time.Millisecond)
		server.Start(t)
		if err := <-answered; err != nil {
			t.Fatalf("ConnectedRefusal across a reconnect = %v, want nil for the in-process v2.15.1", err)
		}
	})

	t.Run("no reconnect within the wait is refused as a lost connection, not as a version", func(t *testing.T) {
		server.Stop(t)
		waitDisconnected(t, conn)
		ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
		defer cancel()
		err := ConnectedRefusal(ctx, conn)
		if err == nil {
			t.Fatal("ConnectedRefusal with no server = nil, want a refusal")
		}
		if strings.Contains(err.Error(), "cannot be read") || !strings.Contains(err.Error(), "not connected") {
			t.Fatalf("ConnectedRefusal with no server = %q, want it to say the connection is not connected, not that a version cannot be read", err)
		}
		server.Start(t)
	})
}

// A reconnect that completes between ConnectedRefusal's look at the version and anything else it
// looks at must not reach Refusal(""): the version it read was "", which is no reading of the
// server. The connection here answers "" to its first look at the version and connected to every
// other question, the order nats.go's separate lock acquisitions allow when a reconnect lands
// between them, and the server's version from then on. ConnectedRefusal must decide on a version
// it actually read, so it answers nil for v2.15.1.
func TestConnectedRefusalDecidesOnlyOnAVersionItRead(t *testing.T) {
	reconnecting := &reconnectBetweenReads{version: "2.15.1"}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := connectedRefusal(ctx, reconnecting); err != nil {
		t.Fatalf("connectedRefusal over a reconnect between its reads = %v, want nil for v2.15.1", err)
	}
	if reads := reconnecting.reads(); reads < 2 {
		t.Fatalf("connectedRefusal looked at the version %d time(s), want it to look again after the empty reading", reads)
	}
}

// A connection that is closed is answered at once, with an error that says so and wraps
// nats.ErrConnectionClosed, and ConnectedRefusal's error wraps it too. The Envoy listener's start
// makes this read in its ensure of the stream (internal/bus connectedDefaultsAnUnsetCap) and tries
// a closed connection again (cmd/listener natsConnectionError), which it can only do when the
// error carries nats.go's own; the bare text it carried before ended the start.
func TestAClosedConnectionIsAnsweredWithNATSClosedConnectionError(t *testing.T) {
	_, err := connectedVersion(context.Background(), closedConn{})
	if !errors.Is(err, nats.ErrConnectionClosed) || !strings.Contains(err.Error(), "closed and not connected") {
		t.Fatalf("connectedVersion of a closed connection = %v, want an error that says it is closed and wraps nats.ErrConnectionClosed", err)
	}
	if err := connectedRefusal(context.Background(), closedConn{}); !errors.Is(err, nats.ErrConnectionClosed) {
		t.Fatalf("connectedRefusal of a closed connection = %v, want an error that wraps nats.ErrConnectionClosed", err)
	}
}

// closedConn is a connection that is closed: it reports no server version.
type closedConn struct{}

func (closedConn) ConnectedServerVersion() string { return "" }

func (closedConn) IsClosed() bool { return true }

// A draining connection is connected for nats.go's IsConnected and reports no server version
// (ConnectedServerVersion answers "" outside CONNECTED), so it is no reading of the server either:
// ConnectedRefusal keeps waiting and, when the drain does not end in a reconnect within the wait,
// answers that the connection is not connected, never that a version cannot be read.
func TestConnectedRefusalDoesNotReadADrainingConnectionAsAVersion(t *testing.T) {
	_, uri := testnats.Start(t)
	conn, err := nats.Connect(uri, nats.Name(t.Name()), nats.DrainTimeout(30*time.Second))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(conn.Close)
	release := make(chan struct{})
	handling := make(chan struct{}, 1)
	if _, err := conn.Subscribe("natsversion.drain", func(*nats.Msg) {
		handling <- struct{}{}
		<-release
	}); err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	if err := conn.Publish("natsversion.drain", nil); err != nil {
		t.Fatalf("publish: %v", err)
	}
	select {
	case <-handling:
	case <-time.After(5 * time.Second):
		t.Fatal("the subscription's handler did not start within 5s")
	}
	defer close(release)
	if err := conn.Drain(); err != nil {
		t.Fatalf("drain: %v", err)
	}
	if !conn.IsDraining() || !conn.IsConnected() || conn.ConnectedServerVersion() != "" {
		t.Fatalf("draining %v connected %v version %q, want a draining connection that is connected for IsConnected and reports no version: this test needs that reading",
			conn.IsDraining(), conn.IsConnected(), conn.ConnectedServerVersion())
	}

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	err = ConnectedRefusal(ctx, conn)
	if err == nil {
		t.Fatal("ConnectedRefusal of a draining connection = nil, want a refusal")
	}
	if strings.Contains(err.Error(), "cannot be read") || !strings.Contains(err.Error(), "not connected") {
		t.Fatalf("ConnectedRefusal of a draining connection = %q, want it to say the connection is not connected, not that a version cannot be read", err)
	}
}

// waitDisconnected waits, at most 5s, until conn no longer reports itself connected.
func waitDisconnected(t *testing.T, conn *nats.Conn) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for conn.IsConnected() {
		if time.Now().After(deadline) {
			t.Fatal("the connection still reports connected 5s after its server stopped")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// reconnectBetweenReads is a connection whose reconnect completes just after its first version
// read: that read answers "", and every later answer is a connected one.
type reconnectBetweenReads struct {
	mu      sync.Mutex
	looks   int
	version string
}

func (c *reconnectBetweenReads) ConnectedServerVersion() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.looks++
	if c.looks == 1 {
		return ""
	}
	return c.version
}

func (c *reconnectBetweenReads) IsConnected() bool { return true }

func (c *reconnectBetweenReads) IsClosed() bool { return false }

func (c *reconnectBetweenReads) reads() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.looks
}
