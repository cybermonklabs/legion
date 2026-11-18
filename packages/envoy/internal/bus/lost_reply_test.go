package bus

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/testnats"
)

// What these rows decide is an order, not a time: a request whose reply the connection's drop
// lost fails at the drop, while the client is still disconnected and before its own deadline
// fires, where nats.go alone leaves it to fail at that deadline (processOpErr clears no pending
// request). The hold cuts the connection and refuses every reconnect (testnats.ReplyHold.Cut) until
// the row has its answer, so a request that fails only at the reconnect cannot pass. A row gives
// its request a deadline of its own that ends just short of the test binary's (ownWait), and sees
// the order through the error, that deadline and the connection: the answer is ErrConnectionLost,
// which bindRequest gives only to a request whose own context is still running; the request's own
// context has not fired when it comes; and the client is not connected when it comes. Every wait
// is bounded by the test's deadline alone (testnats.Remaining). So a loaded or starved box can make
// a row slow, never fail it; a request that waits for its own deadline, or for the reconnect,
// fails it.

// ownWait is the deadline a row gives the request it holds: five seconds short of what the test
// binary's deadline leaves, so the request's own deadline fires, and fails the row with its own
// error, before the row's wait for it (testnats.Remaining) ends.
func ownWait(t *testing.T) time.Duration { return testnats.Remaining(t) - 5*time.Second }

// heldClient is a client of its own on a server of its own, connected through a hold
// (testnats.ReplyHold), owning the stream so the stream's requests have a stream to answer for.
func heldClient(t *testing.T) (*Client, *testnats.ReplyHold) {
	t.Helper()
	hold := testnats.StartReplyHold(t, testnats.URL(t))
	client, err := ConnectOwningStream([]string{hold.URL()}, WithReplicas(1))
	if err != nil {
		t.Fatalf("connect through the hold: %v", err)
	}
	t.Cleanup(client.Close)
	return client, hold
}

// flushes says whether a request sends a flush behind its publish (publishConfirmed) and so is
// waiting for its reply only once that flush's PONG has reached it (testnats.HeldReplies.Flushed),
// or sends none and is waiting from the moment its reply is withheld.
type flushes bool

const (
	withFlush flushes = true
	noFlush   flushes = false
)

// requireLostAtTheDrop holds the replies to the requests on prefix, runs send with a context of
// the request's own (ownWait), waits until the hold has withheld its reply and, for a request that
// flushes, forwarded that flush's PONG, so the request is past its publish and its flush and
// waiting for its reply, then cuts the connection (testnats.ReplyHold.Cut). It requires send to
// fail with ErrConnectionLost while its own context is still running and while connected, the
// client's own report, is still false, and only then restores the hold, so the client reconnects
// for what the row does next. Every wait is bounded by the test's deadline alone.
func requireLostAtTheDrop(t *testing.T, hold *testnats.ReplyHold, connected func() bool, prefix string, flush flushes, send func(ctx context.Context) error) {
	t.Helper()
	own, cancel := context.WithTimeout(context.Background(), ownWait(t))
	defer cancel()
	held := hold.Hold(prefix)
	answered := make(chan error, 1)
	go func() { answered <- send(own) }()
	waitFor := func(event <-chan struct{}, what string) {
		t.Helper()
		select {
		case <-event:
		case err := <-answered:
			t.Fatalf("the request on %s answered %v before the hold %s", prefix, err, what)
		case <-time.After(testnats.Remaining(t)):
			t.Fatalf("the hold had not %s of a request on %s by the test's deadline", what, prefix)
		}
	}
	waitFor(held.Withheld(), "withheld its reply")
	if flush {
		waitFor(held.Flushed(), "forwarded the PONG of its flush")
	}
	dropped := time.Now()
	hold.Cut()
	defer hold.Restore()
	select {
	case err := <-answered:
		took := time.Since(dropped)
		reconnected := connected()
		if !errors.Is(err, ErrConnectionLost) {
			t.Fatalf("the request on %s whose reply the drop lost answered %v %s after the drop, want ErrConnectionLost", prefix, err, took)
		}
		if own.Err() != nil {
			t.Fatalf("the request on %s whose reply the drop lost answered %v only once its own deadline had fired, want it failed at the drop", prefix, err)
		}
		if reconnected {
			t.Fatalf("the request on %s whose reply the drop lost answered %v only once the client was connected again, want it failed while the client was disconnected", prefix, err)
		}
		t.Logf("the request on %s whose reply the drop lost failed %s after the drop, its own deadline still running and the client still disconnected: %v", prefix, took, err)
	case <-time.After(testnats.Remaining(t)):
		t.Fatalf("the request on %s whose reply the drop lost was still waiting at the test's deadline, want it failed at the drop", prefix)
	}
}

