package bus

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/sjawhar/envoy/internal/testnats"
	"github.com/sjawhar/envoy/internal/testwait"
)

// A client that asked for stream message reads (WithStreamMessageReads) reads and creates keys of
// an AllowDirect bucket without one direct get: Get, GetRevision, a Get of a key it does not hold
// and of a deleted one, and Create over a delete marker and over a live value all go through the
// stream message get, read off the wire from a second connection, and answer what nats.go's own
// reads answer. The control in the same row is a client that did not ask, whose Get of the same
// key is a direct get, so the wire reading sees one when it is sent.
func TestAClientWithStreamMessageReadsReadsAndCreatesWithNoDirectGet(t *testing.T) {
	const bucket = "kv_message_reads"
	const key = "ses_live"
	stream := "KV_" + bucket
	uri := testnats.URL(t)
	client, err := Connect([]string{uri}, WithStreamMessageReads())
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(client.Close)
	js, err := client.Conn.JetStream()
	if err != nil {
		t.Fatalf("jetstream: %v", err)
	}
	kv, err := EnsureKeyValue(t.Context(), client.Conn, js, &nats.KeyValueConfig{Bucket: bucket, Storage: nats.MemoryStorage})
	if err != nil {
		t.Fatalf("create bucket %s: %v", bucket, err)
	}
	t.Cleanup(func() { _ = js.DeleteKeyValue(bucket) })

	watcher, err := Connect([]string{uri})
	if err != nil {
		t.Fatalf("connect the second connection: %v", err)
	}
	t.Cleanup(watcher.Close)
	directGets, err := watcher.Conn.SubscribeSync("$JS.API.DIRECT.GET.>")
	if err != nil {
		t.Fatalf("subscribe to the direct gets: %v", err)
	}
	streamMsgGets, err := watcher.Conn.SubscribeSync("$JS.API.STREAM.MSG.GET.>")
	if err != nil {
		t.Fatalf("subscribe to the stream message gets: %v", err)
	}
	if err := watcher.Conn.Flush(); err != nil {
		t.Fatalf("flush the subscriptions: %v", err)
	}

	revision, err := kv.Put(key, []byte("live"))
	if err != nil {
		t.Fatalf("put %s: %v", key, err)
	}
	entry, err := kv.Get(key)
	if err != nil || string(entry.Value()) != "live" || entry.Revision() != revision {
		t.Fatalf("Get(%s) = %v, %v; want %q at revision %d", key, entry, err, "live", revision)
	}
	if entry, err := kv.GetRevision(key, revision); err != nil || string(entry.Value()) != "live" {
		t.Fatalf("GetRevision(%s, %d) = %v, %v; want %q", key, revision, entry, err, "live")
	}
	if _, err := kv.Get("ses_absent"); !errors.Is(err, nats.ErrKeyNotFound) {
		t.Fatalf("Get(ses_absent) answered %v, want nats.ErrKeyNotFound", err)
	}
	if err := kv.Delete(key); err != nil {
		t.Fatalf("delete %s: %v", key, err)
	}
	if _, err := kv.Get(key); !errors.Is(err, nats.ErrKeyNotFound) {
		t.Fatalf("Get(%s) after its delete answered %v, want nats.ErrKeyNotFound", key, err)
	}
	if _, err := kv.Create(key, []byte("again")); err != nil {
		t.Fatalf("Create(%s) over its delete marker: %v", key, err)
	}
	if entry, err := kv.Get(key); err != nil || string(entry.Value()) != "again" {
		t.Fatalf("Get(%s) after Create = %v, %v; want %q", key, entry, err, "again")
	}
	if _, err := kv.Create(key, []byte("third")); !errors.Is(err, nats.ErrKeyExists) || !IsCASConflict(err) {
		t.Fatalf("Create(%s) over a live value answered %v, want nats.ErrKeyExists", key, err)
	}

	if sent := requestFor(t, directGets, stream, noRequestFor); sent != nil {
		t.Fatalf("the client that reads with stream message gets sent the direct get %s", sent.Subject)
	}
	if requestFor(t, streamMsgGets, stream, noRequestFor) == nil {
		t.Fatalf("the client that reads with stream message gets sent no stream message get of %s", stream)
	}

	other, err := watcher.Conn.JetStream()
	if err != nil {
		t.Fatalf("jetstream: %v", err)
	}
	plain, err := OpenKeyValue(t.Context(), watcher.Conn, other, bucket)
	if err != nil {
		t.Fatalf("open %s on the second connection: %v", bucket, err)
	}
	if _, err := plain.Get(key); err != nil {
		t.Fatalf("the plain client's Get(%s): %v", key, err)
	}
	if requestFor(t, directGets, stream, noRequestFor*10) == nil {
		t.Fatalf("the plain client's Get(%s) drew no direct get on the wire, so the reading cannot see one", key)
	}
}

