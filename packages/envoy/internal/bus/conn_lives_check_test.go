package bus

import (
	"errors"
	"testing"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/testnats"
)

// A connection life ends as soon as its status listener receives the report of a drop. nats.go can
// drop that report only when the listener's ten-event channel is full. The connection's watch then
// catches the missed report from the reconnect count recorded when the life was first pinned: a
// count past that first pin means a reconnect happened in this life, whatever reached its listener.
//
// The row drives the watch itself (lifeCheckEvery is set past the row) and decides by connection
// events and windows, not elapsed time:
//   - one drop: a listener removed before one drop leaves the old life current until one check ends
//     it; the windows it carried are refused and a fresh window pins;
//   - two drops in one gap: a listener removed before two drops leaves the old life current until
//     one check sees the count past its first pin; the windows it carried are refused and a fresh
//     window pins;
//   - a drop before first registration: two reports happen before the first listener registers;
//     after the reconnect the first pin records that count, and a check keeps it;
//   - a drop then a clean reconnect: after two reports before registration, a normal reported drop
//     ends that old life. A window pinned after its clean reconnect stays current through two checks.
//
// Each control is the first-life bracket at f83e859506: it credits only one of the third and
// fourth rows' two reports before registration. Its check then ends the first pinned life in the
// third, and the clean new life in the fourth. The tests' 30 s waits only guard against NATS not
// changing state.
func TestAConnectionsOwnCheckKeepsALifeAfterACleanReconnect(t *testing.T) {
	every := lifeCheckEvery.Swap(int64(time.Hour))
	t.Cleanup(func() { lifeCheckEvery.Store(every) })

	t.Run("one drop", func(t *testing.T) {
		client, hold := heldHandshakeClient(t)
		conn := client.Connection()
		old := client.WindowUntil(nil).(*window)
		if err := old.pinToSend(); err != nil {
			t.Fatalf("control: pin the old window: %v", err)
		}
		life := lifeOf(conn)
		conn.RemoveStatusListener(life.reports)
		stale := dropAndPin(t, client, conn, hold)
		life.check()
		requireStale(t, old, "the old window after one missed report")
		requireStale(t, stale, "the window pinned before the missed report was found")
		requireFresh(t, client, "one missed report")
	})

	t.Run("two drops in one gap", func(t *testing.T) {
		client, hold := heldHandshakeClient(t)
		conn := client.Connection()
		old := client.WindowUntil(nil).(*window)
		if err := old.pinToSend(); err != nil {
			t.Fatalf("control: pin the old window: %v", err)
		}
		life := lifeOf(conn)
		conn.RemoveStatusListener(life.reports)
		dropAndPin(t, client, conn, hold)
		stale := dropAndPin(t, client, conn, hold)
		life.check()
		requireStale(t, old, "the old window after two missed reports")
		requireStale(t, stale, "the window pinned before two missed reports were found")
		requireFresh(t, client, "two missed reports")
	})

	t.Run("a drop before first registration", func(t *testing.T) {
		client, hold := heldHandshakeClient(t)
		conn := client.Connection()
		life, before := lifeAfterTwoMissedReports(t, conn, hold)
		w := pinAfterReconnect(t, client, conn, before)
		life.check()
		life.check()
		if err := w.pinToSend(); err != nil {
			t.Errorf("the first window pinned after two reports before registration answered %v after the check, want nil", err)
		}
	})

	t.Run("a drop then a clean reconnect", func(t *testing.T) {
		client, hold := heldHandshakeClient(t)
		conn := client.Connection()
		life, before := lifeAfterTwoMissedReports(t, conn, hold)
		old := pinAfterReconnect(t, client, conn, before)
		before = conn.Stats().Reconnects
		hold.Cut()
		hold.Restore()
		waitForLifeEnded(t, old.life)
		fresh := pinAfterReconnect(t, client, conn, before)
		life.check()
		life.check()
		if err := fresh.pinToSend(); err != nil {
			t.Errorf("a window pinned after a clean reconnect answered %v after the checks, want nil", err)
		}
		requireStale(t, old, "the old window after the reported drop")
	})

}

