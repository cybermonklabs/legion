package bus

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/testnats"
)

// leaderReadBucket opens bucket on a connection of its own and answers the server's URL with it,
// so a row can open a second connection to the same server and read the wire.
func leaderReadBucket(t *testing.T, bucket string) (string, KeyValue) {
	t.Helper()
	uri := testnats.URL(t)
	client, err := Connect([]string{uri})
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(client.Close)
	js, err := client.Conn.JetStream()
	if err != nil {
		t.Fatalf("jetstream: %v", err)
	}
	kv, err := EnsureKeyValue(client.Conn, js, &nats.KeyValueConfig{Bucket: bucket, Storage: nats.MemoryStorage})
	if err != nil {
		t.Fatalf("create bucket %s: %v", bucket, err)
	}
	t.Cleanup(func() { _ = js.DeleteKeyValue(bucket) })
	return uri, kv
}

// requestFor is the next message the subscription holds whose subject names stream, waited for up
// to wait, and nil where none came: the requests of the other rows sharing this server, and of
// this row's own setup, name another stream and are passed over.
func requestFor(t *testing.T, sub *nats.Subscription, stream string, wait time.Duration) *nats.Msg {
	t.Helper()
	deadline := time.Now().Add(wait)
	for {
		left := time.Until(deadline)
		if left <= 0 {
			return nil
		}
		msg, err := sub.NextMsg(left)
		if errors.Is(err, nats.ErrTimeout) {
			return nil
		}
		if err != nil {
			t.Fatalf("read the requests of %s: %v", stream, err)
		}
		if strings.Contains(msg.Subject, stream) {
			return msg
		}
	}
}

// noRequestFor is the quiet wait requestFor's absence takes: long enough for a request already
// sent to reach this subscriber, since the read that would have sent it has already answered.
const noRequestFor = 500 * time.Millisecond

