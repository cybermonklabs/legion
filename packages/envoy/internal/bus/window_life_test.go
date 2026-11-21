package bus

import (
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/testnats"
)

// A window made after the client replaced its closed connection pins the new connection's life
// (Client.windowLifeLocked makes a life for each connection the client holds). The closed
// connection's life has ended for good: nats.go's report of the close ends it, and a closed life
// makes no next one (connLife.endLocked). The row waits for that end before the second pin, so it
// decides by the pin's answer alone, and the window pinned on the closed connection stays stale.
//
// The control is windowLifeLocked keeping the first life it made (`if c.windowLife == nil {`): the
// window after the replacement takes the closed connection's life, every pin of it is refused, and
// its first send answers ErrUnavailable at its own bound. pinFirst without its wait for the
// registration can fail this row too, only when the life's listener registers after the close; the
// row below is that control's own.
func TestAWindowAfterTheConnectionIsReplacedPinsTheNewConnectionsLife(t *testing.T) {
	every := lifeCheckEvery.Swap(int64(time.Hour))
	t.Cleanup(func() { lifeCheckEvery.Store(every) })
	client, err := ConnectOwningStream([]string{testnats.URL(t)})
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(client.Close)
	old := client.WindowUntil(nil).(*window)
	if err := old.pinToSend(); err != nil {
		t.Fatalf("control: a window on the first connection answered %v, want nil", err)
	}
	old.pinning.Lock()
	first, oldLife := old.conn, old.life
	old.pinning.Unlock()
	first.Close()
	waitForLifeEnded(t, oldLife)
	w := client.WindowUntil(nil).(*window)
	if err := w.pinToSend(); err != nil {
		t.Fatalf("a window's first send after the client replaced its closed connection answered %v, want nil: it must pin the new connection's life, not the closed one's", err)
	}
	w.pinning.Lock()
	pinned := w.conn
	w.pinning.Unlock()
	if pinned == first || pinned != client.Connection() {
		t.Fatalf("the window after the replacement pinned connection %p, want the client's new connection %p (the closed one was %p)", pinned, client.Connection(), first)
	}
	requireStale(t, old, "the window pinned on the closed connection")
}

// A window pins no life whose status listener is not yet registered: the pin waits for the
// registration (connLife.awaitRegistered) within its own bound. The row holds the registration of
// the life's listener with the client's seam (windowLifeFollowGate) for the whole of one window's
// first send, and reconnects the connection in place (ForceReconnect) while it is held: inside the
// pin, once it has read the connection's reconnect count (pinCountRead), which a pin reaches before
// the registration only when it does not wait for it, and otherwise after that pin has answered.
// So the drop always comes before the registration, and nats.go's report of it reaches no listener
// of the life. It decides by the answers:
//   - the send made while the registration is held answers ErrUnavailable at its own bound, and
//     pins nothing;
//   - once the registration is released, a new window pins the life at the reconnect count after
//     the drop, and reads it current.
//
// The control is pinFirst without its wait for the registration: the pin reads the reconnect count
// before the drop, records it, and answers nil; nothing ends the life the drop left unreported, so
// the window reads it current across the reconnect, until the watch's next check (lifeCheckEvery,
// set past the row).
func TestAWindowDoesNotPinALifeBeforeItsListenerIsRegistered(t *testing.T) {
	every := lifeCheckEvery.Swap(int64(time.Hour))
	t.Cleanup(func() { lifeCheckEvery.Store(every) })
	client, err := ConnectOwningStream([]string{testnats.URL(t)})
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(client.Close)
	conn := client.Connection()
	entered, release := make(chan struct{}), make(chan struct{})
	var enterOnce, releaseOnce sync.Once
	releaseRegistration := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(releaseRegistration)
	client.mu.Lock()
	client.windowLifeFollowGate = func() {
		enterOnce.Do(func() { close(entered) })
		<-release
	}
	client.mu.Unlock()

	before := conn.Stats().Reconnects
	heard := heardReconnects(client)
	within := testnats.Remaining(t)
	var dropOnce sync.Once
	var dropErr error
	drop := func() {
		dropOnce.Do(func() { dropErr = reconnectInPlace(conn, heard, before, within) })
	}
	w := client.WindowUntil(nil).(*window)
	w.pinCountRead.Store(&drop)
	t.Cleanup(func() { w.pinCountRead.Store(nil) })

	held := w.pinToSend()
	select {
	case <-entered:
	case <-time.After(testnats.Remaining(t)):
		t.Fatal("control: the window's life did not reach its listener's registration by the test's deadline")
	}
	if held == nil {
		drop()
		client.mu.Lock()
		life := client.windowLife
		client.mu.Unlock()
		life.mu.Lock()
		pinnedAt := life.reconnects
		life.mu.Unlock()
		t.Fatalf("a window pinned its connection's life while that life's status listener was not registered: it pinned at reconnect count %d, the reconnect that followed reached no listener, and the window reads its life current %v at count %d (drop: %v)",
			pinnedAt, w.current(), conn.Stats().Reconnects, dropErr)
	}
	if !errors.Is(held, ErrUnavailable) {
		t.Fatalf("a window's first send while its life's listener registration was held answered %v, want ErrUnavailable at its own bound", held)
	}

	drop()
	if dropErr != nil {
		t.Fatalf("control: %v", dropErr)
	}
	releaseRegistration()
	life := windowLifeOf(t, client)
	fresh := client.WindowUntil(nil).(*window)
	if err := fresh.pinToSend(); err != nil {
		t.Fatalf("control: a window's first send once the registration was released answered %v, want nil", err)
	}
	life.mu.Lock()
	pinnedAt := life.reconnects
	life.mu.Unlock()
	if now := conn.Stats().Reconnects; pinnedAt != now || pinnedAt == before || !fresh.current() {
		t.Fatalf("the window made after the registration pinned at reconnect count %d and reads its life current %v; want it pinned at the count after the drop, %d (%d before it), and current",
			pinnedAt, fresh.current(), now, before)
	}
}

// reconnectInPlace reconnects conn in place (ForceReconnect) and waits, up to within, until nats.go
// reads it connected at a reconnect count past before, waking at each reconnect the client hears
// (heard). It answers an error rather than failing the test, since a row may call it off the test's
// goroutine.
func reconnectInPlace(conn *nats.Conn, heard <-chan struct{}, before uint64, within time.Duration) error {
	if err := conn.ForceReconnect(); err != nil {
		return fmt.Errorf("force a reconnect: %w", err)
	}
	expired := time.After(within)
	for conn.Stats().Reconnects == before || !conn.IsConnected() {
		select {
		case <-heard:
		case <-expired:
			return fmt.Errorf("the connection was not reconnected by the test's deadline: status %v, %d reconnect(s)", conn.Status(), conn.Stats().Reconnects-before)
		}
	}
	return nil
}
