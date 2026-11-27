package bus_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/testnats"
)

// A write while the client's connection is reconnecting fails by name, and is never sent: the
// client keeps no reconnect buffer, so nats.go refuses every send then with its own
// nats.ErrReconnectBufExceeded, "nats: outbound buffer limit exceeded", which names a buffer and
// not the reconnect. A KV write, which takes no deadline, fails at once with an error that says
// NATS is reconnecting and carries nats.go's refusal; a publish waits for the connection within
// its deadline, and one whose deadline ends first fails with that same name. Neither is in the
// bucket or the stream once the server is back.
//
// The controls: a send on the client's own connection during the outage fails with nats.go's
// refusal, whose text the two writes' errors must not hold; and once the server is back, the same
// key and the same publish, sent again, are read by the row's observer, so the observer would read
// either write had it reached the server.
//
// A KV write whose caller holds a deadline waits for the connection within it (bus.Bounded); the
// listener's TestAWriteThroughANATSReconnectWaitsOrFailsByName drives that through its routes.
func TestAWriteThroughANATSReconnectWaitsOrFailsByName(t *testing.T) {
	// Serial (busTestParallelism): the KV write's failure has a wall-clock bound of 1 s.
	const (
		reconnecting    = "NATS is reconnecting"
		key             = "written-while-reconnecting"
		publishDeadline = 500 * time.Millisecond
	)
	bufferRefusal := natsgo.ErrReconnectBufExceeded.Error()
	srv, uri := testnats.Start(t)
	client, err := bus.ConnectOwningStream([]string{uri})
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(client.Close)
	kv, err := bus.EnsureKeyValue(t.Context(), client.Conn, client.JS(), &natsgo.KeyValueConfig{Bucket: "reconnect-writes"})
	if err != nil {
		t.Fatalf("open the bucket: %v", err)
	}
	item := contracts.Envelope{
		EventID:        "evt-publish-while-reconnecting-by-name",
		Source:         "github",
		SourceEventID:  "source-publish-while-reconnecting-by-name",
		Topic:          "notifications.github.acme.widgets.push.branch.reconnect-writes",
		DedupeKey:      "publish-while-reconnecting-by-name",
		IssuedAt:       contracts.NowMillis(),
		PayloadSummary: "a publish whose deadline ends while NATS reconnects",
		TraceID:        "trace-publish-while-reconnecting-by-name",
	}

	srv.Stop(t)
	waitFor(t, "the client to start reconnecting", func() bool { return client.Conn.IsReconnecting() })

	t.Run("control: a send on the client's own connection while reconnecting is nats.go's buffer refusal", func(t *testing.T) {
		err := client.Conn.Publish("reconnect-writes.raw", []byte("{}"))
		if !errors.Is(err, natsgo.ErrReconnectBufExceeded) || !strings.Contains(err.Error(), bufferRefusal) {
			t.Fatalf("a send on the client's own connection while reconnecting answered %v, want nats.go's %q", err, bufferRefusal)
		}
	})

	t.Run("a KV write while reconnecting fails at once by name", func(t *testing.T) {
		began := time.Now()
		_, err := kv.Put(key, []byte("never sent"))
		if err == nil {
			t.Fatal("a KV write while the server was away reported success")
		}
		if !strings.Contains(err.Error(), reconnecting) || strings.Contains(err.Error(), bufferRefusal) {
			t.Errorf("a KV write while reconnecting failed with %q, want an error naming %q and not %q", err, reconnecting, bufferRefusal)
		}
		if !errors.Is(err, natsgo.ErrReconnectBufExceeded) {
			t.Errorf("a KV write while reconnecting failed with %q, which does not carry nats.go's refusal %q", err, bufferRefusal)
		}
		if took := time.Since(began); took > time.Second {
			t.Errorf("a KV write while reconnecting took %s to fail; with no deadline to wait within it fails at once", took)
		}
	})

	t.Run("a publish whose deadline ends while reconnecting fails by name", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), publishDeadline)
		defer cancel()
		err := client.PublishWithin(ctx, item)
		if err == nil {
			t.Fatal("a publish while the server was away reported success")
		}
		if !strings.Contains(err.Error(), reconnecting) || strings.Contains(err.Error(), bufferRefusal) {
			t.Errorf("a publish whose deadline ended while reconnecting failed with %q, want an error naming %q and not %q", err, reconnecting, bufferRefusal)
		}
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("a publish whose deadline ended while reconnecting failed with %q, which does not carry its deadline", err)
		}
	})

	t.Run("neither write reaches the server once it is back", func(t *testing.T) {
		srv.Start(t)
		waitFor(t, "the client to reconnect", client.Connected)
		// A write held anywhere and sent on the reconnect would be on the server within this.
		time.Sleep(2 * time.Second)
		observer := testnats.Connect(t, uri)
		t.Cleanup(observer.Close)
		js, err := observer.JetStream()
		if err != nil {
			t.Fatalf("open JetStream: %v", err)
		}
		bucket, err := js.KeyValue("reconnect-writes")
		if err != nil {
			t.Fatalf("open the bucket: %v", err)
		}
		if entry, err := bucket.Get(key); !errors.Is(err, natsgo.ErrKeyNotFound) {
			t.Errorf("the KV write that failed while reconnecting is in the bucket: %v, %v", entry, err)
		}
		if message, err := js.GetLastMsg(bus.Stream, item.Topic); !errors.Is(err, natsgo.ErrMsgNotFound) {
			t.Errorf("the publish that failed while reconnecting is in the stream: %v, %v", message, err)
		}

		// The control: the same two writes, sent now, are what the observer reads.
		if _, err := kv.Put(key, []byte("sent once the server is back")); err != nil {
			t.Fatalf("control: the KV write once the server is back: %v", err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := client.PublishWithin(ctx, item); err != nil {
			t.Fatalf("control: the publish once the server is back: %v", err)
		}
		if _, err := bucket.Get(key); err != nil {
			t.Errorf("control: the observer did not read the KV write sent once the server was back: %v", err)
		}
		if _, err := js.GetLastMsg(bus.Stream, item.Topic); err != nil {
			t.Errorf("control: the observer did not read the publish sent once the server was back: %v", err)
		}
	})
}
