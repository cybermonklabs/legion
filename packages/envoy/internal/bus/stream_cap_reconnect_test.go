package bus

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/testnats"
)

// A connection that is reconnecting reports no server version ((*nats.Conn).ConnectedServerVersion
// answers "" outside CONNECTED), and nats.go holds the requests it is given until it is connected
// again. So an ensure that read "" there and decided on it wrote the stream on the server it
// reached with an older server's answer: a deployed stream with no consumer cap, which
// nats-server 2.15 caps at a default of 1000, was left as it is. The reconnect hook's ensure
// (ensureStreamOn) decides only on a version the connection reports, and it looks once rather than
// wait on nats.go's callback goroutine, so on a reconnecting connection it answers an error and
// writes nothing; the recovery runs it again once the connection is back (onReconnect, recover),
// and on the in-process v2.15.1 that run raises the stream to streamMaxConsumers. The control is
// the empty reading itself: serverDefaultsAnUnsetCap of what the reconnecting connection reports
// is false, the older server's answer, on a server that caps such a stream.
func TestAnEnsureHookAcrossAReconnectDecidesNothingOnTheEmptyVersion(t *testing.T) {
	server, uri := testnats.Start(t)
	conn, err := nats.Connect(uri, nats.Name(t.Name()), nats.MaxReconnects(-1), nats.ReconnectWait(50*time.Millisecond))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(conn.Close)
	js, err := conn.JetStream(nats.MaxWait(10 * time.Second))
	if err != nil {
		t.Fatalf("jetstream: %v", err)
	}
	cfg := *streamCfg
	cfg.Replicas = 1
	deployed := cfg
	deployed.MaxConsumers = -1
	deployed.Metadata = nil
	if _, err := js.AddStream(&deployed); err != nil {
		t.Fatalf("add a stream with no consumer cap: %v", err)
	}
	if version := conn.ConnectedServerVersion(); !serverDefaultsAnUnsetCap(version) {
		t.Fatalf("the test server reports %q, which leaves a stream with no cap unlimited: this test needs a server that caps it", version)
	}

	server.Stop(t)
	waitNotConnected(t, conn)
	if version := conn.ConnectedServerVersion(); version != "" || serverDefaultsAnUnsetCap(version) {
		t.Fatalf("a reconnecting connection reports %q (caps an unset stream: %t), want \"\" read as an older server: this test needs the reading a reconnect leaves", version, serverDefaultsAnUnsetCap(version))
	}

	client := &Client{streamConfig: &cfg, stopCh: make(chan struct{})}
	ensured := make(chan error, 1)
	go func() { ensured <- client.ensureStreamOn(conn) }()
	time.Sleep(300 * time.Millisecond)
	server.Start(t)
	select {
	case err := <-ensured:
		if err == nil || !strings.Contains(err.Error(), "not connected") {
			t.Fatalf("the ensure hook on a reconnecting connection answered %v, want an error that the connection is not connected: it decided on, or waited for, a version the connection had not reported", err)
		}
	case <-time.After(60 * time.Second):
		t.Fatal("the ensure hook across a reconnect did not return within 60s")
	}
	waitConnected(t, conn)
	info, err := js.StreamInfo(Stream)
	if err != nil {
		t.Fatalf("read the stream: %v", err)
	}
	if info.Config.MaxConsumers != -1 {
		t.Fatalf("the refused ensure hook wrote the stream's consumer cap to %d, want it untouched at -1", info.Config.MaxConsumers)
	}

	if err := client.ensureStreamOn(conn); err != nil {
		t.Fatalf("the ensure hook once the connection is back: %v", err)
	}
	if info, err = js.StreamInfo(Stream); err != nil {
		t.Fatalf("read the stream: %v", err)
	}
	if info.Config.MaxConsumers != streamMaxConsumers {
		t.Fatalf("the ensure hook on the reconnected v2.15.1 left the stream's consumer cap at %d, want %d", info.Config.MaxConsumers, streamMaxConsumers)
	}
}

// The reconnect hooks run on nats.go's one callback goroutine, and so does every disconnect
// callback, the KV caches' and the client's own among them (AddDisconnectHook). A connection that
// drops again between its reconnect and the stream ensure's read of the server version is a flap,
// and the second disconnect's callback waits for that ensure to return: an ensure that waited for a
// version there would hold every disconnect hook through the whole second outage, and each cache
// would go on answering from a bucket it no longer follows. The ensure looks once, so the second
// disconnect hook runs at once. The flap is made exact through the version read
// (connectedVersion): its first call after the reconnect stops the server and waits until the
// connection is down, then reads the version as the client does, so the read sees the second
// outage every time.
func TestAFlapRunsTheSecondDisconnectHookAtOnce(t *testing.T) {
	server, uri := testnats.Start(t)
	client, err := ConnectOwningStream([]string{uri}, WithName(t.Name()))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(client.Close)
	disconnects := make(chan time.Time, 8)
	client.AddDisconnectHook(func() { disconnects <- time.Now() })

	var armed atomic.Bool
	secondOutage := make(chan time.Time, 1)
	read := connectedVersion
	connectedVersion = func(ctx context.Context, conn *nats.Conn) (string, error) {
		if armed.CompareAndSwap(true, false) {
			server.Stop(t)
			for conn.IsConnected() {
				time.Sleep(5 * time.Millisecond)
			}
			secondOutage <- time.Now()
		}
		return read(ctx, conn)
	}
	t.Cleanup(func() { connectedVersion = read })

	server.Stop(t)
	select {
	case <-disconnects:
	case <-time.After(10 * time.Second):
		t.Fatal("the first disconnect hook did not run within 10s of the server stopping")
	}
	armed.Store(true)
	server.Start(t)
	var down time.Time
	select {
	case down = <-secondOutage:
	case <-time.After(30 * time.Second):
		t.Fatal("the reconnect's ensure did not read the server version within 30s of the restart")
	}
	select {
	case ran := <-disconnects:
		if late := ran.Sub(down); late > 2*time.Second {
			t.Fatalf("the second disconnect hook ran %s after the second outage began, want at once: the ensure hook held nats.go's callback goroutine", late)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the second disconnect hook did not run within 10s of the second outage: the ensure hook held nats.go's callback goroutine waiting for a server version")
	}
	server.Start(t)
}

func waitNotConnected(t *testing.T, conn *nats.Conn) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for conn.IsConnected() {
		if time.Now().After(deadline) {
			t.Fatal("the connection still reports connected 5s after its server stopped")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func waitConnected(t *testing.T, conn *nats.Conn) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for conn.Status() != nats.CONNECTED {
		if time.Now().After(deadline) {
			t.Fatalf("the connection is %v 30s after its server started, want connected", conn.Status())
		}
		time.Sleep(10 * time.Millisecond)
	}
}
