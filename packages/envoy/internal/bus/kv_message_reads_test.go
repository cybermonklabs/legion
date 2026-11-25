package bus

import (
	"errors"
	"testing"

	"github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/testnats"
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
