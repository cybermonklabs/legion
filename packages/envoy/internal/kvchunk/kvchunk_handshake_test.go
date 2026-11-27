package kvchunk_test

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/kvchunk"
	"github.com/sjawhar/envoy/internal/kvchunk/kvchunktest"
	"github.com/sjawhar/envoy/internal/testnats"
	"github.com/sjawhar/envoy/internal/testwait"
)

// heldBound is the bound the row gives a write it makes while a handshake is held.
const heldBound = 300 * time.Millisecond

// A chunked write begun while nats.go holds its connection lock through a reconnect's handshake
// answers at its ctx's end, names the reconnect, and writes nothing. Write lays the record out by
// the server's max payload, and that read (bus.KeyValue.MaxPayload, nats.Conn.MaxPayload) takes
// nats.go's connection lock, so Write makes it through bus.Bounded, as it makes each bucket call.
// A read made on the caller's goroutine would hold the write until the handshake attempt ends, at
// the client's connect timeout (5 s), long past the write's bound.
//
// The row holds a reconnect's handshake (testnats.HandshakeHold) and writes a record of three times
// the server's max payload under a ctx bounded at 300 ms. The write answers within the attempt held
// when it began (HandshakeHold.AnswersWithinAttempt), once its bound has passed and not before,
// with `write the record held: read the server's max payload: bus: NATS is reconnecting: context
// deadline exceeded`, which is bus.ErrReconnecting with ctx's error. Its head is never called, and
// once the hold is released the bucket holds no chunk of it and no value under its key.
//
// The controls: a write of another key before the hold, and a write of the held key once the hold
// is released and the connection is back, each store the record in chunks and call the head once,
// so the bucket shows the chunks of a write that is made, and the answer is the hold's. The
// negative control reads the max payload on the caller's goroutine, outside the bound: then the
// write answers only once the attempt has ended, and the row fails.
func TestAChunkedWriteBegunDuringAHeldHandshakeAnswersAtItsBoundAndWritesNothing(t *testing.T) {
	_, uri := testnats.StartConfig(t, fmt.Sprintf("max_payload: %d\njetstream {}\n", kvchunktest.MaxPayload), []string{"-js"}, true)
	hold := testnats.StartHandshakeHold(t, uri)
	client, err := bus.Connect([]string{hold.URL()})
	if err != nil {
		t.Fatalf("connect through the handshake hold: %v", err)
	}
	t.Cleanup(client.Close)
	t.Cleanup(hold.Restore)
	t.Cleanup(hold.Release)
	conn := client.Connection()
	if got := conn.MaxPayload(); got != kvchunktest.MaxPayload {
		t.Fatalf("the server's max payload is %d bytes, want %d", got, kvchunktest.MaxPayload)
	}
	js, err := conn.JetStream()
	if err != nil {
		t.Fatalf("jetstream: %v", err)
	}
	const bucket = "kvchunk_held"
	kv, err := bus.EnsureKeyValue(t.Context(), conn, js, &nats.KeyValueConfig{Bucket: bucket, Storage: nats.MemoryStorage})
	if err != nil {
		t.Fatalf("create the bucket: %v", err)
	}
	legacy, err := js.KeyValue(bucket)
	if err != nil {
		t.Fatalf("open the bucket's legacy handle: %v", err)
	}
	record := []byte(`{"topics":["` + strings.Repeat("x", 3*kvchunktest.MaxPayload) + `"]}`)
	var heads atomic.Int64
	write := func(ctx context.Context, key string) (kvchunk.Written, error) {
		return kvchunk.Write(ctx, kv, key, record, nil, kvchunk.Layout{}, func(head []byte) (uint64, error) {
			heads.Add(1)
			return kv.Put(key, head)
		}, time.Now, slog.Default(), "record")
	}
	stores := func(t *testing.T, key, when string, want []string) []string {
		t.Helper()
		ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
		defer cancel()
		called := heads.Load()
		written, err := write(ctx, key)
		if err != nil || !written.Stored || !written.Layout.Chunked() || heads.Load()-called != 1 {
			t.Fatalf("control: a write of %s %s: %+v (%v), head called %d time(s); want a chunked record stored and its head called once",
				key, when, written, err, heads.Load()-called)
		}
		want = slices.Sorted(slices.Values(append(slices.Clone(want), written.Layout.Keys()...)))
		if live := kvchunktest.LiveChunkKeys(t, legacy); !slices.Equal(live, want) {
			t.Fatalf("control: after a write of %s %s the bucket holds the chunks %v, want %v", key, when, live, want)
		}
		return want
	}
	before := stores(t, "before", "before the hold", nil)

	hold.HoldReconnect(t, conn)
	called := heads.Load()
	answers := hold.AnswersWithinAttempt(t, conn, "a chunked write during the held handshake", func() error {
		began := time.Now()
		ctx, cancel := context.WithTimeout(context.Background(), heldBound)
		defer cancel()
		written, err := write(ctx, "held")
		if written != (kvchunk.Written{}) {
			return fmt.Errorf("a write that answered %+v, with %v", written, err)
		}
		if took := time.Since(began); took < heldBound {
			return fmt.Errorf("%w, after %s, before its bound of %s: it did not wait for the connection within its bound", err, took, heldBound)
		}
		return err
	})
	const want = "write the record held: read the server's max payload: bus: NATS is reconnecting: context deadline exceeded"
	if err := answers[0]; !errors.Is(err, bus.ErrReconnecting) || !errors.Is(err, context.DeadlineExceeded) || err.Error() != want {
		t.Fatalf("a chunked write during the held handshake answered %v, want %q", err, want)
	}
	if n := heads.Load() - called; n != 0 {
		t.Fatalf("the head of the write made during the held handshake was called %d time(s), want none", n)
	}

	hold.Release()
	testwait.WithinPolling(t, 30*time.Second, 10*time.Millisecond, "the connection to reconnect once the hold was released", conn.IsConnected)
	if live := kvchunktest.LiveChunkKeys(t, legacy); !slices.Equal(live, before) {
		t.Fatalf("once the hold was released the bucket holds the chunks %v, want only %v, the write's before the hold: the write made during the hold sent a chunk", live, before)
	}
	if _, err := kv.Get("held"); !errors.Is(err, nats.ErrKeyNotFound) {
		t.Fatalf("once the hold was released a read of the key written during the hold answered %v, want nats.ErrKeyNotFound", err)
	}
	stores(t, "held", "once the hold was released", before)
}
