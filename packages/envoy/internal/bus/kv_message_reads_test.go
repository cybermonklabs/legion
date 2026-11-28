package bus

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/nats-io/nats.go"

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
	// The watcher's direct-get subscription sees this mark. It names no stream, so
	// nothing answers it.
	const directGetMark = "$JS.API.DIRECT.GET.MARK_" + bucket
	stream := "KV_" + bucket
	uri := testnats.URL(t)
	client, err := Connect([]string{uri}, WithStreamMessageReads())
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(client.Close)
	js, err := client.Conn.JetStream(testnats.MaxWait(t))
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
	if err := watcher.Conn.FlushWithContext(testnats.Context(t)); err != nil {
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

	// The same connection sends the mark after its last read, so any direct get it sent
	// reaches the watcher before the mark.
	requests := testwait.Context(t)
	if err := client.Conn.Publish(directGetMark, nil); err != nil {
		t.Fatalf("publish the mark after the reads: %v", err)
	}
	for {
		sent := nextRequest(t, requests, directGets, "the mark the client published after its reads")
		if sent.Subject == directGetMark {
			break
		}
		if strings.Contains(sent.Subject, stream) {
			t.Fatalf("the client that reads with stream message gets sent the direct get %s", sent.Subject)
		}
	}
	awaitRequestOf(t, requests, streamMsgGets, stream, "a stream message get of "+stream+" from the client that reads with stream message gets")

	other, err := watcher.Conn.JetStream(testnats.MaxWait(t))
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
	awaitRequestOf(t, requests, directGets, stream, "the direct get of the plain client's Get("+key+"), which checks that the watcher can see one")
}

// nextRequest fails the test with what it was waiting for if the subscription read fails.
func nextRequest(t *testing.T, ctx context.Context, sub *nats.Subscription, what string) *nats.Msg {
	t.Helper()
	msg, err := sub.NextMsgWithContext(ctx)
	if err != nil {
		t.Fatalf("wait for %s: %v", what, err)
	}
	return msg
}

// awaitRequestOf passes over requests for other streams, including those from setup.
func awaitRequestOf(t *testing.T, ctx context.Context, sub *nats.Subscription, stream, what string) {
	t.Helper()
	for {
		if strings.Contains(nextRequest(t, ctx, sub, what).Subject, stream) {
			return
		}
	}
}
