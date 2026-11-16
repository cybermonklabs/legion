package bus

import (
	"testing"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/testnats"
)

// A connection that is reconnecting reports no server version ((*nats.Conn).ConnectedServerVersion
// answers "" outside CONNECTED), and nats.go holds the requests it is given until it is connected
// again. So an ensure that read "" there and decided on it would write the stream on the server
// it reaches with an older server's answer: a deployed stream with no consumer cap, which
// nats-server 2.15 caps at a default of 1000, would be left as it is. The ensure decides on a
// version the connection reports (connectedDefaultsAnUnsetCap, through natsversion's read), so on
// the in-process v2.15.1 it raises that stream to streamMaxConsumers. The control is the empty
// reading itself: serverDefaultsAnUnsetCap of what the reconnecting connection reports is false,
// the older server's answer, on a server that caps such a stream.
func TestAnEnsureAcrossAReconnectDecidesTheCapOnTheServerItReaches(t *testing.T) {
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
	deadline := time.Now().Add(5 * time.Second)
	for conn.IsConnected() {
		if time.Now().After(deadline) {
			t.Fatal("the connection still reports connected 5s after its server stopped")
		}
		time.Sleep(10 * time.Millisecond)
	}
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
		if err != nil {
			t.Fatalf("ensure across a reconnect: %v", err)
		}
	case <-time.After(60 * time.Second):
		t.Fatal("the ensure across a reconnect did not return within 60s")
	}
	info, err := js.StreamInfo(Stream)
	if err != nil {
		t.Fatalf("read the stream: %v", err)
	}
	if info.Config.MaxConsumers != streamMaxConsumers {
		t.Fatalf("an ensure across a reconnect left the stream's consumer cap at %d, want %d: it decided on a version the connection never reported", info.Config.MaxConsumers, streamMaxConsumers)
	}
}