// A handle given its caller's context (Within) sends its writes through the jetstream handle, and
// on a client with stream message reads it creates without a direct get as well: Create over a
// delete marker and over a live value through Within read the marker with a stream message get
// (boundCreate), where nats.go's jetstream Create would read it with a direct get. The control is a
// plain client's bound Create over a marker, which sends one.
func TestABoundCreateOnAClientWithStreamMessageReadsSendsNoDirectGet(t *testing.T) {
	const bucket = "kv_message_reads_bound"
	const key = "ses_bound"
	stream := "KV_" + bucket
	uri := testnats.URL(t)
	client, err := Connect([]string{uri}, WithStreamMessageReads())
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(client.Close)
	js, err := client.Conn.JetStream()
	if err != nil {
		t.Fatalf("jetstream: %v", err)
	}
	kv, err := EnsureKeyValue(t.Context(), client.Conn, js, &nats.KeyValueConfig{Bucket: bucket, Storage: nats.MemoryStorage})
	if err != nil {
		t.Fatalf("create bucket %s: %v", bucket, err)
	}
	t.Cleanup(func() { _ = js.DeleteKeyValue(bucket) })
	bound := kv.Within(t.Context())

	watcher, err := Connect([]string{uri})
	if err != nil {
		t.Fatalf("connect the second connection: %v", err)
	}
	t.Cleanup(watcher.Close)
	directGets, err := watcher.Conn.SubscribeSync("$JS.API.DIRECT.GET.>")
	if err != nil {
		t.Fatalf("subscribe to the direct gets: %v", err)
	}
	if err := watcher.Conn.Flush(); err != nil {
		t.Fatalf("flush the subscription: %v", err)
	}

	if _, err := bound.Put(key, []byte("live")); err != nil {
		t.Fatalf("put %s: %v", key, err)
	}
	if err := bound.Delete(key); err != nil {
		t.Fatalf("delete %s: %v", key, err)
	}
	if _, err := bound.Create(key, []byte("again")); err != nil {
		t.Fatalf("a bound Create(%s) over its delete marker: %v", key, err)
	}
	if entry, err := kv.Get(key); err != nil || string(entry.Value()) != "again" {
		t.Fatalf("Get(%s) after the bound Create = %v, %v; want %q", key, entry, err, "again")
	}
	if _, err := bound.Create(key, []byte("third")); !errors.Is(err, nats.ErrKeyExists) || !IsCASConflict(err) {
		t.Fatalf("a bound Create(%s) over a live value answered %v, want nats.ErrKeyExists", key, err)
	}
	if sent := requestFor(t, directGets, stream, noRequestFor); sent != nil {
		t.Fatalf("the bound Create of the client that reads with stream message gets sent the direct get %s", sent.Subject)
	}

	other, err := watcher.Conn.JetStream()
	if err != nil {
		t.Fatalf("jetstream: %v", err)
	}
	plain, err := OpenKeyValue(t.Context(), watcher.Conn, other, bucket)
	if err != nil {
		t.Fatalf("open %s on the second connection: %v", bucket, err)
	}
	if err := plain.Delete(key); err != nil {
		t.Fatalf("the plain client's delete of %s: %v", key, err)
	}
	if _, err := plain.Within(t.Context()).Create(key, []byte("plain")); err != nil {
		t.Fatalf("the plain client's bound Create(%s) over its delete marker: %v", key, err)
	}
	if requestFor(t, directGets, stream, noRequestFor*10) == nil {
		t.Fatalf("the plain client's bound Create(%s) drew no direct get on the wire, so the reading cannot see one", key)
	}
}

// markerGets is the JetStream context a stream-message-reads handle reads through, its last
// message gets (GetLastMsg, the read of a delete marker in Create) taken over by the row: each fails
// at once with nats.ErrTimeout (fail), or is held until the context it was given ends, or the row
// ends (released); or, with after set, is sent as the handle would send it and then runs after. A
// get given no context fails at once too (except when passed through), and the row reads which
// context each get was given (given).
type markerGets struct {
	nats.JetStreamContext
	fail     bool
	after    func()
	released chan struct{}
	mu       sync.Mutex
	// gets counts the last message gets; given is the context the last one was given, nil for none.
	gets  int
	given context.Context
}

