package bus

import (
	"errors"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/testnats"
)

// heldHandshakeClient is a client of its own through a handshake hold of its own
// (testnats.HandshakeHold) on the package's shared server. The hold is released before the client
// closes, since nats.go's Close waits for its connection lock, which a held handshake holds.
func heldHandshakeClient(t *testing.T) (*Client, *testnats.HandshakeHold) {
	t.Helper()
	hold := testnats.StartHandshakeHold(t, testnats.URL(t))
	client, err := Connect([]string{hold.URL()})
	if err != nil {
		t.Fatalf("connect through the handshake hold: %v", err)
	}
	t.Cleanup(client.Close)
	t.Cleanup(hold.Release)
	return client, hold
}

// A window's pin is not held across a reconnect's handshake. While the hub holds its answer to the
// client's CONNECT, nats.go holds its connection lock, and a send of a window that read the
// connection under the window's pin held the pin until the handshake attempt ended: the send that
// held it gave no answer, and every other send of the window waited behind it. Two sends of one
// window made at once while the handshake is held each answer ErrUnavailable within
// testnats.HeldHandshakeAnswerWait - the one that takes the pin first answers the loss, and the
// other takes the pin and answers too - both for a window pinned before the hold and for one whose
// first send comes during it.
//
// The controls: each window's client pins a window before the hold, and pins a new one once the
// hold is released, so the row's answers are the hold's and not a client that cannot connect. The
// negative control is the pin as it was (ensureConnWithContext, Stats and current() under
// pinning): neither send answers within the wait.
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
		unavailable(t, "a send of the pinned window", testnats.AnswersWhileHeld(t, "a send of the pinned window", w.pinToSend, w.pinToSend))
		pinsAgain(t, client, hold)
	})

	t.Run("first send during the hold", func(t *testing.T) {
		client, hold := heldHandshakeClient(t)
		if err := client.WindowUntil(nil).(*window).pinToSend(); err != nil {
			t.Fatalf("control: a window could not pin the connection before the hold: %v", err)
		}
		hold.HoldReconnect(t)
		// The first send's wait for a connection, well inside the answer wait, so a send that
		// ends its wait on time answers within it.
		const wait = 500 * time.Millisecond
		w := client.WindowUntil(nil).(*window)
		pin := func() error { return w.pin(wait) }
		unavailable(t, "a first send of the window", testnats.AnswersWhileHeld(t, "a first send of the window", pin, pin))
		pinsAgain(t, client, hold)
	})
}
