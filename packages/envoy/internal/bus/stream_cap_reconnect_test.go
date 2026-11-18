package bus

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/testnats"
	"github.com/sjawhar/envoy/internal/testwait"
)

// A connection that is reconnecting reports no server version ((*nats.Conn).ConnectedServerVersion
// answers "" outside CONNECTED). A bus connection buffers nothing while it reconnects, so the race
// the version read closes is narrow: an ensure that read "" and then met a reconnect before its
// first request wrote the stream on the server it reached with an older server's answer, and a
// deployed stream with no consumer cap, which nats-server 2.15 caps at a default of 1000, was left
// as it is. This connection keeps nats.go's default reconnect buffer, which holds the ensure's
// requests until the reconnect, to make that window exact. The reconnect hook's ensure
// (ensureStreamOn) decides only on a version the connection reports, and it looks once rather than
// wait out an outage that would keep the next reconnect's run behind it (rewatch), so on a
// reconnecting connection it answers an error and writes nothing; the next run (afterReconnect,
// recover) runs it again once the connection is back, and on the in-process v2.15.1 that run
// raises the stream to streamMaxConsumers. The control is the empty reading itself:
// serverDefaultsAnUnsetCap of what the reconnecting connection reports is false, the older
// server's answer, on a server that caps such a stream.
func TestAnEnsureHookAcrossAReconnectDecidesNothingOnTheEmptyVersion(t *testing.T) {
	server, uri := testnats.Start(t)
	conn, err := nats.Connect(uri, nats.Name(t.Name()), nats.MaxReconnects(-1), nats.ReconnectWait(50*time.Millisecond))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(conn.Close)
	js, err := conn.JetStream(testnats.MaxWait(t))
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
	go func() { ensured <- client.ensureStreamOn(context.Background(), conn) }()
	// The ensure answers while the server is still down: it looks once at the reconnecting
	// connection rather than waiting for a version, so an ensure that waited for one, or held its
	// writes for the reconnect, would never answer here and the wait would say so.
	if err := testwait.Receive(t, "the ensure hook on the reconnecting connection to answer", ensured); err == nil || !strings.Contains(err.Error(), "not connected") {
		t.Fatalf("the ensure hook on a reconnecting connection answered %v, want an error that the connection is not connected: it decided on, or waited for, a version the connection had not reported", err)
	}
	server.Start(t)
	waitConnected(t, conn)
	info, err := js.StreamInfo(Stream)
	if err != nil {
		t.Fatalf("read the stream: %v", err)
	}
	if info.Config.MaxConsumers != -1 {
		t.Fatalf("the refused ensure hook wrote the stream's consumer cap to %d, want it untouched at -1", info.Config.MaxConsumers)
	}

	if err := client.ensureStreamOn(context.Background(), conn); err != nil {
		t.Fatalf("the ensure hook once the connection is back: %v", err)
	}
	if info, err = js.StreamInfo(Stream); err != nil {
		t.Fatalf("read the stream: %v", err)
	}
	if info.Config.MaxConsumers != streamMaxConsumers {
		t.Fatalf("the ensure hook on the reconnected v2.15.1 left the stream's consumer cap at %d, want %d", info.Config.MaxConsumers, streamMaxConsumers)
	}
}

// Every disconnect callback runs on nats.go's one callback goroutine, the KV caches' and the
// client's own among them (AddDisconnectHook), and so does the reconnect callback that starts a run
// of the reconnect hooks. A connection that drops again between its reconnect and the stream
// ensure's read of the server version is a flap: an ensure that waited for a version on that
// goroutine held the second disconnect's callback through the whole second outage. The run of the
// hooks is off that goroutine (onReconnect) and the ensure looks once, so the second disconnect
// hook runs at once. The flap is made exact through the version read (connectedVersion): its first
// call after the reconnect stops the server and waits until the connection is down, then reads the
// version as the client does, so the read sees the second outage every time.
func TestAFlapRunsTheSecondDisconnectHookAtOnce(t *testing.T) {
	server, uri := testnats.Start(t)
	client, err := ConnectOwningStream([]string{uri}, WithName(t.Name()))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(client.Close)
	disconnects := make(chan struct{}, 8)
	client.AddDisconnectHook(func() { disconnects <- struct{}{} })

	// The reconnect's version read, once it has seen the second outage begin, goes on only once
	// the second disconnect hook has run: a read off nats.go's callback goroutine lets that hook
	// run beside it, and one on that goroutine holds it, so the wait for the hook below names the
	// ensure that held it rather than a span of the clock.
	var armed atomic.Bool
	secondOutage := make(chan struct{})
	secondHookRan := make(chan struct{})
	read := connectedVersion
	connectedVersion = func(ctx context.Context, conn *nats.Conn) (string, error) {
		if armed.CompareAndSwap(true, false) {
			lost := conn.StatusChanged(nats.DISCONNECTED, nats.RECONNECTING, nats.CLOSED)
			server.Stop(t)
			if conn.IsConnected() {
				<-lost
			}
			conn.RemoveStatusListener(lost)
			close(secondOutage)
			select {
			case <-secondHookRan:
			case <-t.Context().Done():
			}
		}
		return read(ctx, conn)
	}
	t.Cleanup(func() { connectedVersion = read })

	server.Stop(t)
	testwait.Receive(t, "the first disconnect hook once the server stopped", disconnects)
	armed.Store(true)
	server.Start(t)
	testwait.Receive(t, "the reconnect's ensure to read the server version as the second outage began", secondOutage)
	testwait.Receive(t, "the second disconnect hook while the reconnect's ensure is still reading the version: an ensure on nats.go's callback goroutine holds it there", disconnects)
	close(secondHookRan)
	server.Start(t)
}

func waitNotConnected(t *testing.T, conn *nats.Conn) {
	t.Helper()
	testwait.Eventually(t, "the connection to leave CONNECTED once its server stopped", func() bool { return !conn.IsConnected() })
}

func waitConnected(t *testing.T, conn *nats.Conn) {
	t.Helper()
	testwait.Until(t, "the connection to be CONNECTED once its server started", func() (nats.Status, bool) {
		status := conn.Status()
		return status, status == nats.CONNECTED
	})
}
