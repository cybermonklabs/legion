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
// made at once while the handshake is held each answer ErrUnavailable: two sends and a Probe of a
// window pinned before the hold within 600 ms, since the check waits for nothing, and two
// first sends of a window made during it within 250 ms past their 400 ms wait for a connection,
// which two sends that waited one behind the other would miss.
//
// The controls: nats.go's lock is still held when the sends have answered (RequireLockHeld), so
// they answered during the handshake and not after it; and each client pins a window before the
// hold, and once the hold is released and the client has heard of its reconnect, a new window
// sends twice, so the answers are the hold's and not a client that cannot connect. The negative
// control is the pin as it was (ensureConnWithContext, Stats and current() under pinning): neither
// send answers within its bound.
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
		hold.HoldReconnect(t, w.conn)
		// The check reads nats.go's own report that the life ended, so each answers at once:
		// within 600 ms on a loaded runner.
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
		hold.HoldReconnect(t, conn)
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
// that reconnect, keeps its pin once the client hears of it; and a window pinned before the drop
// refuses the reconnected connection before the client has heard of the drop, before it has heard of
// the reconnect, and after. nats.go marks a reconnected connection connected before its callback
// goroutine runs the client's onDisconnect and onReconnect, and onDisconnect waits for the client's
// lock, which a caller of Client.Connected holds while nats.go's lock keeps it waiting. So a check
// that waited for the client to hear of an event let the old window send on the reconnected
// connection in that gap, and a check that took any client event since the pin for a loss refused
// the new window's good connection once the client heard.
//
// The row holds both gaps open. It holds the client's lock through the drop and the reconnect, as
// that caller would, so the client has heard of neither; then a disconnect hook, which runs on
// nats.go's callback goroutine ahead of the reconnect's callback, holds the client between hearing of
// the drop and hearing of the reconnect. Every step waits for an event: nats.go reconnected with the
// reconnect counted, the hook entered, the client's reconnect hook run.
//
// The controls: nats.go reads the connection connected, with the reconnect counted, before the
// windows are checked; the client has heard of neither event at the first check, of the drop alone
// at the next, and of the reconnect by the last; and a window pinned after the client heard sends.
// The negative controls: a check that reads nothing of nats.go's until the client has heard of an
// event (this change's earlier 0ef5b85b85) lets the old window send before the client hears of the
// drop; a check that takes any client event since the pin for a loss (earlier 184231c372) refuses
// the new window once the client heard; and a check that compares the reconnect count with >= rather
// than == lets the old window send once the client heard.
func TestAWindowPinnedBeforeTheClientHearsOfAReconnectKeepsIt(t *testing.T) {
	client, hold := heldHandshakeClient(t)
	conn := client.Connection()
	before := conn.Stats().Reconnects
	old := client.WindowUntil(nil).(*window)
	if err := old.pinToSend(); err != nil {
		t.Fatalf("control: the old window's first send, before the drop, answered %v, want nil", err)
	}
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

	// The client's lock, held as a caller of Client.Connected holds it while it waits for
	// nats.go's lock. Let go before the client closes, whose Close takes it.
	client.mu.Lock()
	var unlocked sync.Once
	unlock := func() { unlocked.Do(client.mu.Unlock) }
	t.Cleanup(unlock)
	hold.Cut()
	hold.Restore()
	deadline := time.Now().Add(30 * time.Second)
	for conn.Status() != nats.CONNECTED || conn.Stats().Reconnects == before {
		if time.Now().After(deadline) {
			t.Fatalf("control: nats.go did not reconnect within 30 s of the cut: status %v, %d reconnect(s)", conn.Status(), conn.Stats().Reconnects-before)
		}
		time.Sleep(10 * time.Millisecond)
	}
	select {
	case <-entered:
		t.Fatal("control: the client heard of the drop while the row held its lock, so the first gap was not held")
	default:
	}
	if err := old.pinToSend(); !errors.Is(err, ErrUnavailable) {
		t.Errorf("the old window's send on the reconnected connection, before the client heard of the drop, answered %v, want ErrUnavailable", err)
	}
	unlock()
	select {
	case <-entered:
	case <-time.After(30 * time.Second):
		t.Fatal("control: the client's disconnect hook never ran once the row let the client's lock go")
	}
	select {
	case <-heard:
		t.Fatal("control: the client heard of the reconnect while its disconnect hook held nats.go's callbacks, so the second gap was not held")
	default:
	}
	if err := old.pinToSend(); !errors.Is(err, ErrUnavailable) {
		t.Errorf("the old window's send on the reconnected connection, before the client heard of the reconnect, answered %v, want ErrUnavailable", err)
	}
	w := client.WindowUntil(nil).(*window)
	if err := w.pinToSend(); err != nil {
		t.Fatalf("control: the new window's first send, after nats.go had reconnected, answered %v, want nil", err)
	}
	open()
	select {
	case <-heard:
	case <-time.After(30 * time.Second):
		t.Fatal("control: the client did not hear of the reconnect within 30 s of the disconnect hook's release")
	}
	if err := w.pinToSend(); err != nil {
		t.Errorf("the new window's next send, once the client heard of the reconnect its pin already counted, answered %v, want nil", err)
	}
	if err := old.pinToSend(); !errors.Is(err, ErrUnavailable) {
		t.Errorf("the old window's send on the reconnected connection, once the client heard of the reconnect, answered %v, want ErrUnavailable", err)
	}
	if err := client.WindowUntil(nil).(*window).pinToSend(); err != nil {
		t.Errorf("control: a window pinned once the client heard of the reconnect answered %v, want nil", err)
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
