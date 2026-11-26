package bus

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/testnats"
	"github.com/sjawhar/envoy/internal/testwait"
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
// held it gave no answer, and every other send of the window waited behind it. The row decides by
// events and counts, never by how long a call took:
//   - pinned before the hold: two sends and a Probe of a window pinned before the hold, made at once
//     while a handshake is held, each answer ErrUnavailable within that attempt
//     (HandshakeHold.AnswersWithinAttempt);
//   - first send during the hold: two first sends of a window made during the hold each reach that
//     window's wait for a connection (pinFirst) while neither has answered, so they wait side by side
//     and neither waits behind the other on the pin; once the hold is released, both pin the
//     connection.
//
// The controls: each client pins a window before the hold, and once the hold is released and the
// client has heard of its reconnect, a new window sends twice, so the answers are the hold's and not
// a client that cannot connect. The negative control is the pin as it was (ensureConnWithContext,
// Stats and current() under pinning): its answers come only once the attempt has ended, and its first
// sends do not both reach their wait for a connection while the handshake is held.
func TestAWindowsPinIsNotHeldAcrossAHeldHandshake(t *testing.T) {
	t.Parallel()
	pinsAgain := func(t *testing.T, client *Client, hold *testnats.HandshakeHold) {
		t.Helper()
		heard := heardReconnects(client)
		hold.Release()
		testwait.Receive(t, "control: the client to hear of its reconnect once the hold is released", heard)
		w := client.WindowUntil(nil).(*window)
		for send := range 2 {
			if err := w.pinToSend(); err != nil {
				t.Fatalf("control: send %d of a new window, once the client heard of its reconnect, answered %v, want nil", send+1, err)
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
		probe := func() error { return w.Probe(time.Second) }
		answers := hold.AnswersWithinAttempt(t, w.conn, "a send or probe of the pinned window", w.pinToSend, w.pinToSend, probe)
		for i, err := range answers {
			if !errors.Is(err, ErrUnavailable) {
				t.Errorf("a send or probe of the pinned window %d answered %v while the handshake was held, want ErrUnavailable", i+1, err)
			}
		}
		pinsAgain(t, client, hold)
	})

	t.Run("first send during the hold", func(t *testing.T) {
		client, hold := heldHandshakeClient(t)
		if err := client.WindowUntil(nil).(*window).pinToSend(); err != nil {
			t.Fatalf("control: a window could not pin the connection before the hold: %v", err)
		}
		conn := client.Connection()
		hold.HoldReconnect(t, conn)
		w := client.WindowUntil(nil).(*window)
		var waits atomic.Int64
		count := func() { waits.Add(1) }
		w.connectionWaitEntered.Store(&count)
		answers := make(chan error, 2)
		// No span of its own: each first send waits for a connection until 15 s before the test
		// binary's deadline, and the hold's release gives it one. That outlasts the row's own
		// wait below, which gives up 30 s before the deadline, so a defect run fails there, on
		// the line that names the cause, and not on a pin that ran out first.
		wait := testwait.Remaining(t, 15*time.Second)
		for range 2 {
			go func() { answers <- w.pin(wait) }()
		}
		testwait.UntilPolling(t, time.Millisecond, "both first sends to reach their wait for a connection side by side (the last read is how many had; one that waits behind the other on the window's pin never does)", func() (int64, bool) {
			select {
			case err := <-answers:
				t.Fatalf("a first send answered %v before both first sends had reached their wait for a connection: they did not wait side by side", err)
			default:
			}
			reached := waits.Load()
			return reached, reached >= 2
		})
		select {
		case err := <-answers:
			t.Fatalf("a first send answered %v while the handshake was held, want it still waiting for a connection", err)
		default:
		}
		testnats.RequireLockHeld(t, conn)
		hold.Release()
		for send := range 2 {
			if err := testwait.Receive(t, fmt.Sprintf("first send %d to answer once the hold is released", send+1), answers); err != nil {
				t.Errorf("first send %d answered %v once the hold was released, want nil", send+1, err)
			}
		}
	})
}

// heardReconnects returns a channel that takes a value each time client has heard of a reconnect:
// its reconnect hooks run after it has reported the reconnect (queueReconnect, ConnectionChanged).
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
// goroutine runs the client's onDisconnect and queueReconnect, and onDisconnect waits for the client's
// lock, which a caller of Client.CoreSubOK holds while nats.go's lock keeps it waiting. So a check
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
// event lets the old window send before the client hears of the drop; a check that takes any client
// event since the pin for a loss refuses the new window once the client heard.
func TestAWindowPinnedBeforeTheClientHearsOfAReconnectKeepsIt(t *testing.T) {
	t.Parallel()
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

	// The client's lock, held as a caller of Client.CoreSubOK holds it while it waits for
	// nats.go's lock. Let go before the client closes, whose Close takes it.
	client.mu.Lock()
	var unlocked sync.Once
	unlock := func() { unlocked.Do(client.mu.Unlock) }
	t.Cleanup(unlock)
	hold.Cut()
	hold.Restore()
	testwait.Until(t, "control: nats.go to reconnect after the cut", func() (string, bool) {
		status, reconnects := conn.Status(), conn.Stats().Reconnects
		return fmt.Sprintf("status %v, %d reconnect(s)", status, reconnects-before), status == nats.CONNECTED && reconnects != before
	})
	select {
	case <-entered:
		t.Fatal("control: the client heard of the drop while the row held its lock, so the first gap was not held")
	default:
	}
	if err := old.pinToSend(); !errors.Is(err, ErrUnavailable) {
		t.Errorf("the old window's send on the reconnected connection, before the client heard of the drop, answered %v, want ErrUnavailable", err)
	}
	unlock()
	testwait.Receive(t, "control: the client's disconnect hook to run once the row lets the client's lock go", entered)
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
	testwait.Receive(t, "control: the client to hear of the reconnect once the disconnect hook is released", heard)
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

// Serial: it holds a wall-clock bound close to what it waits for, the outbox's 2 s probe budget
// and the negative control's 100 ms status read, which parallel load pushed past (#5217).
//
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
		if err := testwait.Receive(t, "the "+what+" to answer once the link moves again", answer); err != nil {
			t.Errorf("the %s of a window whose connection stayed connected under a held write answered %v, want nil", what, err)
		}
	}
}