// lifeAfterTwoMissedReports starts a drop, then holds and loses one reconnect handshake before
// life registers its listener. It returns once the second held handshake has been released; both
// RECONNECTING reports happened before the listener could receive them.
func lifeAfterTwoMissedReports(t *testing.T, conn *nats.Conn, hold *testnats.HandshakeHold) (*connLife, uint64) {
	t.Helper()
	before := conn.Stats().Reconnects
	hold.Cut()
	cutUntilReconnecting(t, conn, hold)
	hold.Restore()
	beforeRegister := func() {
		hold.Hold()
		waitForAttempts(t, hold, 2)
		hold.Release()
	}
	lifeOfBeforeRegister.Store(&beforeRegister)
	t.Cleanup(func() { lifeOfBeforeRegister.Store(nil) })
	life := lifeOf(conn)
	return life, before
}

func waitForAttempts(t *testing.T, hold *testnats.HandshakeHold, want int64) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for hold.Attempts() < want {
		if time.Now().After(deadline) {
			t.Fatalf("only %d of %d reconnect handshakes reached the hold", hold.Attempts(), want)
		}
		time.Sleep(time.Millisecond)
	}
}

// dropAndPin cuts conn through hold, lets it reconnect, and returns a window pinned before the
// caller's check notices its listener was removed.
func dropAndPin(t *testing.T, client *Client, conn *nats.Conn, hold *testnats.HandshakeHold) *window {
	t.Helper()
	before := conn.Stats().Reconnects
	hold.Cut()
	cutUntilReconnecting(t, conn, hold)
	hold.Restore()
	return pinAfterReconnect(t, client, conn, before)
}

func requireStale(t *testing.T, w *window, what string) {
	t.Helper()
	if err := w.pinToSend(); !errors.Is(err, ErrUnavailable) {
		t.Errorf("%s answered %v, want ErrUnavailable", what, err)
	}
}

func requireFresh(t *testing.T, client *Client, after string) {
	t.Helper()
	w := client.WindowUntil(nil).(*window)
	if err := w.pinToSend(); err != nil {
		t.Errorf("a fresh window after %s answered %v, want nil", after, err)
	}
}

func waitForLifeEnded(t *testing.T, life <-chan struct{}) {
	t.Helper()
	select {
	case <-life:
	case <-time.After(30 * time.Second):
		t.Fatal("control: nats.go's report of the drop never ended the old life")
	}
}

// cutUntilReconnecting waits, with hold's link cut, until nats.go reads conn as RECONNECTING: it has
// reported the drop.
func cutUntilReconnecting(t *testing.T, conn *nats.Conn, hold *testnats.HandshakeHold) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for conn.Status() != nats.RECONNECTING {
		if time.Now().After(deadline) {
			hold.Restore()
			t.Fatalf("control: nats.go did not read the connection as reconnecting within 30 s of the cut: status %v", conn.Status())
		}
		time.Sleep(time.Millisecond)
	}
}

// pinAfterReconnect waits until nats.go has reconnected conn, with the reconnect counted past before,
// and returns a new window of client whose first send pinned it.
func pinAfterReconnect(t *testing.T, client *Client, conn *nats.Conn, before uint64) *window {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for conn.Status() != nats.CONNECTED || conn.Stats().Reconnects == before {
		if time.Now().After(deadline) {
			t.Fatalf("control: nats.go did not reconnect within 30 s of the cut: status %v, %d reconnect(s)", conn.Status(), conn.Stats().Reconnects-before)
		}
		time.Sleep(10 * time.Millisecond)
	}
	w := client.WindowUntil(nil).(*window)
	if err := w.pinToSend(); err != nil {
		t.Fatalf("control: a window's first send after the reconnect answered %v, want nil", err)
	}
	return w
}
