package bus

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/testnats"
)

// heldHandshakeClient is a client of its own, owning the stream, through a handshake hold of its own
// (testnats.HandshakeHold) on the package's shared server. The hold is released and the proxy
// restored before the client closes, since nats.go's Close waits for its connection lock, which a
// held handshake or a held write holds.
func heldHandshakeClient(t *testing.T) (*Client, *testnats.HandshakeHold) {
	t.Helper()
	hold := testnats.StartHandshakeHold(t, testnats.URL(t))
	client, err := ConnectOwningStream([]string{hold.URL()})
	if err != nil {
		t.Fatalf("connect through the handshake hold: %v", err)
	}
	t.Cleanup(client.Close)
	t.Cleanup(hold.Restore)
	t.Cleanup(hold.Release)
	return client, hold
}

// A window's pin is not held across a reconnect's handshake. While the hub holds its answer to the
// client's CONNECT, nats.go holds its connection lock, and a send of a window that read the
// connection under the window's pin held the pin until the handshake attempt ended: the send that
// held it gave no answer, and every other send of the window waited behind it. Sends of one window
// made at once while the handshake is held each answer ErrUnavailable within
// testnats.HeldHandshakeAnswerWait: two sends and a Probe of a window pinned before the hold, and
// two first sends of a window made during it.
//
// The controls: nats.go's lock is still held when the sends have answered (RequireLockHeld), so
// they answered during the handshake and not after it; and each client pins a window before the
// hold, and once the hold is released and the client has heard of its reconnect, a new window
// sends twice, so the answers are the hold's and not a client that cannot connect. The negative
// control is the pin as it was (ensureConnWithContext, Stats and current() under pinning): neither
// send answers within the wait.
func TestAWindowsPinIsNotHeldAcrossAHeldHandshake(t *testing.T) {
	pinsAgain := func(t *testing.T, client *Client, hold *testnats.HandshakeHold) {
		t.Helper()
		heard := heardReconnects(client)
		hold.Release()
		select {
		case <-heard:
		case <-time.After(30 * time.Second):
			t.Fatal("control: the client did not hear of its reconnect within 30 s of the hold's release")
		}
		w := client.WindowUntil(nil).(*window)
		for send := range 2 {
			if err := w.pinToSend(); err != nil {
				t.Fatalf("control: send %d of a new window, once the client heard of its reconnect, answered %v, want nil", send+1, err)
			}
		}
	}
	unavailable := func(t *testing.T, what string, answers []error) {
		t.Helper()
		for i, err := range answers {
			if !errors.Is(err, ErrUnavailable) {
				t.Errorf("%s %d answered %v while the handshake was held, want ErrUnavailable", what, i+1, err)
			}
		}
	}

	t.Run("pinned before the hold", func(t *testing.T) {
		client, hold := heldHandshakeClient(t)
		w := client.WindowUntil(nil).(*window)
		if err := w.pinToSend(); err != nil {
			t.Fatalf("control: the window's first send could not pin the connection before the hold: %v", err)
		}
		hold.HoldReconnect(t)
		// Each answers at the check's bounded read (eventReadWait), so within half a second on a
		// loaded runner.
		probe := func() error { return w.Probe(time.Second) }
		unavailable(t, "a send or probe of the pinned window", testnats.AnswersWithin(t, 600*time.Millisecond, "a send or probe of the pinned window", w.pinToSend, w.pinToSend, probe))
		testnats.RequireLockHeld(t, w.conn)
		pinsAgain(t, client, hold)
	})

	t.Run("first send during the hold", func(t *testing.T) {
		client, hold := heldHandshakeClient(t)
		if err := client.WindowUntil(nil).(*window).pinToSend(); err != nil {
			t.Fatalf("control: a window could not pin the connection before the hold: %v", err)
		}
		conn := client.Connection()
		hold.HoldReconnect(t)
		// The first send's wait for a connection. Two first sends that waited one behind the other
		// would answer at twice it, past the answer wait below; side by side they answer at it.
		const wait = 400 * time.Millisecond
		w := client.WindowUntil(nil).(*window)
		pin := func() error { return w.pin(wait) }
		unavailable(t, "a first send of the window", testnats.AnswersWithin(t, wait+250*time.Millisecond, "a first send of the window", pin, pin))
		testnats.RequireLockHeld(t, conn)
		pinsAgain(t, client, hold)
	})
}

// heardReconnects returns a channel that takes a value each time client has heard of a reconnect:
// its reconnect hooks run after it has reported the reconnect (onReconnect, ConnectionChanged).
func heardReconnects(client *Client) <-chan struct{} {
	heard := make(chan struct{}, 4)
	client.AddReconnectHook(func(context.Context, *nats.Conn) error {
		select {
		case heard <- struct{}{}:
		default:
		}
		return nil
	})
	return heard
}

