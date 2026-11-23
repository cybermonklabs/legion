package bus

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/testnats"
)

// heldAnswerWait is the timeout a row gives a Drain it makes while a handshake is held
// (testnats.HandshakeHold). No row waits on it for an answer: a row decides by order,
// an answer while the handshake is still held, and bounds the wait only by the test's deadline.
const heldAnswerWait = 10 * time.Second

// proxiedClient is a client of its own through a handshake hold of its own (testnats.HandshakeHold),
// whose dials and handshakes nats.go's own timeout does not end before the test's deadline
// (WithConnectTimeout), so a held handshake lasts until the row releases the hold. The hold is
// released and the proxy restored before the client closes, since nats.go's Close waits for its
// connection lock, which a held handshake holds.
func proxiedClient(t *testing.T, options ...ConnectOption) (*Client, *testnats.HandshakeHold) {
	t.Helper()
	hold := testnats.StartHandshakeHold(t, testnats.URL(t))
	client, err := Connect([]string{hold.URL()}, append([]ConnectOption{WithConnectTimeout(testnats.Remaining(t))}, options...)...)
	if err != nil {
		t.Fatalf("connect through the handshake hold: %v", err)
	}
	t.Cleanup(client.Close)
	t.Cleanup(hold.Restore)
	t.Cleanup(hold.Release)
	return client, hold
}

// waitForNatsStatus waits until nats.go's own status of conn satisfies want, reading it as any
// caller does, under nats.go's lock.
func waitForNatsStatus(t *testing.T, conn *nats.Conn, what string, want func(nats.Status) bool) {
	t.Helper()
	deadline := time.Now().Add(testnats.Remaining(t))
	for !want(conn.Status()) {
		if time.Now().After(deadline) {
			t.Fatalf("nats.go's status of the connection was still %v at the test's deadline, want %s", conn.Status(), what)
		}
		time.Sleep(time.Millisecond)
	}
}

// awaitHeld waits until held, the channel a handshake hold's Hold or HoldNext returns, is closed: a
// client's CONNECT has reached the server through the hold, so its handshake is held.
func awaitHeld(t *testing.T, held <-chan struct{}) {
	t.Helper()
	select {
	case <-held:
	case <-time.After(testnats.Remaining(t)):
		t.Fatal("no reconnect's CONNECT reached the server through the hold by the test's deadline")
	}
}

// endRunningLife ends the life of client's connection that requests are bound to, if one runs, so
// the row's next request begins one.
func endRunningLife(client *Client) {
	client.mu.Lock()
	end := client.takeLifeLocked()
	client.mu.Unlock()
	client.endLifeNow(end)
}

// A request made once nats.go has closed the connection is never sent, though a life of the
// connection a request of the client's start began still runs: every request reads whether the
// connection is connected (requestLife), which nats.go set CLOSED before Close returned, so Request
// answers ErrConnectionLost, unsent, without calling send. The proxy refuses every dial from its
// cut on, so the client's recovery cannot replace the connection before the request.
//
// The control is this row with requestLife taking a running life without reading the connection's
// status: the request is sent, and send is called.
func TestARequestMadeAfterNatsGoClosedTheConnectionIsNeverSent(t *testing.T) {
	client, proxy := proxiedClient(t)
	ctx, cancel := context.WithTimeout(context.Background(), ownWait(t))
	defer cancel()
	if _, err := Request(ctx, client, func(context.Context) (struct{}, error) { return struct{}{}, nil }); err != nil {
		t.Fatalf("the request that begins the life answered %v", err)
	}
	proxy.Cut()
	client.Connection().Close()
	sends := 0
	_, err := Request(ctx, client, func(context.Context) (struct{}, error) {
		sends++
		return struct{}{}, nil
	})
	if !errors.Is(err, ErrConnectionLost) || !requestUnsent(err) || sends != 0 {
		t.Fatalf("a request made after nats.go closed the connection answered %v (unsent: %t) after %d sends, want ErrConnectionLost, unsent, and no send", err, requestUnsent(err), sends)
	}
}

// natsgoClosedAnswer is the answer nats.go gives a request its connection's close or forced
// reconnect cut off (clearPendingRequestCalls closes the request's reply channel).
var natsgoClosedAnswer = nats.ErrConnectionClosed

