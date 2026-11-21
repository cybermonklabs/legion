package bus

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/testnats"
)

// A session consumer call made while its connection is down answers ErrConnectionLost at once,
// rather than waiting for the reconnect before it sends anything: its caller holds the session's
// lock (a registration's create, a relay restart, a delete) or the shared stream read, and releases
// it before it waits for the connection (Reconnected). Once the connection is back the call answers.
// The order is what the row decides, not a time: each call answers the loss while its own context
// (ownWait) is still running and the connection is still down, where a call that waited for the
// reconnect would answer only once that context fired, the proxy being restored only after.
//
// The control is the same row at 2b8a5d7cbb: each call waited for the reconnect inside its own
// budget (requestConfirmed's and Pull's ensureConnWithContext), so with the connection still down
// it answered its own deadline's error, with whatever its caller held held all that time.
func TestASessionConsumerCallWhileTheConnectionIsDownAnswersAtOnce(t *testing.T) {
	lossAtOnceRow(t, "ses_loss_at_once", false)
}

// A session consumer call made while its connection is closed - nats.go has closed it and no
// replacement is connected yet, the client still running - answers ErrConnectionLost at once too,
// unsent, rather than dialling the replacement first: its caller holds what the row above names,
// and the dial lasts until a server answers, up to ten attempts a second apart
// (connectWithContext), so it is made by the caller's retry with nothing held (Reconnected, which
// dials a closed connection). Each kind of request the client's calls make under a session's lock
// or a relay's turn has a row: the shared stream read, a consumer create or update, a fenced filter
// write, the list the sweep and the relays read, a pull, and a delete, whose first request is its
// info read. Once the connection is back Reconnected dials the replacement and reports true only
// then, so no retry spins on the closed connection, and the call answers.
//
// The proxy refuses every dial from its cut on, and the connection is closed after the cut, so no
// replacement can connect until the row restores the proxy. The answer each call gives is the one
// connForRequest gives at once, a connectionLost with no cause of nats.go's own; a dial through the
// cut proxy answers its context's deadline, or, after its ten attempts, a connectionLost carrying
// nats.go's ErrNoServers as its cause (lostIfClosed), and either fails the row.
//
// The control is this row with connForRequest's `!conn.IsClosed()` restored, as at b5dc181071:
// each call dials the replacement through the cut proxy, with its caller's lock held, and the
// first, the stream read, answers `context deadline exceeded` when its read's own 5 s bound
// (sessionStreamReadWait) ends.
func TestASessionConsumerCallWhileTheConnectionIsClosedAnswersAtOnce(t *testing.T) {
	lossAtOnceRow(t, "ses_loss_at_once_closed", true)
}

// lossAtOnceRow makes every session consumer call of sessionID while the connection is down, its
// proxy cut and, when closeIt is true, the connection closed after the cut, and requires each to
// answer ErrConnectionLost at once, unsent and with no cause of nats.go's own, while its own
// context runs and the client is not connected. It then restores the proxy and requires
// Reconnected to report the client connected, on a replacement where the connection was closed,
// and the stream read made then to answer.
func lossAtOnceRow(t *testing.T, sessionID string, closeIt bool) {
	t.Helper()
	url := testnats.URL(t)
	owner, err := ConnectOwningStream([]string{url}, WithReplicas(1))
	if err != nil {
		t.Fatalf("connect the stream's owner: %v", err)
	}
	t.Cleanup(owner.Close)
	proxy := testnats.StartProxy(t, url)
	consumers, err := ConnectSessionConsumers([]string{proxy.URL()}, 0, nil)
	if err != nil {
		t.Fatalf("connect the session consumers through the proxy: %v", err)
	}
	t.Cleanup(consumers.Close)
	filter := []string{contracts.AgentSubject(contracts.NoProject, sessionID)}
	ctx, cancel := context.WithTimeout(context.Background(), ownWait(t))
	defer cancel()
	if _, err := consumers.Ensure(ctx, sessionID, filter, 0); err != nil {
		t.Fatalf("create the session's consumer: %v", err)
	}

	down := consumers.client.Connection()
	proxy.Cut()
	for consumers.client.Connected() {
		time.Sleep(time.Millisecond)
	}
	if closeIt {
		consumers.CloseConnection()
		if !down.IsClosed() {
			t.Fatal("the client's connection is not closed after CloseConnection")
		}
	}
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
		callCtx, cancel := context.WithTimeout(context.Background(), ownWait(t))
		started := time.Now()
		err := call.call(callCtx)
		took := time.Since(started)
		fired := callCtx.Err() != nil
		cancel()
		var answered *connectionLost
		atOnce := errors.As(err, &answered) && answered.unsent && answered.cause == nil
		if !errors.Is(err, ErrConnectionLost) || !atOnce || fired || consumers.client.Connected() {
			t.Fatalf("%s made while the connection was down (closed: %t) answered %v after %s (its own deadline fired: %t), want ErrConnectionLost at once, unsent and before any dial, while that deadline ran and the connection was down", call.name, closeIt, err, took, fired)
		}
		t.Logf("%s made while the connection was down (closed: %t) answered %s: %v", call.name, closeIt, took, err)
		lost = err
	}
	if closeIt && consumers.client.Connection() != down {
		t.Fatal("the client replaced its closed connection while the proxy refused every dial")
	}

	proxy.Restore()
	if !Reconnected(ctx, lost) {
		t.Fatal("Reconnected did not report the connection back once the proxy was restored")
	}
	if !consumers.client.Connected() {
		t.Fatal("Reconnected reported the connection back and the client is not connected")
	}
	if closeIt && consumers.client.Connection() == down {
		t.Fatal("Reconnected reported the connection back on the closed connection, with no replacement dialled")
	}
	if id, err := consumers.StreamID(ctx); err != nil || id == "" {
		t.Fatalf("the stream read made once the connection was back answered %q, %v", id, err)
	}
	if _, err := consumers.Delete(ctx, sessionID, nil); err != nil {
		t.Fatalf("the delete made once the connection was back answered %v", err)
	}
	if _, err := consumers.Info(ctx, sessionID); !errors.Is(err, ErrSessionConsumerNotFound) {
		t.Fatalf("the consumer's info after the delete made once the connection was back = %v, want ErrSessionConsumerNotFound", err)
	}
}