func (g *markerGets) GetLastMsg(stream, subject string, opts ...nats.JSOpt) (*nats.RawStreamMsg, error) {
	var given context.Context
	for _, opt := range opts {
		if ctx, ok := opt.(nats.ContextOpt); ok {
			given = ctx.Context
		}
	}
	g.mu.Lock()
	g.gets++
	g.given = given
	g.mu.Unlock()
	if g.after != nil {
		message, err := g.JetStreamContext.GetLastMsg(stream, subject, opts...)
		g.after()
		return message, err
	}
	if g.fail || given == nil {
		return nil, nats.ErrTimeout
	}
	select {
	case <-given.Done():
		return nil, given.Err()
	case <-g.released:
		return nil, nats.ErrTimeout
	}
}

// read is the number of last message gets made and the context the last was given.
func (g *markerGets) read() (int, context.Context) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.gets, g.given
}

// markerGetsOn replaces the JetStream context the reads of kv, a handle on a client with stream
// message reads, go through with markerGets, and returns it. A get still held when the row ends
// is released then.
func markerGetsOn(t *testing.T, kv *KeyValue, fail bool) *markerGets {
	t.Helper()
	reads, ok := kv.legacy.(streamMessageReads)
	if !ok {
		t.Fatalf("the handle's legacy handle is %T, not stream message reads", kv.legacy)
	}
	gets := &markerGets{JetStreamContext: reads.js, fail: fail, released: make(chan struct{})}
	t.Cleanup(func() { close(gets.released) })
	reads.js = gets
	kv.legacy = reads
	return gets
}

// liveBucket is a bucket on a client with stream message reads whose key holds a live value, and
// the client's own handle on it.
func liveBucket(t *testing.T, bucket, key string) KeyValue {
	t.Helper()
	client, err := Connect([]string{testnats.URL(t)}, WithStreamMessageReads())
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(client.Close)
	js, err := client.Conn.JetStream()
	if err != nil {
		t.Fatalf("jetstream: %v", err)
	}
	kv, err := EnsureKeyValue(t.Context(), client.Conn, js, &nats.KeyValueConfig{Bucket: bucket, Storage: nats.MemoryStorage})
	if err != nil {
		t.Fatalf("create bucket %s: %v", bucket, err)
	}
	t.Cleanup(func() { _ = js.DeleteKeyValue(bucket) })
	if _, err := kv.Put(key, []byte("live")); err != nil {
		t.Fatalf("put %s: %v", key, err)
	}
	return kv
}

// A bound Create's marker read on a client with stream message reads takes the caller's context:
// over a live value the write is refused, and the read of the marker is sent with a context that
// ends no later than the caller's deadline, so a read the server holds ends with the caller and
// the Create answers nats.ErrKeyExists. A read sent with no context would run on its own 10 s.
func TestABoundCreatesMarkerReadTakesTheCallersContext(t *testing.T) {
	const key = "ses_live"
	kv := liveBucket(t, "kv_marker_read_ctx", key)
	gets := markerGetsOn(t, &kv, false)
	caller, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_, err := kv.Within(caller).Create(key, []byte("again"))
	n, given := gets.read()
	if n != 1 {
		t.Fatalf("the bound Create over a live value made %d marker reads, want one, so this row holds nothing", n)
	}
	if given == nil {
		t.Fatal("the bound Create's marker read was sent with no context, so it runs on its own RequestTimeout past the caller's deadline")
	}
	callerEnd, _ := caller.Deadline()
	if readEnd, ok := given.Deadline(); !ok || readEnd.After(callerEnd) {
		t.Fatalf("the marker read's context ends at %v (has deadline %t), after the caller's %v", readEnd, ok, callerEnd)
	}
	if !errors.Is(err, nats.ErrKeyExists) || !IsCASConflict(err) {
		t.Fatalf("a bound Create over a live value whose marker read was held = %v, want nats.ErrKeyExists", err)
	}
}

// A bound Create's marker read ends when the caller cancels, not only at the caller's deadline:
// over a live value the read is held until its context ends, the caller cancels it while it is
// held, and the read's context has ended the moment the cancel returns (a context derived from the
// caller's is cancelled with it). The Create then answers nats.ErrKeyExists, before the test
// binary's deadline. A read whose context kept the caller's deadline but not its cancel would run
// on until that deadline.
func TestABoundCreatesMarkerReadEndsWhenTheCallerCancels(t *testing.T) {
	const key = "ses_live"
	kv := liveBucket(t, "kv_marker_read_cancel", key)
	gets := markerGetsOn(t, &kv, false)
	caller, cancel := context.WithTimeout(context.Background(), time.Hour)
	defer cancel()
	answered := make(chan error, 1)
	go func() {
		_, err := kv.Within(caller).Create(key, []byte("again"))
		answered <- err
	}()
	testwait.Until(t, "the bound Create's marker read to be sent", func() (int, bool) {
		n, _ := gets.read()
		return n, n > 0
	})
	cancel()
	n, given := gets.read()
	if n != 1 || given == nil {
		t.Fatalf("the bound Create made %d marker reads (context %v), want one with a context, so this row holds nothing", n, given)
	}
	if given.Err() == nil {
		t.Fatal("the marker read's context had not ended when the caller's cancel returned: it keeps running until its deadline after the caller gave up")
	}
	if err := testwait.Receive(t, "the bound Create's answer after the caller's cancel", answered); !errors.Is(err, nats.ErrKeyExists) || !IsCASConflict(err) {
		t.Fatalf("a bound Create over a live value whose caller cancelled during its marker read = %v, want nats.ErrKeyExists", err)
	}
}

