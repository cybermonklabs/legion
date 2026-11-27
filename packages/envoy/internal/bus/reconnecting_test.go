package bus

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/testnats"
	"github.com/sjawhar/envoy/internal/testwait"
)

// A JetStream send made while nats.go reconnects the connection fails at once by name, at the one
// place its kind of send names its error: an answer to a pulled message (sessionAck), a session
// consumer's pull request (SessionPull.pull), and a request through the client's JetStream
// context or through a context a caller hands a bucket call (namedRequests). Each error is
// ErrReconnecting, reads `bus: NATS is reconnecting` and nothing more, so nothing named it twice,
// and still unwraps to the error nats.go gave. Most sends nats.go refuses with "nats: outbound
// buffer limit exceeded", since the client keeps no reconnect buffer. A KV bucket's lookup it
// refuses before it sends, with "nats: key-value requires at least server version 2.6.2", since a
// reconnecting connection reports no server version; the context names that from the
// connection's state. EnsureKeyValue's create of a bucket's stream (createBucket, over
// createBucketStream) makes nats.go's requests one at a time: its account read is a request of
// the context, and its own check of the server's version refuses as nats.go's does, which the
// step names from the connection's state too.
func TestAJetStreamSendWhileNATSReconnectsFailsByName(t *testing.T) {
	t.Parallel()
	const (
		named   = "bus: NATS is reconnecting"
		session = "ses_reconnecting_sends"
		bucket  = "reconnecting-sends"
	)
	srv, uri := testnats.Start(t)
	owner, err := ConnectOwningStream([]string{uri})
	if err != nil {
		t.Fatalf("connect the stream's owner: %v", err)
	}
	t.Cleanup(owner.Close)
	consumers, err := ConnectSessionConsumers([]string{uri}, 0, nil)
	if err != nil {
		t.Fatalf("connect session consumers: %v", err)
	}
	t.Cleanup(consumers.Close)
	ctx := context.Background()
	agent := contracts.LegacyAgentSubject(session)
	if _, err := consumers.Ensure(ctx, session, []string{agent}, 0); err != nil {
		t.Fatalf("ensure the session's consumer: %v", err)
	}
	publishStored(t, owner, agent, "held")
	pull := pullSession(t, consumers, session)
	msg := next(t, pull)
	// A context of the caller's own, as the listener opens its spent-proof bucket through one
	// (cmd/listener/brokerauth.go): nothing names its requests until a bucket call is handed it.
	handed, err := owner.Conn.JetStream()
	if err != nil {
		t.Fatalf("open a JetStream context of the test's own: %v", err)
	}
	if _, err := handed.CreateKeyValue(&nats.KeyValueConfig{Bucket: bucket}); err != nil {
		t.Fatalf("create the bucket: %v", err)
	}

	srv.Stop(t)
	testwait.WithinPolling(t, 15*time.Second, 50*time.Millisecond, "both connections to start reconnecting", func() bool {
		return owner.Conn.IsReconnecting() && consumers.client.Connection().IsReconnecting()
	})

	for _, row := range []struct {
		what string
		send func() error
		want string
		// refused is whether nats.go refused the send at its reconnect buffer, rather than
		// answering it with words of its own before it sent anything.
		refused bool
	}{
		{what: "an ack of a pulled message", send: msg.Ack, want: named, refused: true},
		{what: "a progress of a pulled message", send: msg.InProgress, want: named, refused: true},
		{what: "a nak of a pulled message", send: func() error { return msg.NakWithDelay(0) }, want: named, refused: true},
		{what: "a pull request", send: func() error {
			// The pull's request in flight has delivered all it may, so Next sends the next.
			pull.remaining = 0
			_, err := pull.Next(ctx)
			return err
		}, want: named, refused: true},
		{what: "a stream read through the client's context", send: func() error {
			_, err := owner.JS().StreamInfo(Stream)
			return err
		}, want: named, refused: true},
		{what: "a consumer create through the client's context", send: func() error {
			_, err := owner.JS().AddConsumer(Stream, &nats.ConsumerConfig{Durable: "reconnecting-sends", AckPolicy: nats.AckExplicitPolicy})
			return err
		}, want: named, refused: true},
		{what: "a bucket lookup through the client's context", send: func() error {
			_, err := owner.JS().KeyValue(bucket)
			return err
		}, want: named},
		{what: "a bucket open through a context its caller handed it", send: func() error {
			_, err := OpenKeyValue(ctx, owner.Conn, handed, bucket)
			return err
		}, want: named},
		{what: "a bucket's ensure through a context its caller handed it", send: func() error {
			_, err := EnsureKeyValue(ctx, owner.Conn, handed, &nats.KeyValueConfig{Bucket: bucket})
			return err
		}, want: "KV_" + bucket + " STREAM.INFO: " + named},
		{what: "a bucket's age set through a context its caller handed it", send: func() error {
			_, err := SetKeyValueMaxAge(ctx, owner.Conn, handed, bucket, time.Hour)
			return err
		}, want: "set KV_" + bucket + " max age: KV_" + bucket + " STREAM.INFO: " + named, refused: true},
		{what: "an account read through the client's context", send: func() error {
			_, err := owner.JS().AccountInfo()
			return err
		}, want: named, refused: true},
		{what: "a bucket's create step through a context its caller handed it", send: func() error {
			return createBucket(ctx, owner.Conn, readsRefusals(owner.Conn, namedJetStream(owner.Conn, handed)),
				&nats.KeyValueConfig{Bucket: bucket + "-created"})
		}, want: named},
	} {
		t.Run(row.what, func(t *testing.T) {
			err := row.send()
			if err == nil {
				t.Fatal("the send reported success while NATS reconnects")
			}
			var send reconnectingSend
			if !errors.Is(err, ErrReconnecting) || err.Error() != row.want || !errors.As(err, &send) || send.refusal == nil {
				t.Fatalf("the send while NATS reconnects failed with %q (ErrReconnecting %t), want %q, ErrReconnecting carrying the error nats.go gave",
					err, errors.Is(err, ErrReconnecting), row.want)
			}
			refused := errors.Is(err, nats.ErrReconnectBufExceeded) || errors.Is(err, nats.ErrConnectionReconnecting)
			if row.refused && !refused {
				t.Errorf("the send while NATS reconnects failed with %q, which no longer carries nats.go's refusal: %v", err, send.refusal)
			}
			t.Logf("nats.go's own error: %v", send.refusal)
		})
	}
}