// A request that a forced reconnect cut off is lost however late its answer is read. The keeper of
// a service's token reconnects with ForceReconnect to present a new token (natscred), which answers
// every pending request nats.ErrConnectionClosed and reconnects in place; a request whose answer is
// read only once the connection is connected again, and before the end of its life reached it,
// reads the connection connected. Its life began at nats.go's reconnect count then (requestLife),
// the forced reconnect counted one more before it reported the connection connected, and
// closedUnder reads the count, so the answer is ErrConnectionLost.
//
// The order is made certain: the gate holds the life's end (withLifeEndGate) until the row has read
// the answer, and the row reads it only once nats.go reads the connection connected again.
//
// The control is this row with closedUnder not comparing the reconnect count: the answer is
// nats.go's "connection closed".
func TestARequestAForcedReconnectCutOffIsLostOnceTheConnectionIsBack(t *testing.T) {
	gate := newLifeEndHold()
	client, err := Connect([]string{testnats.URL(t)}, withLifeEndGate(gate.gate))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(client.Close)
	t.Cleanup(gate.release)
	ctx, cancel := context.WithTimeout(context.Background(), ownWait(t))
	defer cancel()
	_, finish := client.bindRequest(ctx)
	conn := client.Connection()
	if err := conn.ForceReconnect(); err != nil {
		t.Fatalf("force a reconnect: %v", err)
	}
	select {
	case <-gate.held:
	case <-time.After(testnats.Remaining(t)):
		t.Fatal("the forced reconnect never reached the end of the request's life")
	}
	waitForNatsStatus(t, conn, "connected again after the forced reconnect", func(s nats.Status) bool { return s == nats.CONNECTED })
	got := finish(natsgoClosedAnswer)
	gate.release()
	if !errors.Is(got, ErrConnectionLost) {
		t.Fatalf("a request a forced reconnect answered %v, read once the connection was connected again and before its life ended, answered %v, want ErrConnectionLost", natsgoClosedAnswer, got)
	}
}

// The same answer read while the connection is still connected and has not reconnected since the
// request's life began is a handle's of a connection the client replaced, and stays nats.go's: a
// caller reads it as lasting rather than retrying it for ever.
//
// The control is this row with every close answer read as lost: it fails.
func TestACloseAnswerInTheRequestsOwnRunStaysNatsGos(t *testing.T) {
	client, err := Connect([]string{testnats.URL(t)})
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(client.Close)
	ctx, cancel := context.WithTimeout(context.Background(), ownWait(t))
	defer cancel()
	_, finish := client.bindRequest(ctx)
	if got := finish(natsgoClosedAnswer); errors.Is(got, ErrConnectionLost) || !errors.Is(got, nats.ErrConnectionClosed) {
		t.Fatalf("a close answer read with the reconnect count unchanged answered %v, want nats.go's %v", got, natsgoClosedAnswer)
	}
}

// A request through Request on a client whose callers can hold a lock (withLossAtOnce), made while
// the hub holds the handshake of its connection's reconnect, is not sent, and answers
// ErrConnectionLost, unsent, while the handshake is still held: requestLife reads the connection's
// status by the probe, which waits at most ConnLockWait for nats.go's lock, where nats.go's own
// IsConnected waits for the whole handshake.
//
// The control is this row with requestLife reading conn.IsConnected() on such a client: the
// request gives no answer while the handshake is held, and the row fails at the test's deadline.
func TestARequestDuringAHeldHandshakeAnswersAtOnce(t *testing.T) {
	client, hold := proxiedClient(t, withLossAtOnce())
	awaitHeld(t, hold.Hold())
	ctx, cancel := context.WithTimeout(context.Background(), ownWait(t))
	defer cancel()
	var sends atomic.Int64
	answered := make(chan error, 1)
	go func() {
		_, err := Request(ctx, client, func(context.Context) (struct{}, error) {
			sends.Add(1)
			return struct{}{}, nil
		})
		answered <- err
	}()
	var err error
	select {
	case err = <-answered:
	case <-time.After(testnats.Remaining(t)):
		hold.Release()
		t.Fatal("a request made during the held handshake gave no answer by the test's deadline: it waited for nats.go's connection lock, which the handshake holds")
	}
	if !errors.Is(err, ErrConnectionLost) || !requestUnsent(err) || sends.Load() != 0 {
		t.Fatalf("a request made during the held handshake answered %v (unsent: %t) after %d sends, want ErrConnectionLost, unsent, and no send", err, requestUnsent(err), sends.Load())
	}
	select {
	case <-hold.Ended():
		t.Fatal("the request answered only once the held handshake had ended")
	default:
	}
}