// A request on a client's connection whose reply the connection's drop lost fails at the drop with
// ErrConnectionLost, before its own deadline fires, whatever shape of request it is: a core request
// on a reply inbox of its own (requestConfirmed, which carries every session consumer call and
// stream read, and RequestCore, the connection probe's), a forward's wait for the stream's
// acknowledgement, a JetStream API call through the client's JetStream context, and a KV read and
// write through a bucket handle opened on the client's connection. The request reaches the server,
// which answers it, and the hold keeps that answer from the client: what the client sees is a
// request whose reply its lost connection took. Each request's own deadline is the row's (ownWait):
// its context, the forward's timeout, and for the calls nats.go gives no context, the MaxWait of
// the JetStream context they go through.
//
// The control is the same rows with the binding taken out (bindRequest binding nothing), and at
// main a97cd59397 and 44a1f1fb97: nats.go clears no pending request when a dropped socket turns the
// connection to reconnecting (processOpErr), so each request waits for its own deadline and every
// row fails with the error that deadline gives.
func TestARequestWhoseReplyTheDropLostFailsAtTheDrop(t *testing.T) {
	for _, row := range []struct {
		name, prefix string
		flush        flushes
		// prepare makes what the row's request needs before anything is held, and returns the
		// request, which it makes under the context it is given, its own.
		prepare func(*testing.T, *Client) func(context.Context) error
	}{
		{"core request", "$JS.API.STREAM.INFO.", withFlush, func(_ *testing.T, c *Client) func(context.Context) error {
			return func(ctx context.Context) error {
				_, err := c.requestConfirmed(ctx, "$JS.API.STREAM.INFO."+Stream, nil)
				return err
			}
		}},
		{"core request on a channel inbox", "envoy.lostreply.probe", withFlush, func(_ *testing.T, c *Client) func(context.Context) error {
			return func(ctx context.Context) error {
				_, err := c.RequestCore(ctx, "envoy.lostreply.probe", nats.Header{ConnectionProbeHeader: []string{"1"}})
				return err
			}
		}},
		// An agent subject, which the stream stores, so the withheld reply is the stream's
		// acknowledgement of the forward.
		{"forward", contracts.AgentSubject(contracts.NoProject, "ses_lost_forward"), withFlush, func(_ *testing.T, c *Client) func(context.Context) error {
			item := contracts.Envelope{Topic: contracts.AgentSubject(contracts.NoProject, "ses_lost_forward"), Source: "test", DedupeKey: "lost-reply"}
			return func(ctx context.Context) error {
				deadline, _ := ctx.Deadline()
				forward, err := c.StartRequestStoredTo(item.Topic, item, "lost-reply", time.Until(deadline))
				if err != nil {
					return err
				}
				_, err = forward.Wait()
				return err
			}
		}},
		{"JetStream API call", "$JS.API.STREAM.INFO.", noFlush, func(_ *testing.T, c *Client) func(context.Context) error {
			return func(ctx context.Context) error {
				_, err := c.JS().StreamInfo(Stream, nats.Context(ctx))
				return err
			}
		}},
		{"KV read", "$JS.API.DIRECT.GET.KV_envoy_lost_reply", noFlush, func(t *testing.T, c *Client) func(context.Context) error {
			kv := lostReplyBucket(t, c)
			return func(context.Context) error {
				_, err := kv.Get("key")
				return err
			}
		}},
		{"KV write", "$KV.envoy_lost_reply.", noFlush, func(t *testing.T, c *Client) func(context.Context) error {
			kv := lostReplyBucket(t, c)
			return func(context.Context) error {
				_, err := kv.Put("key", []byte("value"))
				return err
			}
		}},
	} {
		t.Run(row.name, func(t *testing.T) {
			client, hold := heldClient(t)
			requireLostAtTheDrop(t, hold, client.Connected, row.prefix, row.flush, row.prepare(t, client))
		})
	}
}

