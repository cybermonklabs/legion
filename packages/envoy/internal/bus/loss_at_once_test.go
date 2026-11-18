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
	const sessionID = "ses_loss_at_once"
	filter := []string{contracts.AgentSubject(contracts.NoProject, sessionID)}
	ctx, cancel := context.WithTimeout(context.Background(), ownWait(t))
	defer cancel()
	if _, err := consumers.Ensure(ctx, sessionID, filter, 0); err != nil {
		t.Fatalf("create the session's consumer: %v", err)
	}

	proxy.Cut()
	for consumers.client.Connected() {
		time.Sleep(time.Millisecond)
	}
	calls := []struct {
		name string
		call func(context.Context) error
	}{
		{"the stream read", func(ctx context.Context) error { _, err := consumers.StreamID(ctx); return err }},
		{"a consumer create or update", func(ctx context.Context) error { _, err := consumers.Ensure(ctx, sessionID, filter, 0); return err }},
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
		if !errors.Is(err, ErrConnectionLost) || fired || consumers.client.Connected() {
			t.Fatalf("%s made while the connection was down answered %v after %s (its own deadline fired: %t), want ErrConnectionLost while that deadline ran and the connection was down", call.name, err, took, fired)
		}
		t.Logf("%s made while the connection was down answered %s: %v", call.name, took, err)
		lost = err
	}

	proxy.Restore()
	if !Reconnected(ctx, lost) {
		t.Fatal("Reconnected did not report the connection back once the proxy was restored")
	}
	if id, err := consumers.StreamID(ctx); err != nil || id == "" {
		t.Fatalf("the stream read made once the connection was back answered %q, %v", id, err)
	}
}
