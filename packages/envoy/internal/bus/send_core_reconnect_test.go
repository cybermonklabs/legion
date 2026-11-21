package bus

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/testnats"
	"github.com/sjawhar/envoy/internal/testwait"
)

// A send of the core subscription that a reconnect overtakes gives no verdict. sendCore compares
// the connection's last error before the bind with the one after the flush, and nats.go sets that
// error anew within a reconnect: nil as the reconnect begins (doReconnect), a failed attempt's error
// on each failed attempt, and nil again once an attempt connects (processConnectInit). A verdict
// made across that compares two sessions: a refusal before and nil after counted a subscription the
// new server accepted as refused, and ResendRefusedCore then tried the machine's group and dropped
// the subscription the client held.
//
// Here the server stops and the client starts to reconnect. The relay then freezes and the server
// starts again, granting the subscription: an attempt can connect to the relay but its handshake
// cannot finish. sendCore runs now: nats.go keeps the subscription for the reconnect and puts the
// flush's PING in its reconnect buffer. The relay thaws once that PING is buffered, the reconnect
// finishes, nats.go sends the subscription to the server, which accepts it, and the PING's PONG
// ends the flush. sendCore must answer with an error, not a verdict made across the two sessions.
//
// This enters sendCore on a connection already reconnecting. The order ResendRefusedCore reaches it
// in, connected at the check and disconnected between the error's read and the flush, cannot be
// placed: nothing in the client stops a send between those two reads, and a disconnect during the
// flush's wait instead ends the flush with nats.ErrConnectionClosed (nats.go clears every waiting
// flush as it disconnects), which sendCore already returns.
func TestSendCoreGivesNoVerdictForASendAReconnectOvertook(t *testing.T) {
	const (
		subject = "notifications.>"
		queue   = "send-core-reconnect"
	)
	seed, public := testnats.User(t)
	refusing := testnats.SubscribeGrantConfig(public, "_INBOX.>")
	granting := testnats.SubscribeGrantConfig(public, subject, "_INBOX.>")
	server, uri := testnats.StartConfig(t, refusing, []string{"-DV", "-js"}, false)
	t.Setenv("NATS_NKEY_SEED", seed)
	client, err := Connect([]string{uri})
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(client.Close)
	if _, err := client.SubscribeCore(subject, func(*nats.Msg) {}, queue); err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	conn := client.Conn
	if err := conn.Flush(); err != nil {
		t.Fatalf("flush the subscription: %v", err)
	}

	server.Stop(t)
	testwait.WithinPolling(t, 30*time.Second, 10*time.Millisecond, "the connection to read the stop", func() bool {
		return conn.Status() == nats.RECONNECTING
	})
	thaw := server.Freeze(t)
	t.Cleanup(thaw)
	server.Restart(t, granting)
	stale := conn.LastError()
	if conn.Status() != nats.RECONNECTING {
		t.Fatalf("the connection reads %v before the send; the test proves nothing", conn.Status())
	}
	reconnects := conn.Stats().Reconnects

	type answer struct {
		sub      *nats.Subscription
		accepted bool
		err      error
	}
	answered := make(chan answer, 1)
	go func() {
		sub, accepted, err := sendCore(recoverableSubscription{transport: coreSubscription, subject: subject, queue: queue, handler: func(*nats.Msg) {}}, conn, client.js)
		answered <- answer{sub, accepted, err}
	}()
	// The PING is the one thing a reconnecting connection buffers here: the bind's SUB waits for
	// the reconnect, which sends every subscription again. A send that ended before it bound
	// anything buffers nothing, so the wait also ends when the answer arrives.
	var got answer
	pinged, done := false, false
	testwait.WithinPolling(t, 10*time.Second, time.Millisecond, "the flush's PING in the reconnect buffer, or the send's answer", func() bool {
		select {
		case got = <-answered:
			done = true
			return true
		default:
		}
		buffered, err := conn.Buffered()
		pinged = err == nil && buffered > 0
		return pinged
	})
	if pinged && conn.Status() != nats.RECONNECTING {
		t.Fatalf("the connection reads %v once the PING is buffered; the test proves nothing", conn.Status())
	}
	thaw()
	if !done {
		select {
		case got = <-answered:
		case <-time.After(10 * time.Second):
			t.Fatal("sendCore gave no answer")
		}
	}
	testwait.WithinPolling(t, 30*time.Second, 10*time.Millisecond, "the connection to reconnect", func() bool {
		return conn.Status() == nats.CONNECTED && conn.Stats().Reconnects > reconnects
	})
	if errors.Is(got.err, nats.ErrTimeout) {
		t.Fatalf("the flush timed out, so no answer was read; the test proves nothing: %v", got.err)
	}

	// The restarted server accepted every subscription it was sent. nats.go reads CONNECTED before
	// it sends the subscriptions again, under the connection's lock, so this flush's PING follows
	// them and its PONG comes once the server has read, and traced, every one.
	if err := conn.Flush(); err != nil {
		t.Fatalf("flush the reconnected connection: %v", err)
	}
	trace := server.Log()
	trace = trace[strings.LastIndex(trace, "Starting nats-server"):]
	if sent := strings.Count(trace, "<<- [SUB "+subject+" "+queue+" "); sent == 0 {
		t.Fatalf("the restarted server received no subscription to %s in %s; the test proves nothing", subject, queue)
	}
	if refused := strings.Count(trace, "Permissions Violation for Subscription"); refused != 0 {
		t.Fatalf("the restarted server refused %d subscriptions; the test proves nothing", refused)
	}

	if got.err == nil {
		_ = got.sub.Unsubscribe()
		t.Fatalf("sendCore answered accepted=%v for a send its connection's reconnect overtook (last error %v as the send began, %v after it); want an error and no verdict", got.accepted, stale, conn.LastError())
	}
	if got.sub != nil {
		t.Fatalf("sendCore returned a subscription beside its error %v", got.err)
	}
}