// lostReplyBucket opens the bucket the KV rows hold their requests on, on c's connection, as the
// listener opens its buckets (EnsureKeyValue through a JetStream context bound to the connection's
// life, JetStream). The handle's calls take no context, so their own deadline is that context's
// MaxWait, which is the row's (ownWait).
func lostReplyBucket(t *testing.T, c *Client) KeyValue {
	t.Helper()
	js, err := JetStream(c.Conn, nats.MaxWait(ownWait(t)))
	if err != nil {
		t.Fatalf("open JetStream: %v", err)
	}
	kv, err := EnsureKeyValue(t.Context(), c.Conn, js, &nats.KeyValueConfig{Bucket: "envoy_lost_reply", Storage: nats.MemoryStorage})
	if err != nil {
		t.Fatalf("open the bucket: %v", err)
	}
	return kv
}

// The shared read of the stream's state (SessionConsumers.streamState), which every session
// consumer call waits on, gives up a read whose reply the connection's drop lost at the drop, before
// that read's own deadline (sessionStreamReadWait) fires, and the read made once the connection is
// back is answered: a session registration after a reconnect, which waits on that read before its
// consumer call, no longer waits behind the one the drop cut off (the held-reconnect harness: 7 of
// 30 registrations answered 503 at main).
//
// The control is the same row with the binding taken out, and at main a97cd59397: the read cut off
// waits out its own sessionStreamReadWait (5 s) and answers that deadline's error, not
// ErrConnectionLost.
func TestAStreamReadTheDropCutOffReleasesTheCallsWaitingOnIt(t *testing.T) {
	hold := testnats.StartReplyHold(t, testnats.URL(t))
	owner, err := ConnectOwningStream([]string{hold.URL()}, WithReplicas(1))
	if err != nil {
		t.Fatalf("connect the stream's owner: %v", err)
	}
	t.Cleanup(owner.Close)
	consumers, err := ConnectSessionConsumers([]string{hold.URL()}, 0, nil)
	if err != nil {
		t.Fatalf("connect the session consumers: %v", err)
	}
	t.Cleanup(consumers.Close)
	requireLostAtTheDrop(t, hold, consumers.client.Connected, "$JS.API.STREAM.INFO."+Stream, withFlush, func(ctx context.Context) error {
		_, err := consumers.StreamID(ctx)
		return err
	})
	ctx, cancel := context.WithTimeout(context.Background(), ownWait(t))
	defer cancel()
	for !consumers.client.Connected() {
		if ctx.Err() != nil {
			t.Fatal("the session consumers' connection was not back by the test's deadline")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if id, err := consumers.StreamID(ctx); err != nil || id == "" {
		t.Fatalf("the stream read made once the connection was back answered %q, %v; want the stream's id", id, err)
	}
}

// A session consumer's pull whose connection drops ends at the drop with ErrConnectionLost: the
// pull request standing at the server went with the connection, and the relay restarts the
// consumer on the new connection rather than waiting to hear nothing for twice the pull's heartbeat
// (ErrSessionPullStalled). That stall is the pull's own end, so the order is seen through the
// error: ErrConnectionLost, which only the end of the connection's life gives it.
//
// The control is the same row at main a97cd59397: Next goes on waiting for the server that will
// never send on the lost pull, until it stalls at twice sessionPullHeartbeat (30 s).
func TestAPullWhoseConnectionDropsEndsAtTheDrop(t *testing.T) {
	hold := testnats.StartReplyHold(t, testnats.URL(t))
	owner, err := ConnectOwningStream([]string{hold.URL()}, WithReplicas(1))
	if err != nil {
		t.Fatalf("connect the stream's owner: %v", err)
	}
	t.Cleanup(owner.Close)
	consumers, err := ConnectSessionConsumers([]string{hold.URL()}, 0, nil)
	if err != nil {
		t.Fatalf("connect the session consumers: %v", err)
	}
	t.Cleanup(consumers.Close)
	const sessionID = "ses_lost_pull"
	ctx, cancel := context.WithTimeout(context.Background(), ownWait(t))
	defer cancel()
	if _, err := consumers.Ensure(ctx, sessionID, []string{contracts.AgentSubject(contracts.NoProject, sessionID)}, 0); err != nil {
		t.Fatalf("create the session's consumer: %v", err)
	}
	pull, err := consumers.Pull(ctx, sessionID)
	if err != nil {
		t.Fatalf("pull the session's consumer: %v", err)
	}
	t.Cleanup(pull.Stop)
	answered := make(chan error, 1)
	go func() {
		_, err := pull.Next(context.Background())
		answered <- err
	}()
	// The pull request is out and the stream holds nothing for it: Next waits.
	select {
	case err := <-answered:
		t.Fatalf("the pull of an empty consumer answered %v before the drop", err)
	case <-time.After(200 * time.Millisecond):
	}
	dropped := time.Now()
	hold.Cut()
	defer hold.Restore()
	select {
	case err := <-answered:
		took := time.Since(dropped)
		if !errors.Is(err, ErrConnectionLost) {
			t.Fatalf("the pull whose connection dropped ended %s after the drop with %v, want ErrConnectionLost before it stalled", took, err)
		}
		if consumers.client.Connected() {
			t.Fatalf("the pull whose connection dropped ended %s after the drop only once the client was connected again, want it ended while the client was disconnected", took)
		}
		t.Logf("the pull whose connection dropped ended %s after the drop, the client still disconnected: %v", took, err)
	case <-time.After(testnats.Remaining(t)):
		t.Fatal("the pull whose connection dropped was still waiting at the test's deadline, want it ended at the drop")
	}
}

// A pull whose connection drops in the moment between its read of the connection's life and its
// wait still ends at the drop with ErrConnectionLost, before it stalls: the wait it then makes is on
// that life, which has ended. The row forces that moment (beforeWait): the drop is made there and
// the hook returns only once the life has ended, so the wait begins on a life already over.
//
// The control is the same row at 2b8a5d7cbb with the hook placed in that moment: that take read
// the life a second time after its loss check, found it ended, took that for a stopped client's and
// waited on nothing, so the pull heard of the loss only when it stalled, at twice
// sessionPullHeartbeat (30 s).
func TestAPullWhoseConnectionDropsBeforeItsWaitStillEndsAtTheDrop(t *testing.T) {
	hold := testnats.StartReplyHold(t, testnats.URL(t))
	owner, err := ConnectOwningStream([]string{hold.URL()}, WithReplicas(1))
	if err != nil {
		t.Fatalf("connect the stream's owner: %v", err)
	}
	t.Cleanup(owner.Close)
	consumers, err := ConnectSessionConsumers([]string{hold.URL()}, 0, nil)
	if err != nil {
		t.Fatalf("connect the session consumers: %v", err)
	}
	t.Cleanup(consumers.Close)
	const sessionID = "ses_lost_pull_before_wait"
	ctx, cancel := context.WithTimeout(context.Background(), ownWait(t))
	defer cancel()
	if _, err := consumers.Ensure(ctx, sessionID, []string{contracts.AgentSubject(contracts.NoProject, sessionID)}, 0); err != nil {
		t.Fatalf("create the session's consumer: %v", err)
	}
	pull, err := consumers.Pull(ctx, sessionID)
	if err != nil {
		t.Fatalf("pull the session's consumer: %v", err)
	}
	t.Cleanup(pull.Stop)
	dropped := make(chan time.Time, 1)
	var once sync.Once
	pull.beforeWait = func() {
		once.Do(func() {
			dropped <- time.Now()
			hold.Cut()
			for pull.life.Err() == nil && testnats.Remaining(t) > 0 {
				time.Sleep(time.Millisecond)
			}
		})
	}
	t.Cleanup(hold.Restore)
	answered := make(chan error, 1)
	go func() {
		_, err := pull.Next(context.Background())
		answered <- err
	}()
	var at time.Time
	select {
	case at = <-dropped:
	case err := <-answered:
		t.Fatalf("the pull answered %v before it waited", err)
	case <-time.After(testnats.Remaining(t)):
		t.Fatal("the pull made no wait by the test's deadline")
	}
	select {
	case err := <-answered:
		took := time.Since(at)
		if !errors.Is(err, ErrConnectionLost) {
			t.Fatalf("the pull whose connection dropped before its wait ended %s after the drop with %v, want ErrConnectionLost before it stalled", took, err)
		}
		if consumers.client.Connected() {
			t.Fatalf("the pull whose connection dropped before its wait ended %s after the drop only once the client was connected again, want it ended while the client was disconnected", took)
		}
		t.Logf("the pull whose connection dropped before its wait ended %s after the drop, the client still disconnected: %v", took, err)
	case <-time.After(testnats.Remaining(t)):
		t.Fatal("the pull whose connection dropped before its wait was still waiting at the test's deadline, want it ended at the drop")
	}
}
