package bus

import (
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/testnats"
	"github.com/sjawhar/envoy/internal/testwait"
)

// A connection life ends as soon as its status listener receives a drop report. nats.go drops a
// report its listener has no room for, so the connection watch compares the reconnect count the
// first window pinned in that life read after CONNECTED. A later count ends the life even when the
// report was missed; removing the listener stands in for that missed report.
//
// The row drives the watch itself (lifeCheckEvery is set past the row) and decides by events:
//   - one missed report: a listener removed before one drop leaves the old life current until one
//     check ends it; the windows it carried are refused and a fresh window pins;
//   - a drop between the life and connected reads: the old life closes before the pin can record it,
//     so the loop takes a fresh life after the reconnect; that fresh window survives two checks;
//   - a clean reconnect: a reported drop ends the old life, and the post-reconnect window survives
//     two checks.
//
// The controls: a pinIf that ignores its ch argument lets a stale life pin, so the second row's
// window is refused. A check that never ends a pinned life fails the first; a >= rather than > in
// endReconnected fails both the second and the third. A pin whose wait for a connection runs on the
// real clock fails the second once its staged reconnect takes longer than that wait
// (publishAcknowledgementTimeout, 5 s); a pin loop that keeps the life it took first, never the
// fresh one after the reconnect, fails the second at its first send's give-up.
//
// Each wait for a fact is testwait's, with no bound of the row's: nats.go's state and a life's end
// through Until and Receive (the second row's hooks through waitInHook), and the first send of a
// window not yet pinned through firstSend, since that send's wait for a connection is on the held
// window clock, which only a pin ends. Each gives up only when NATS does not change state or a pin
// never comes, and names what it waited for.
func TestAConnectionsOwnCheckKeepsALifeAfterACleanReconnect(t *testing.T) {
	every := lifeCheckEvery.Swap(int64(time.Hour))
	t.Cleanup(func() { lifeCheckEvery.Store(every) })

	t.Run("one missed report", func(t *testing.T) {
		client, hold := heldHandshakeClient(t)
		conn := client.Connection()
		old := client.WindowUntil(nil).(*window)
		if err := firstSend(t, old, "control: the old window's first send to pin the connection before the missed report"); err != nil {
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

	t.Run("a drop between life and connected reads", func(t *testing.T) {
		client, hold := heldHandshakeClient(t)
		conn := client.Connection()
		// The hook below holds a reconnect's handshake (Hold) until the pin has read its count; the
		// attempt ends only when the row releases it, with a connect timeout of the test's own. The
		// pin's own wait for a connection is on the client's window clock, a fake the row never moves
		// (heldHandshakeClient), so it does not end inside the staged drop and reconnect, however long
		// they take: only a pin ends it. The row's give-ups are the shared wait's: the first send runs
		// inside it (firstSend), which names the pin should the loop never take a life it can pin, and
		// each hook's wait (waitInHook) writes its give-up at once and hands it to hookFailed.
		testnats.OwnConnectTimeout(t, conn)
		w := client.WindowUntil(nil).(*window)
		original := lifeOf(conn).current()
		// The hooks run on the goroutine pinFirst's Bounded call starts, where no method of t may
		// run: each waits through waitInHook and hands its first error to hookFailed, which the
		// subtest reads once pinToSend has returned. The cleanup closes ended, so a hook still
		// waiting when the subtest ends returns then rather than at the shared wait's end.
		hookFailed := make(chan error, 1)
		ended := make(chan struct{})
		failed := func(err error) {
			select {
			case hookFailed <- err:
			default:
			}
		}
		var once sync.Once
		var held <-chan struct{}
		startDrop := func() {
			once.Do(func() {
				held = hold.Hold()
				if err := waitInHook("control: nats.go's report of the drop to end the old life", original, ended); err != nil {
					failed(err)
				}
			})
		}
		releaseAfterCount := func() {
			if err := waitInHook("the reconnect handshake to reach the hold", held, ended); err != nil {
				failed(err)
				return
			}
			hold.Release()
		}
		w.afterConnected.Store(&startDrop)
		w.pinCountRead.Store(&releaseAfterCount)
		t.Cleanup(func() {
			w.afterConnected.Store(nil)
			w.pinCountRead.Store(nil)
			close(ended)
			hold.Release()
		})
		pinned := firstSend(t, w, "the window's first send to pin the life after the staged drop and reconnect (its wait for a connection is on the held window clock)")
		select {
		case err := <-hookFailed:
			t.Fatalf("a hook of the staged drop failed: %v; the pin answered %v", err, pinned)
		default:
		}
		if pinned != nil {
			t.Fatalf("a window whose life read the drop before connected answered %v, want nil", pinned)
		}
		life := lifeOf(conn)
		life.check()
		life.check()
		if err := w.pinToSend(); err != nil {
			t.Errorf("the window pinned after the reconnect answered %v after the checks, want nil", err)
		}
	})

	t.Run("a clean reconnect", func(t *testing.T) {
		client, hold := heldHandshakeClient(t)
		conn := client.Connection()
		old := client.WindowUntil(nil).(*window)
		if err := firstSend(t, old, "control: the old window's first send to pin the connection before the clean reconnect"); err != nil {
			t.Fatalf("control: pin the old window: %v", err)
		}
		life := lifeOf(conn)
		before := conn.Stats().Reconnects
		oldLife := old.life
		hold.Cut()
		hold.Restore()
		waitForLifeEnded(t, oldLife)
		w := pinAfterReconnect(t, client, conn, before)
		life.check()
		life.check()
		if err := w.pinToSend(); err != nil {
			t.Errorf("a window pinned after a clean reconnect answered %v after the checks, want nil", err)
		}
		requireStale(t, old, "the old window after the reported drop")
	})
}

// The watch's check reads nats.go's reconnect count on a goroutine of its own, and the life that
// count was read against can end before the check acts on it: the drain takes a late report of
// the drop, which ends the old life, and a new window pins the next life on the reconnected
// connection. That window pinned a good connection after the reconnect, so the check keeps it.
//
// The row stages that overlap by events. The listener is removed before the drop, so the old life
// stays current through the reconnect; the check reads a count past the old life's pin; then, in
// the check's read hook, the row ends the old life as the drain does when it takes a report (end),
// and a new window pins the next life; then the check acts.
//
// The controls: when the hook runs, the old life is still current and pinned at a count below the
// one nats.go reports, so the check has a life to end; the new window pinned that connection's
// next life at the count nats.go reports; the old window is refused after the check. The negative
// control is the check as it was, with this row's hook run where it released its lock to put a new
// listener in place of the old: it then ended whatever life was current, and the new window
// answered ErrUnavailable although its connection never dropped after its pin.
func TestAConnectionsCheckKeepsALifePinnedAfterItsRead(t *testing.T) {
	every := lifeCheckEvery.Swap(int64(time.Hour))
	t.Cleanup(func() { lifeCheckEvery.Store(every) })

	client, hold := heldHandshakeClient(t)
	conn := client.Connection()
	old := client.WindowUntil(nil).(*window)
	if err := firstSend(t, old, "control: the old window's first send to pin the connection before the drop"); err != nil {
		t.Fatalf("control: pin the old window: %v", err)
	}
	life := lifeOf(conn)
	conn.RemoveStatusListener(life.reports)
	before := conn.Stats().Reconnects
	hold.Cut()
	cutUntilReconnecting(t, conn)
	hold.Restore()
	waitForReconnect(t, conn, before)

	var fresh *window
	read := func() {
		life.mu.Lock()
		current, pinnedAt := life.life == old.life, life.reconnects
		life.mu.Unlock()
		if !current || conn.Stats().Reconnects <= pinnedAt {
			t.Fatalf("control: the check read no life to end: old life current %v, pinned at %d reconnect(s), nats.go reports %d", current, pinnedAt, conn.Stats().Reconnects)
		}
		life.end(nats.RECONNECTING)
		waitForLifeEnded(t, old.life)
		fresh = client.WindowUntil(nil).(*window)
		if err := firstSend(t, fresh, "control: a new window's first send, in the check's read hook, to pin the connection's next life"); err != nil {
			t.Fatalf("control: a window's first send after the old life ended answered %v, want nil", err)
		}
		life.mu.Lock()
		next, nextPinnedAt := life.life, life.reconnects
		life.mu.Unlock()
		if fresh.conn != conn || fresh.life != next || nextPinnedAt != conn.Stats().Reconnects {
			t.Fatalf("control: the new window did not pin the connection's next life at nats.go's count: same connection %v, current life %v, pinned at %d reconnect(s), nats.go reports %d", fresh.conn == conn, fresh.life == next, nextPinnedAt, conn.Stats().Reconnects)
		}
	}
	life.checkRead.Store(&read)
	t.Cleanup(func() { life.checkRead.Store(nil) })
	if life.check() {
		t.Fatal("the check took the reconnected connection for a closed one")
	}
	if fresh == nil {
		t.Fatal("control: the check never reached its read hook")
	}
	if err := fresh.pinToSend(); err != nil {
		t.Errorf("a window pinned after the check read the count, and before it acted, answered %v, want nil", err)
	}
	requireStale(t, old, "the old window after the check")
}

// A count the watch read past a life's pin, acted on while nats.go's report of the drop still
// waits in the listener, leaves the life to that report. Ending the life at the count as well would
// leave the report, which came before the next life, to end that one too.
//
// The row builds a life by hand, with no connection, since endReconnected and the drain read none:
// it pins the life, puts a report in its listener and acts on a count past the pin.
//
// The control: once the drain runs, it takes the report and ends the pinned life. The negative
// control is an endReconnected that ends the life whatever waits in the listener: the read ends
// the pinned life at once.
func TestAWatchsReadLeavesALifeToAReportTheDrainHasNotTaken(t *testing.T) {
	t.Parallel()
	l := &connLife{reports: make(chan nats.Status, 10), checks: make(chan reconnectRead), quit: make(chan struct{}), life: make(chan struct{})}
	pinned := l.current()
	if !l.pinIf(pinned, 0) {
		t.Fatal("control: pin the hand-built life")
	}
	l.reports <- nats.RECONNECTING
	l.endReconnected(1)
	if l.current() != pinned {
		t.Fatal("a read past the pin ended the life while a report of the drop waited in the listener")
	}
	go l.drain()
	t.Cleanup(func() { l.end(nats.CLOSED) })
	waitForLifeEnded(t, pinned)
}

// dropAndPin cuts conn through hold, lets it reconnect, and returns a window pinned before the
// caller's check notices its listener was removed.
func dropAndPin(t *testing.T, client *Client, conn *nats.Conn, hold *testnats.HandshakeHold) *window {
	t.Helper()
	before := conn.Stats().Reconnects
	hold.Cut()
	cutUntilReconnecting(t, conn)
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
	if err := firstSend(t, w, "the first send of a fresh window after "+after+" to pin the connection"); err != nil {
		t.Errorf("a fresh window after %s answered %v, want nil", after, err)
	}
}

func waitForLifeEnded(t *testing.T, life <-chan struct{}) {
	t.Helper()
	testwait.Receive(t, "control: nats.go's report of the drop to end the old life", life)
}

// waitInHook waits, inside a hook a product goroutine runs (a window's afterConnected or
// pinCountRead, which pinFirst calls on its Bounded call's goroutine), until ch closes or the row
// ends (stop closes): through testwait.ReceiveInHelper, so no method of t runs off the test
// goroutine. It answers the wait's give-up, or that the row ended first, as an error the hook hands
// to the test goroutine. A nil ch never closes.
func waitInHook(what string, ch, stop <-chan struct{}) error {
	// A closed ch answers at once: a pass after the margin must not report a fact it sees as missing.
	select {
	case <-ch:
		return nil
	default:
	}
	either := make(chan struct{})
	go func() {
		defer close(either)
		select {
		case <-ch:
		case <-stop:
		}
	}()
	if _, err := testwait.ReceiveInHelper("bus", what, either); err != nil {
		return err
	}
	select {
	case <-ch:
		return nil
	default:
		return fmt.Errorf("the row ended before %s", what)
	}
}

// cutUntilReconnecting waits, with the link cut, until nats.go reads conn as RECONNECTING: it has
// reported the drop. The row's cleanup restores the link should the wait give up.
func cutUntilReconnecting(t *testing.T, conn *nats.Conn) {
	t.Helper()
	testwait.Until(t, "control: nats.go to read the connection as reconnecting once the link is cut", func() (nats.Status, bool) {
		status := conn.Status()
		return status, status == nats.RECONNECTING
	})
}

// waitForReconnect waits until nats.go has reconnected conn, with the reconnect counted past before,
// bounded only by the test binary's deadline (testwait.Until).
func waitForReconnect(t *testing.T, conn *nats.Conn, before uint64) {
	t.Helper()
	testwait.Until(t, "control: nats.go to reconnect after the cut, with the reconnect counted", func() (string, bool) {
		status, reconnects := conn.Status(), conn.Stats().Reconnects
		return fmt.Sprintf("status %v, %d reconnect(s)", status, reconnects-before), status == nats.CONNECTED && reconnects != before
	})
}

// pinAfterReconnect waits until nats.go has reconnected conn, with the reconnect counted past before,
// and returns a new window of client whose first send pinned it.
func pinAfterReconnect(t *testing.T, client *Client, conn *nats.Conn, before uint64) *window {
	t.Helper()
	waitForReconnect(t, conn, before)
	w := client.WindowUntil(nil).(*window)
	if err := firstSend(t, w, "control: a new window's first send after the reconnect to pin the connection"); err != nil {
		t.Fatalf("control: a window's first send after the reconnect answered %v, want nil", err)
	}
	return w
}