// A session consumer call made while the hub holds the handshake of its connection's reconnect
// answers ErrConnectionLost at once, unsent, while the handshake is still held: its caller holds the
// session's lock, the shared stream read or a relay's restart turn, and releases it before it waits
// for the connection (Reconnected). nats.go holds its connection lock through the whole handshake
// (doReconnect, nats.go v1.55.0 :3554-3581), as a hub whose auth service has not answered the
// CONNECT holds it for its authorization timeout; the call reads the connection's status by the
// probe (connForRequest, requestLife), which waits for nats.go's lock at most ConnLockWait. Each
// kind of request the client's calls make under a session's lock or a relay's turn has a row, as in
// lossAtOnceRow. Once the hold is released the handshake ends, Reconnected reports the connection
// back, and the call answers.
//
// The control is this row with connForRequest reading conn.IsConnected(): the first call, the
// stream read, gives no answer while the handshake is held, and the row fails at the test's deadline.
func TestASessionConsumerCallDuringAHeldHandshakeAnswersAtOnce(t *testing.T) {
	const sessionID = "ses_held_handshake"
	url := testnats.URL(t)
	owner, err := ConnectOwningStream([]string{url}, WithReplicas(1))
	if err != nil {
		t.Fatalf("connect the stream's owner: %v", err)
	}
	t.Cleanup(owner.Close)
	hold := testnats.StartHandshakeHold(t, url)
	consumers, err := ConnectSessionConsumers([]string{hold.URL()}, 0, nil, WithConnectTimeout(testnats.Remaining(t)))
	if err != nil {
		t.Fatalf("connect the session consumers through the handshake hold: %v", err)
	}
	t.Cleanup(consumers.Close)
	t.Cleanup(hold.Release)
	filter := []string{contracts.AgentSubject(contracts.NoProject, sessionID)}
	ctx, cancel := context.WithTimeout(context.Background(), ownWait(t))
	defer cancel()
	if _, err := consumers.Ensure(ctx, sessionID, filter, 0); err != nil {
		t.Fatalf("create the session's consumer: %v", err)
	}

	awaitHeld(t, hold.Hold())
	calls := []struct {
		name string
		call func(context.Context) error
	}{
		{"the stream read", func(ctx context.Context) error { _, err := consumers.StreamID(ctx); return err }},
		{"a consumer create or update", func(ctx context.Context) error { _, err := consumers.Ensure(ctx, sessionID, filter, 0); return err }},
		{"a filter write", func(ctx context.Context) error { return consumers.UpdateFilter(ctx, sessionID, filter) }},
		{"a consumer list", func(ctx context.Context) error { _, err := consumers.ListSessionConsumers(ctx); return err }},
		{"a pull", func(ctx context.Context) error {
			pull, err := consumers.Pull(ctx, sessionID)
			if err == nil {
				pull.Stop()
			}
			return err
		}},
		{"a consumer delete", func(ctx context.Context) error { _, err := consumers.Delete(ctx, sessionID, nil); return err }},
	}
	var lost error
	for _, call := range calls {
		answered := make(chan error, 1)
		go func() { answered <- call.call(ctx) }()
		var err error
		select {
		case err = <-answered:
		case <-time.After(testnats.Remaining(t)):
			hold.Release()
			t.Fatalf("%s made during the held handshake gave no answer by the test's deadline: it waited for nats.go's connection lock, which the handshake holds", call.name)
		}
		var answer *connectionLost
		if !errors.As(err, &answer) || !answer.unsent || answer.cause != nil {
			t.Fatalf("%s made during the held handshake answered %v, want ErrConnectionLost at once, unsent", call.name, err)
		}
		select {
		case <-hold.Ended():
			t.Fatalf("%s answered only once the held handshake had ended", call.name)
		default:
		}
		lost = err
	}

	hold.Release()
	if !Reconnected(ctx, lost) {
		t.Fatal("Reconnected did not report the connection back once the hold ended")
	}
	if id, err := consumers.StreamID(ctx); err != nil || id == "" {
		t.Fatalf("the stream read made once the connection was back answered %q, %v", id, err)
	}
}

// A life ends at the drop while nats.go's callback goroutine is held: the life's own status
// listener, registered fresh when the life began, is sent the drop nats.go reports, so a request in
// flight on that life answers ErrConnectionLost at the drop, though the disconnect callback, which
// the row holds, has not run. nats.go queues every callback on one goroutine, so a slow callback
// holds the disconnect's behind it.
//
// The control is this row with the life's own listener taken out of requestLife: nothing ends the
// life at the drop, and the request gives no answer within the row's wait.
func TestALifeEndsAtTheDropWhileNatsGosCallbacksAreHeld(t *testing.T) {
	client, proxy := proxiedClient(t)
	conn := client.Connection()
	release := make(chan struct{})
	var releaseOnce sync.Once
	t.Cleanup(func() { releaseOnce.Do(func() { close(release) }) })
	disconnected := conn.DisconnectErrHandler()
	conn.SetDisconnectErrHandler(func(nc *nats.Conn, err error) {
		<-release
		if disconnected != nil {
			disconnected(nc, err)
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), ownWait(t))
	defer cancel()
	started := make(chan struct{})
	answered := make(chan error, 1)
	go func() {
		_, err := Request(ctx, client, func(bound context.Context) (struct{}, error) {
			close(started)
			<-bound.Done()
			return struct{}{}, context.Cause(bound)
		})
		answered <- err
	}()
	select {
	case <-started:
	case err := <-answered:
		t.Fatalf("the request answered %v before it was sent", err)
	}
	proxy.Cut()
	select {
	case err := <-answered:
		if !errors.Is(err, ErrConnectionLost) {
			t.Fatalf("the request in flight at the drop answered %v, want ErrConnectionLost", err)
		}
	case <-time.After(testnats.Remaining(t)):
		t.Fatal("the request in flight at the drop gave no answer by the test's deadline with nats.go's disconnect callback held: its life did not end at the drop")
	}
}