// A Get of a key is a direct get, which nats-server answers from any copy of the bucket's stream
// that is 90% caught up, and the leader-answered read is the stream message get of the bucket's
// own stream, which the stream's leader alone answers. That difference is the whole of what
// GetFromLeader buys, so this row pins which request each read sends, read off the wire from a
// second connection subscribed to both API subjects.
//
// Which request is sent is as close as a single-node server allows: it holds one copy of every
// stream, elects it leader and cannot be made to lag, so no row here can watch a follower answer
// a direct get for a key it has not applied yet. Who answers each request is nats-server's, and
// it turns on nothing but the subject the request was sent to.
func TestTheLeaderReadSendsTheStreamMessageGetWhereGetSendsTheDirectGet(t *testing.T) {
	const bucket = "kv_leader_wire"
	const key = "ses_live"
	stream := "KV_" + bucket
	uri, kv := leaderReadBucket(t, bucket)
	if _, err := kv.Put(key, []byte("live")); err != nil {
		t.Fatalf("put %s: %v", key, err)
	}

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

	if _, err := kv.Get(key); err != nil {
		t.Fatalf("Get(%s): %v", key, err)
	}
	direct := requestFor(t, directGets, stream, 5*time.Second)
	if direct == nil {
		t.Fatalf("Get(%s) sent no direct get of %s; this read is the one the leader need not answer", key, stream)
	}
	if want := "$JS.API.DIRECT.GET." + stream + ".$KV." + bucket + "." + key; direct.Subject != want {
		t.Errorf("Get(%s) sent %s, want %s", key, direct.Subject, want)
	}
	if sent := requestFor(t, streamMsgGets, stream, noRequestFor); sent != nil {
		t.Errorf("Get(%s) sent %s too, want the direct get alone", key, sent.Subject)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := kv.GetFromLeader(ctx, key); err != nil {
		t.Fatalf("GetFromLeader(%s): %v", key, err)
	}
	leader := requestFor(t, streamMsgGets, stream, 5*time.Second)
	if leader == nil {
		t.Fatalf("GetFromLeader(%s) sent no stream message get of %s; a direct get would be answered by a copy that is behind", key, stream)
	}
	if want := "$JS.API.STREAM.MSG.GET." + stream; leader.Subject != want {
		t.Errorf("GetFromLeader(%s) sent %s, want %s", key, leader.Subject, want)
	}
	if want := `"last_by_subj":"$KV.` + bucket + "." + key + `"`; !strings.Contains(string(leader.Data), want) {
		t.Errorf("GetFromLeader(%s) asked %s, want it to carry %s: the newest message on the key's subject", key, leader.Data, want)
	}
	if sent := requestFor(t, directGets, stream, noRequestFor); sent != nil {
		t.Errorf("GetFromLeader(%s) sent %s, want no direct get at all: a copy that is behind answers one", key, sent.Subject)
	}
}

// The leader-answered read speaks Get's vocabulary: a key the bucket holds answers that key's
// value and revision, a key it does not hold and one whose newest message is a delete or a purge
// marker are nats.ErrKeyNotFound, and a key no read may name is refused with the error Get
// refuses it with, word for word, before anything is sent.
func TestTheLeaderReadAnswersAKeyAsGetDoes(t *testing.T) {
	const bucket = "kv_leader_answers"
	_, kv := leaderReadBucket(t, bucket)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	revision, err := kv.Put("ses_live", []byte("live"))
	if err != nil {
		t.Fatalf("put ses_live: %v", err)
	}
	entry, err := kv.GetFromLeader(ctx, "ses_live")
	if err != nil {
		t.Fatalf("GetFromLeader(ses_live): %v", err)
	}
	if entry.Bucket() != bucket || entry.Key() != "ses_live" || string(entry.Value()) != "live" {
		t.Errorf("GetFromLeader(ses_live) = %s/%s = %q, want %s/ses_live = \"live\"", entry.Bucket(), entry.Key(), entry.Value(), bucket)
	}
	if entry.Revision() != revision || entry.Operation() != nats.KeyValuePut || entry.Created().IsZero() {
		t.Errorf("GetFromLeader(ses_live) = revision %d, operation %v, created %s; want revision %d, a put, and the time the stream stored it",
			entry.Revision(), entry.Operation(), entry.Created(), revision)
	}
	got, err := kv.Get("ses_live")
	if err != nil || got.Revision() != entry.Revision() || string(got.Value()) != string(entry.Value()) {
		t.Errorf("Get(ses_live) = revision %d, %q, %v; want the leader read's revision %d and %q", got.Revision(), got.Value(), err, entry.Revision(), entry.Value())
	}

	if _, err := kv.Put("ses_deleted", []byte("gone")); err != nil {
		t.Fatalf("put ses_deleted: %v", err)
	}
	if err := kv.Delete("ses_deleted"); err != nil {
		t.Fatalf("delete ses_deleted: %v", err)
	}
	if _, err := kv.Put("ses_purged", []byte("gone")); err != nil {
		t.Fatalf("put ses_purged: %v", err)
	}
	if err := kv.Purge("ses_purged"); err != nil {
		t.Fatalf("purge ses_purged: %v", err)
	}
	for _, key := range []string{"ses_missing", "ses_deleted", "ses_purged"} {
		if _, err := kv.GetFromLeader(ctx, key); !errors.Is(err, nats.ErrKeyNotFound) {
			t.Errorf("GetFromLeader(%s) = %v, want %v", key, err, nats.ErrKeyNotFound)
		}
		if _, err := kv.Get(key); !errors.Is(err, nats.ErrKeyNotFound) {
			t.Errorf("Get(%s) = %v, want %v: the two answer a key with no value alike", key, err, nats.ErrKeyNotFound)
		}
	}

	_, leaderErr := kv.GetFromLeader(ctx, "ses:bad")
	_, getErr := kv.Get("ses:bad")
	if !errors.Is(leaderErr, ErrInvalidKey) || !errors.Is(leaderErr, ErrRefused) || !errors.Is(leaderErr, nats.ErrInvalidKey) {
		t.Errorf("GetFromLeader(ses:bad) = %v, want an ErrRefused naming ErrInvalidKey and nats.ErrInvalidKey", leaderErr)
	}
	if getErr == nil || leaderErr == nil || leaderErr.Error() != getErr.Error() {
		t.Errorf("GetFromLeader(ses:bad) = %v, want the refusal Get answers, %v", leaderErr, getErr)
	}
}