// A bucket call whose caller's ctx had ended before the call started sends nothing. While NATS
// reconnects, its error says why the bus did not answer: ErrReconnecting with ctx's error,
// `bus: NATS is reconnecting: context deadline exceeded`, as for a ctx that ends in the call's
// wait for the connection (Bounded). While the connection is up, the error is ctx's own. The
// listener's routes answer such an error, and its session filter fence logs it
// (cmd/listener/session_filter_fence.go). With ctx's own error, that line would read `context
// deadline exceeded` once in each outage and name no reconnect.
//
// The control: while the connection is up, the same call under a live ctx starts and is answered,
// so a call that started under an ended ctx would be seen.
func TestABucketCallWhoseContextEndedWhileNATSReconnectsFailsByName(t *testing.T) {
	// Serial (busTestParallelism): its connected rows' reads have a wall-clock bound, ConnLockWait.
	srv, uri := testnats.Start(t)
	client, err := ConnectOwningStream([]string{uri})
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(client.Close)
	conn := client.Connection()
	ends := []struct {
		what string
		ctx  func() (context.Context, context.CancelFunc)
		err  error
		want string
	}{
		{what: "a deadline that has passed", ctx: func() (context.Context, context.CancelFunc) {
			return context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
		}, err: context.DeadlineExceeded, want: "bus: NATS is reconnecting: context deadline exceeded"},
		{what: "a cancel", ctx: func() (context.Context, context.CancelFunc) {
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			return ctx, cancel
		}, err: context.Canceled, want: "bus: NATS is reconnecting: context canceled"},
	}
	// bounded makes one bucket call under the ended ctx that ctx returns, and requires that the
	// call never started.
	bounded := func(t *testing.T, ctx func() (context.Context, context.CancelFunc)) error {
		t.Helper()
		ended, cancel := ctx()
		defer cancel()
		started := false
		_, answered, err := Bounded(ended, conn, func() (struct{}, error) {
			started = true
			return struct{}{}, nil
		})
		if started || answered {
			t.Fatalf("the call started (%t) or was answered (%t) under a ctx that had ended", started, answered)
		}
		return err
	}

	for _, end := range ends {
		t.Run("connected, "+end.what, func(t *testing.T) {
			live, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			started := false
			_, answered, err := Bounded(live, conn, func() (struct{}, error) {
				started = true
				return struct{}{}, nil
			})
			if !started || !answered || err != nil {
				t.Fatalf("control: the call under a live ctx started (%t) and was answered (%t) with %v, want it started and answered nil", started, answered, err)
			}
			if err := bounded(t, end.ctx); err != end.err {
				t.Fatalf("the call while connected failed with %q (ErrReconnecting %t), want ctx's own error %q", err, errors.Is(err, ErrReconnecting), end.err)
			}
		})
	}

	srv.Stop(t)
	testwait.WithinPolling(t, 15*time.Second, 50*time.Millisecond, "the connection to start reconnecting", conn.IsReconnecting)

	for _, end := range ends {
		t.Run("reconnecting, "+end.what, func(t *testing.T) {
			err := bounded(t, end.ctx)
			if !errors.Is(err, ErrReconnecting) || !errors.Is(err, end.err) || err.Error() != end.want {
				t.Fatalf("the call while NATS reconnects failed with %q (ErrReconnecting %t, %v %t), want %q",
					err, errors.Is(err, ErrReconnecting), end.err, errors.Is(err, end.err), end.want)
			}
		})
	}
}