// A bound Create whose marker read fails answers the refused write, as the legacy Create and
// nats.go's two Creates do, never a stored create: over a live value with the read failing at once
// (nats.ErrTimeout), the Create answers nats.ErrKeyExists and the key still holds its live value. A
// spent-proof create that answered a stored create here would report a proof spent with nothing
// written.
func TestABoundCreateWhoseMarkerReadFailsAnswersTheRefusedWrite(t *testing.T) {
	const key = "ses_live"
	kv := liveBucket(t, "kv_marker_read_fails", key)
	gets := markerGetsOn(t, &kv, true)
	revision, err := kv.Within(t.Context()).Create(key, []byte("again"))
	if n, _ := gets.read(); n != 1 {
		t.Fatalf("the bound Create over a live value made %d marker reads, want one, so this row holds nothing", n)
	}
	if !errors.Is(err, nats.ErrKeyExists) || !IsCASConflict(err) {
		t.Fatalf("a bound Create over a live value whose marker read failed = revision %d, %v; want nats.ErrKeyExists", revision, err)
	}
	// The handle's own reads go through the failing gets, so the key is read back through its
	// jetstream handle.
	entry, err := kv.WatchHandle().Get(t.Context(), key)
	if err != nil || string(entry.Value()) != "live" {
		t.Fatalf("read %s after the refused Create = %v, %v; want the live value", key, entry, err)
	}
}

// A bound Create on a client with stream message reads writes nothing over a delete marker once its
// caller has cancelled: the first write is refused (the key's last message is the marker), the
// marker read is sent as the handle sends it, the caller cancels the moment it returns, and the
// write over the marker must not go out. The Create answers the caller's context.Canceled and the
// key stays deleted. A write over the marker sent with a context that does not end with the
// caller's would store the value after the caller gave up.
func TestABoundCreateWritesNothingOverAMarkerAfterItsCallerCancels(t *testing.T) {
	const key = "ses_deleted"
	kv := liveBucket(t, "kv_marker_write_cancel", key)
	if err := kv.Delete(key); err != nil {
		t.Fatalf("delete %s: %v", key, err)
	}
	caller, cancel := context.WithCancel(context.Background())
	defer cancel()
	gets := markerGetsOn(t, &kv, false)
	gets.after = cancel
	revision, err := kv.Within(caller).Create(key, []byte("again"))
	if n, given := gets.read(); n != 1 || given == nil {
		t.Fatalf("the bound Create over a delete marker made %d marker reads (context %v), want one with a context, so this row holds nothing", n, given)
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("a bound Create over a delete marker whose caller cancelled after the marker read = revision %d, %v; want context.Canceled", revision, err)
	}
	entry, err := kv.WatchHandle().Get(t.Context(), key)
	if !errors.Is(err, jetstream.ErrKeyNotFound) && !errors.Is(err, jetstream.ErrKeyDeleted) {
		t.Fatalf("read %s after the cancelled Create = %v, %v; want it still deleted: a write went out after the caller cancelled", key, entry, err)
	}
}

// A bound Create on a client with stream message reads whose caller has already cancelled writes
// nothing: its first write must not go out. The Create answers the caller's context.Canceled and
// the key stays absent.
func TestABoundCreateByACancelledCallerWritesNothing(t *testing.T) {
	const key = "ses_fresh"
	kv := liveBucket(t, "kv_create_cancelled_caller", "ses_other")
	caller, cancel := context.WithCancel(context.Background())
	cancel()
	revision, err := kv.Within(caller).Create(key, []byte("fresh"))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("a bound Create by a caller that had cancelled = revision %d, %v; want context.Canceled", revision, err)
	}
	entry, err := kv.WatchHandle().Get(t.Context(), key)
	if !errors.Is(err, jetstream.ErrKeyNotFound) {
		t.Fatalf("read %s after a cancelled caller's Create = %v, %v; want it absent: the first write went out after the caller cancelled", key, entry, err)
	}
}
