package bus

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/testnats"
	"github.com/sjawhar/envoy/internal/testwait"
)

// smallMaxPayload is the max payload of the server this file starts: far under the 1 MiB default,
// so the message its row measures is about 64 KiB, the bound plus one byte and the envelope's
// fields. It is kvchunktest.MaxPayload's value, written out because that package imports this one.
const smallMaxPayload = 64 << 10

// The size checks read the max payload the client keeps (Client.maxPayload) and no state of
// nats.go's, so they answer while a reconnect attempt holds nats.go's connection lock across its
// handshake, which nats.Conn.MaxPayload waits for. The client reaches its own server through a
// handshake hold (testnats.HandshakeHold), the hold drops the connection, and the row waits until
// the reconnect's CONNECT has reached the hold and the lock is taken (HoldReconnect). Then
// TooLargeToPublish and a core publish of a message past the bound must each answer ErrTooLarge
// within that attempt (AnswersWithinAttempt): once both have answered the lock must still be taken,
// nats.go must not have let it go since they began, and no later attempt may have reached the hold.
// A check that waited for the lock answers only once the attempt ends at the client's connect
// timeout, so the subtest fails on the lock's state, and the row then fails on the checks by name:
// they waited for nats.go's connection lock. The verdict reads the lock's own state and the hold's
// counts, never how long a call took; AnswersWithinAttempt's one wall-clock bound is its 30 s guard
// against a call that never answers.
func TestTheSizeChecksAnswerWhileAReconnectHoldsTheConnectionLock(t *testing.T) {
	t.Parallel()
	_, server := testnats.StartConfig(t, "max_payload: "+strconv.Itoa(smallMaxPayload)+"\njetstream {}\n", []string{"-js"}, true)
	hold := testnats.StartHandshakeHold(t, server)
	client, err := Connect([]string{hold.URL()}, WithName(t.Name()))
	if err != nil {
		t.Fatalf("connect through the handshake hold: %v", err)
	}
	// nats.go's Close waits for its connection lock, which a held handshake holds, so the hold is
	// released and the proxy restored before the client closes (heldHandshakeClient).
	t.Cleanup(client.Close)
	t.Cleanup(hold.Restore)
	t.Cleanup(hold.Release)
	if maxPayload := client.maxPayload.Load(); maxPayload != smallMaxPayload {
		t.Fatalf("UNKNOWN: the connected client keeps a max payload of %d, want the server's %d", maxPayload, smallMaxPayload)
	}
	const topic = contracts.RoleTopicPrefix + "held-handshake"
	big := sizeEnvelope(topic, strings.Repeat("p", smallMaxPayload+1))
	conn := client.Connection()

	hold.HoldReconnect(t, conn)
	var answers []error
	if !t.Run("within the held attempt", func(t *testing.T) {
		answers = hold.AnswersWithinAttempt(t, conn, "a size check of a message past the max payload",
			func() error { return client.TooLargeToPublish(big) },
			func() error { return client.PublishCoreContext(context.Background(), topic, big) })
	}) {
		t.Fatal("the size checks waited for nats.go's connection lock: they did not both answer within the reconnect attempt that held it, as the subtest above reads")
	}
	for i, err := range answers {
		if !errors.Is(err, ErrTooLarge) {
			t.Errorf("size check %d answered %v while the reconnect held the lock, want ErrTooLarge", i+1, err)
		}
	}
}

// A background start (WithConnectInBackground) installs the connection it dialed without waiting
// for nats.go's connection lock, which a reconnect nats.go makes in the background holds across
// its dial and handshake: the start leaves the size checks' bound unknown, and onConnect reads it
// once the connection has connected. The client dials through a handshake hold already in force,
// so its first attempt is held and fails at its timeout, and nats.go goes on reconnecting in the
// background. The start runs on a goroutine of its own and is held between its dial and its
// install (WithBeforeBackgroundInstall) until the row has seen a later attempt's CONNECT held with
// the lock taken (AwaitAttempt). The row then lets the install run, which must be over within that
// attempt (AnswersWithinAttempt): once the start has returned the lock must still be taken, nats.go
// must not have let it go since the install began, and no later attempt may have reached the hold.
// An install that read the bound would wait out the attempt, and the row would fail there, on the
// start by name. The control is the bound itself: unknown while the hold stands, and the server's
// once the hold is released and the client has connected.
func TestABackgroundStartInstallsWithoutWaitingForAHeldReconnect(t *testing.T) {
	t.Parallel()
	hold := testnats.StartHandshakeHold(t, testnats.URL(t))
	hold.Hold()
	var (
		client *Client
		err    error
	)
	dialed, install, returned := make(chan *nats.Conn, 1), make(chan struct{}), make(chan struct{})
	go func() {
		defer close(returned)
		client, err = Connect([]string{hold.URL()}, WithName(t.Name()), WithConnectInBackground(), WithBeforeBackgroundInstall(func(nc *nats.Conn) {
			dialed <- nc
			<-install
		}))
		// A start that returned without reaching its install's hook dialed nothing to measure. One
		// that reached it has had its connection taken by now, so this send finds room and is read
		// by no one.
		select {
		case dialed <- nil:
		default:
		}
	}()
	conn := testwait.Receive(t, "the background start to reach its install", dialed)
	if conn == nil {
		t.Fatalf("UNKNOWN: the start returned without reaching its install (%v), so no held attempt was measured", err)
	}
	// Registered before the first wait that can end the test (FailNow): until the start returns it
	// is held in its hook, with a connection that reconnects for good (MaxReconnect -1). nats.go's
	// Close waits for its connection lock, which a held handshake holds, so the hold is released and
	// the proxy restored first (heldHandshakeClient); then the start is let go, and the client it
	// returned is closed, or the connection it dialed when it returned none. With the hold released
	// the install waits for nothing, so the wait for the start has no bound of testwait's: a wait
	// that failed at testwait's bound has spent it, and a wait under it would give up at once and
	// leave the connection open.
	var installOnce sync.Once
	letInstall := func() { installOnce.Do(func() { close(install) }) }
	t.Cleanup(func() {
		hold.Release()
		hold.Restore()
		letInstall()
		<-returned
		if client != nil {
			client.Close()
			return
		}
		conn.Close()
	})
	hold.AwaitAttempt(t, conn, hold.Attempts())
	if !t.Run("within the held attempt", func(t *testing.T) {
		hold.AnswersWithinAttempt(t, conn, "the background start's install", func() error {
			letInstall()
			<-returned
			return err
		})
	}) {
		t.Fatal("the background start waited for nats.go's connection lock: its install was not over within the reconnect attempt that held it, as the subtest above reads")
	}
	if err != nil {
		t.Fatalf("connect in the background through the handshake hold: %v", err)
	}
	if bound := client.maxPayload.Load(); bound != 0 {
		t.Errorf("the client keeps a max payload of %d while its first connection has not connected, want none", bound)
	}
	hold.Release()
	testwait.Eventually(t, "control: the client to read the server's max payload once it has connected", func() bool {
		return conn.IsConnected() && client.maxPayload.Load() == conn.MaxPayload()
	})
}
