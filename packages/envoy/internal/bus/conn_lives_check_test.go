package bus

import (
	"errors"
	"testing"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/testnats"
)

// A connection's own check (connLife.check) does not end the life of a connection that
// reconnected cleanly. nats.go reports RECONNECTING at the drop and counts the reconnect
// (Stats().Reconnects) only once a later attempt's dial succeeds, a reconnect wait (1 s) later at
// the least, so a check can fall between the report and the count. A check that read a count it had
// heard no report of in its own interval as a dropped report ended the life the reconnected
// connection was in at the next check, on a healthy connection: a window pinned after the reconnect
// then answered ErrUnavailable, and the outbox read NATS as lost. A connection first followed
// after nats.go reported a drop — a report no listener of its life could receive — did the same
// once that reconnect was counted. A report that reached the first life twice leaves it one report
// ahead of its reconnect count: the next listener nats.go drops is then hidden, and a window pinned
// before that next drop can send on the reconnected connection.
//
// The row drives the check itself (lifeCheckEvery is set past the row's end, so the watch never
// runs on its own), and every step waits for an event:
//   - a reconnect counted after a check: a window pinned before the drop is refused once nats.go's
//     report has ended its life; a check runs then, with the reconnect not yet counted, which the row
//     reads before and after it; nats.go reconnects, with the reconnect counted; a window pinned then
//     sends; two more checks run; and that window still sends;
//   - a drop before the connection is first followed: the link is cut before any window has pinned
//     the connection, and once nats.go reads RECONNECTING the row follows the connection; the link
//     then comes back, nats.go reconnects with the reconnect counted, a window pinned then sends, two
//     checks run, and that window still sends;
//   - a drop between listener registration and its snapshots: the row cuts the link at that exact
//     point. The listener receives the report. After the first reconnect, it is removed as nats.go
//     would drop a full listener; a second drop follows. Two checks find that report missing, end the
//     life, refuse the windows pinned before the checks, and leave a fresh window current.
//
// The controls: in the first, the reconnect count did not move across the first check, so the check
// ran between the report and the count, and the old window stays refused; in the second, nothing
// followed the connection before the drop; in the third, the listener did receive the first drop
// after registration. The negative controls: the check of this change's earlier 6dc62f5514, which
// reset what it had heard at every check, ends the life the new window pinned in the first; a first
// follow that counts no report it did not hear (884d560ceb) ends it in the second; and this round's
// first-life code, which counts the third's first report twice, leaves its pre-second-drop window
// current. The first two controls make their new window answer ErrUnavailable; the third makes its
// old window answer nil.
func TestAConnectionsOwnCheckKeepsALifeAfterACleanReconnect(t *testing.T) {
	every := lifeCheckEvery.Swap(int64(time.Hour))
	t.Cleanup(func() { lifeCheckEvery.Store(every) })

	t.Run("a reconnect counted after a check", func(t *testing.T) {
		client, hold := heldHandshakeClient(t)
		conn := client.Connection()
		old := client.WindowUntil(nil).(*window)
		if err := old.pinToSend(); err != nil {
			t.Fatalf("control: the old window's first send answered %v, want nil", err)
		}
		life := lifeOf(conn)
		before := conn.Stats().Reconnects

		hold.Cut()
		hold.Restore()
		deadline := time.Now().Add(30 * time.Second)
		for old.current() {
			if time.Now().After(deadline) {
				t.Fatal("control: nats.go's report of the drop never ended the old window's life")
			}
			time.Sleep(time.Millisecond)
		}
		life.check()
		if after := conn.Stats().Reconnects; after != before {
			t.Fatalf("control: nats.go counted the reconnect (%d to %d) before the check between the report and the count ran", before, after)
		}

		w := pinAfterReconnect(t, client, conn, before)
		life.check()
		life.check()
		if err := w.pinToSend(); err != nil {
			t.Errorf("a window pinned after a clean reconnect answered %v after the connection's own checks, want nil: a check ended the life of a healthy connection", err)
		}
		if err := old.pinToSend(); !errors.Is(err, ErrUnavailable) {
			t.Errorf("control: the window pinned before the drop answered %v, want ErrUnavailable", err)
		}
	})

	t.Run("a drop before the connection is first followed", func(t *testing.T) {
		client, hold := heldHandshakeClient(t)
		conn := client.Connection()
		connLives.mu.Lock()
		followed := connLives.of[conn] != nil
		connLives.mu.Unlock()
		if followed {
			t.Fatal("control: the connection was followed before the drop")
		}
		before := conn.Stats().Reconnects

		hold.Cut()
		cutUntilReconnecting(t, conn, hold)
		life := lifeOf(conn)
		hold.Restore()

		w := pinAfterReconnect(t, client, conn, before)
		life.check()
		life.check()
		if err := w.pinToSend(); err != nil {
			t.Errorf("a window pinned after a reconnect whose drop came before the connection was first followed answered %v after the connection's own checks, want nil: a check took the drop's report, made before the follow began, for a dropped listener", err)
		}
	})

	t.Run("a second drop after listener registration", func(t *testing.T) {
		client, hold := heldHandshakeClient(t)
		conn := client.Connection()
		before := conn.Stats().Reconnects
		cutAtRegistration := func() {
			hold.Cut()
			cutUntilReconnecting(t, conn, hold)
		}
		lifeOfAfterListener.Store(&cutAtRegistration)
		t.Cleanup(func() { lifeOfAfterListener.Store(nil) })
		life := lifeOf(conn)
		hold.Restore()

		old := pinAfterReconnect(t, client, conn, before)
		life.check()
		conn.RemoveStatusListener(life.reports)
		before = conn.Stats().Reconnects
		hold.Cut()
		cutUntilReconnecting(t, conn, hold)
		hold.Restore()
		stale := pinAfterReconnect(t, client, conn, before)
		life.check()
		life.check()
		if err := old.pinToSend(); !errors.Is(err, ErrUnavailable) {
			t.Errorf("the window pinned before the second drop answered %v after its listener was dropped, want ErrUnavailable: the first report was counted twice and hid the second drop", err)
		}
		if err := stale.pinToSend(); !errors.Is(err, ErrUnavailable) {
			t.Errorf("the window pinned after the second drop but before its lost listener was found answered %v, want ErrUnavailable", err)
		}
		fresh := client.WindowUntil(nil).(*window)
		if err := fresh.pinToSend(); err != nil {
			t.Errorf("the fresh window after its connection's check found the lost listener answered %v, want nil", err)
		}
	})
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
