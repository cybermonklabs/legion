package bus

import (
	"errors"
	"fmt"
	"testing"
	"time"

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
// hold and a new one once the hold is released, so the answers are the hold's and not a client that
// cannot connect. The negative control is the pin as it was (ensureConnWithContext, Stats and
// current() under pinning): neither send answers within the wait.
func TestAWindowsPinIsNotHeldAcrossAHeldHandshake(t *testing.T) {
	pinsAgain := func(t *testing.T, client *Client, hold *testnats.HandshakeHold) {
		t.Helper()
		hold.Release()
		if err := client.WindowUntil(nil).(*window).pin(30 * time.Second); err != nil {
			t.Fatalf("control: a new window could not pin the connection once the hold was released: %v", err)
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
		probe := func() error { return w.Probe(time.Second) }
		unavailable(t, "a send or probe of the pinned window", testnats.AnswersWhileHeld(t, "a send or probe of the pinned window", w.pinToSend, w.pinToSend, probe))
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
		// The first send's wait for a connection, well inside the answer wait, so a send that
		// ends its wait on time answers within it.
		const wait = 500 * time.Millisecond
		w := client.WindowUntil(nil).(*window)
		pin := func() error { return w.pin(wait) }
		unavailable(t, "a first send of the window", testnats.AnswersWhileHeld(t, "a first send of the window", pin, pin))
		testnats.RequireLockHeld(t, conn)
		pinsAgain(t, client, hold)
	})
}

// A window does not take a connection that stays connected for a lost one while a write holds
// nats.go's connection lock: a slow socket holds it for as long as the write takes, and nothing
// about the connection has changed. A send and a Probe of a window pinned before the write answer
// nil once the link moves again, and the send is stored.
//
// The controls: nats.go's lock is held while the send and the Probe wait (RequireLockHeld), and both
// answer once the proxy thaws. The negative control is a pin that reads the connection's status
// within 100 ms (the pin before this fix's last round): the send and the Probe answer ErrUnavailable,
// which costs the outbox the publisher hold for a connection that never dropped.
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
	go func() { probed <- w.Probe(30 * time.Second) }()
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
