package bus

import (
	"errors"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
)

// A connection's own check (connLife.check) does not end the life of a connection that
// reconnected cleanly. nats.go reports RECONNECTING at the drop and counts the reconnect
// (Stats().Reconnects) only once a later attempt's dial succeeds, a reconnect wait (1 s) later at
// the least, so a check can fall between the report and the count. A check that read a count it had
// heard no report of in its own interval as a dropped report ended the life the reconnected
// connection was in at the next check, on a healthy connection: a window pinned after the reconnect
// then answered ErrUnavailable, and the outbox read NATS as lost.
//
// The row drives the check itself (lifeCheckEvery is set past the row's end, so the watch never
// runs on its own) at each point of the sequence, and every step waits for an event: a window
// pinned before the drop is refused once nats.go's report has ended its life; a check runs then,
// with the reconnect not yet counted, which the row reads before and after it; nats.go reconnects,
// with the reconnect counted; a window pinned then sends; two more checks run; and that window
// still sends.
//
// The controls: the reconnect count did not move across the first check, so the check ran between
// the report and the count; and the old window stays refused. The negative control is the check of
// this change's earlier 6dc62f5514, which reset what it had heard at every check: it ends the life
// the new window pinned, at the second check after the reconnect, and its send answers
// ErrUnavailable.
func TestAConnectionsOwnCheckKeepsALifeAfterACleanReconnect(t *testing.T) {
	every := lifeCheckEvery.Swap(int64(time.Hour))
	t.Cleanup(func() { lifeCheckEvery.Store(every) })
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

	deadline = time.Now().Add(30 * time.Second)
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
	life.check()
	life.check()
	if err := w.pinToSend(); err != nil {
		t.Errorf("a window pinned after a clean reconnect answered %v after the connection's own checks, want nil: a check ended the life of a healthy connection", err)
	}
	if err := old.pinToSend(); !errors.Is(err, ErrUnavailable) {
		t.Errorf("control: the window pinned before the drop answered %v, want ErrUnavailable", err)
	}
}