// A window pinned after nats.go has reconnected its connection, and before the client has heard of
// that reconnect, keeps its pin once the client hears of it. nats.go marks a reconnected connection
// connected before its callback goroutine runs the client's onReconnect, which is what closes the
// connection-event channel a pin takes, so a check that took any event since the pin for a loss
// refused a good connection in that gap. The row holds the gap open: a disconnect hook, which runs on
// that callback goroutine ahead of the reconnect's callback, waits until the window has pinned.
//
// The controls: nats.go reads the connection connected, with the reconnect counted, before the
// window pins; and the client has heard of the reconnect before the window's second send, so that
// send's check reads the reconnect's event. The negative control is a check that takes any
// connection event since the pin for a loss: the second send answers ErrUnavailable.
func TestAWindowPinnedBeforeTheClientHearsOfAReconnectKeepsIt(t *testing.T) {
	client, hold := heldHandshakeClient(t)
	conn := client.Connection()
	before := conn.Stats().Reconnects
	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	var released sync.Once
	open := func() { released.Do(func() { close(release) }) }
	t.Cleanup(open)
	client.AddDisconnectHook(func() {
		select {
		case entered <- struct{}{}:
		default:
		}
		<-release
	})
	heard := heardReconnects(client)

	hold.Cut()
	hold.Restore()
	select {
	case <-entered:
	case <-time.After(30 * time.Second):
		t.Fatal("control: the client's disconnect hook never ran after the cut")
	}
	deadline := time.Now().Add(30 * time.Second)
	for conn.Status() != nats.CONNECTED || conn.Stats().Reconnects == before {
		if time.Now().After(deadline) {
			t.Fatalf("control: nats.go did not reconnect within 30 s of the cut: status %v, %d reconnect(s)", conn.Status(), conn.Stats().Reconnects-before)
		}
		time.Sleep(10 * time.Millisecond)
	}
	select {
	case <-heard:
		t.Fatal("control: the client heard of the reconnect while its disconnect hook held nats.go's callbacks, so the gap was not held")
	default:
	}
	w := client.WindowUntil(nil).(*window)
	if err := w.pinToSend(); err != nil {
		t.Fatalf("control: the window's first send, after nats.go had reconnected, answered %v, want nil", err)
	}
	open()
	select {
	case <-heard:
	case <-time.After(30 * time.Second):
		t.Fatal("control: the client did not hear of the reconnect within 30 s of the disconnect hook's release")
	}
	if err := w.pinToSend(); err != nil {
		t.Fatalf("the window's next send, once the client heard of the reconnect its pin already counted, answered %v, want nil", err)
	}
}

// A window does not take a connection that stays connected for a lost one while a write holds
// nats.go's connection lock: a slow socket holds it for as long as the write takes, and nothing
// about the connection has changed. A send and a Probe of a window pinned before the write answer
// nil once the link moves again, and the send is stored. The Probe is given the outbox's probe
// budget (outbox's probeTimeout, 2 s), as the batch's probe of a connection whose sends failed is.
//
// The controls: nats.go's lock is held while the send and the Probe wait (RequireLockHeld), and both
// answer once the proxy thaws. The negative control is a pin that reads nats.go's status within
// 100 ms on every send: it takes the held write for a lost connection, and the send and the Probe
// answer ErrUnavailable, which costs the outbox the publisher hold for a connection that never
// dropped.
func TestAWindowDoesNotTakeAHeldWriteForALostConnection(t *testing.T) {
	client, hold := heldHandshakeClient(t)
	w := client.WindowUntil(nil).(*window)
	if err := w.pinToSend(); err != nil {
		t.Fatalf("control: the window's first send could not pin the connection: %v", err)
	}
	conn := client.Connection()
	thaw := hold.HoldWriteLock(t, conn)
	item := contracts.Envelope{
		EventID: fmt.Sprintf("held-write-%d", time.Now().UnixNano()), Source: "dispatch",
		Topic: "notifications.agent.held-write", DedupeKey: fmt.Sprintf("held.write.%d", time.Now().UnixNano()),
	}
	// PublishAsync writes on its caller's goroutine, so the send is made on one of its own.
	sent := make(chan error, 1)
	go func() { sent <- <-w.PublishAsync(item) }()
	probed := make(chan error, 1)
	go func() { probed <- w.Probe(2 * time.Second) }()
	time.Sleep(300 * time.Millisecond)
	testnats.RequireLockHeld(t, conn)
	select {
	case err := <-sent:
		t.Fatalf("the send answered %v while the write held nats.go's lock, want it to wait for the link", err)
	case err := <-probed:
		t.Fatalf("the probe answered %v while the write held nats.go's lock, want it to wait for the link", err)
	default:
	}
	thaw()
	for what, answer := range map[string]<-chan error{"send": sent, "probe": probed} {
		select {
		case err := <-answer:
			if err != nil {
				t.Errorf("the %s of a window whose connection stayed connected under a held write answered %v, want nil", what, err)
			}
		case <-time.After(30 * time.Second):
			t.Fatalf("the %s gave no answer within 30 s of the link moving again", what)
		}
	}
}
