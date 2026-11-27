package bus_test

import (
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/bus"
)

// connectedAnswerWithin is how long Connected may take while nats.go holds its connection's lock.
// The answer is the load of one atomic, so this is scheduling alone, and the callers it stands for
// are its readers - /healthz, the keeper's pass and dispatch/outbox's lease checks.
const connectedAnswerWithin = 100 * time.Millisecond

// nats.go holds its connection's lock across every reconnect dial and the connect init that
// follows it, up to the connect timeout each time, so a hub that accepts the connection and never
// answers holds that lock for seconds on end. Connected must answer through it: its readers -
// /healthz, the keeper's pass and dispatch/outbox's lease checks - read it on their own loops, so a
// read that waits for the dial stalls them with it.
func TestConnectedAnswersWhileAReconnectDialHangs(t *testing.T) {
	_, uri := startNATS(t)
	proxy := startRelayProxy(t, uri)
	client, err := bus.ConnectOwningStream([]string{proxy.url()}, bus.WithName(t.Name()))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(client.Close)
	if !client.Connected() {
		t.Fatal("the client reads disconnected over a healthy link")
	}

	// Every connection from here is held before the proxy dials the server, so the reconnect
	// reaches no INFO and hangs with the connection's lock held.
	proxy.delay.Store(int64(time.Minute))
	proxy.drop()
	awaitHeldConnectionLock(t, client.Conn)

	read := time.Now()
	connected := client.Connected()
	if took := time.Since(read); took > connectedAnswerWithin {
		t.Fatalf("Connected took %s while nats.go held the connection's lock, want it within %s", took, connectedAnswerWithin)
	}
	if connected {
		t.Fatal("the client reads connected while its connection is reconnecting")
	}

	// The reconnect that lands is what puts the answer back: a flag the callbacks stopped keeping
	// current would leave the client reading disconnected for the rest of its life.
	proxy.delay.Store(0)
	waitFor(t, "the client to read connected again once its link is back", client.Connected)
}

// awaitHeldConnectionLock returns once a read of conn's state has gone unanswered for
// connectedAnswerWithin, which is nats.go holding the connection's lock across a reconnect dial
// that hangs (doReconnect, processConnectInit). Each read is left on its own goroutine: it
// returns when the lock is let go.
func awaitHeldConnectionLock(t *testing.T, conn *natsgo.Conn) {
	t.Helper()
	for deadline := time.Now().Add(30 * time.Second); time.Now().Before(deadline); {
		done := make(chan struct{})
		go func() {
			defer close(done)
			_ = conn.Status()
		}()
		select {
		case <-done:
			time.Sleep(10 * time.Millisecond)
		case <-time.After(connectedAnswerWithin):
			return
		}
	}
	t.Fatal("nats.go never held the connection's lock across a hanging reconnect dial")
}
