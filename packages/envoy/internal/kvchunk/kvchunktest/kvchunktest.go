// Package kvchunktest is what the tests of a store that lays its records out in chunks
// (internal/kvchunk) share: a NATS server with a small max payload, a reading of the chunks a
// bucket holds, and the bucket handles that stop a write at one of its steps.
package kvchunktest

import (
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/kvchunk"
	"github.com/sjawhar/envoy/internal/testnats"
)

// MaxPayload is the max payload of the server SmallPayloadNATS starts: far under the 1 MiB
// default, so a record of a few hundred kilobytes is past it, and the chunk size a writer reads
// from the connection is this server's and no constant's.
const MaxPayload = 64 << 10

// SmallPayloadNATS starts a NATS server of the test's own, with JetStream, whose max payload is
// MaxPayload, and connects to it.
func SmallPayloadNATS(t *testing.T) *natsgo.Conn {
	t.Helper()
	_, uri := testnats.StartConfig(t, fmt.Sprintf("max_payload: %d\njetstream {}\n", MaxPayload), []string{"-js"}, true)
	conn := testnats.Connect(t, uri)
	t.Cleanup(conn.Close)
	if got := conn.MaxPayload(); got != MaxPayload {
		t.Fatalf("the server's max payload is %d bytes, want %d", got, MaxPayload)
	}
	return conn
}

// Topics is the wildcard `notifications.team.<label>.>` and count topics of about 300 bytes each
// under it, so 220 of them are past MaxPayload as a record. A registration that outgrows one
// message carries such a wildcard: one whose topics no wildcard covers has a session consumer
// filter past one message too, which the listener refuses, and a record of it has no view an
// earlier build can read in one message (store.ChunkedRecordView), which the store refuses.
func Topics(label string, count int) []string {
	topics := make([]string, 0, count+1)
	topics = append(topics, fmt.Sprintf("notifications.team.%s.>", label))
	for i := range count {
		topics = append(topics, fmt.Sprintf("notifications.team.%s.%04d-%s", label, i, strings.Repeat("x", 270)))
	}
	return topics
}

// LiveChunkKeys is the key of every chunk kv holds a value for, sorted, read by a scan of the
// bucket that names no key, so a chunk no record names is listed too.
func LiveChunkKeys(t *testing.T, kv natsgo.KeyValue) []string {
	t.Helper()
	var keys []string
	for key, op := range LiveKeys(t, kv) {
		if op == natsgo.KeyValuePut && kvchunk.IsChunkKey(key) {
			keys = append(keys, key)
		}
	}
	slices.Sort(keys)
	return keys
}

// LiveKeys reads every key kv still holds a message for, and what that message is.
func LiveKeys(t *testing.T, kv natsgo.KeyValue) map[string]natsgo.KeyValueOp {
	t.Helper()
	watcher, err := kv.Watch(natsgo.AllKeys, natsgo.MetaOnly())
	if err != nil {
		t.Fatalf("watch the bucket: %v", err)
	}
	defer func() { _ = watcher.Stop() }()
	ops := map[string]natsgo.KeyValueOp{}
	for entry := range watcher.Updates() {
		if entry == nil {
			return ops
		}
		ops[entry.Key()] = entry.Operation()
	}
	t.Fatal("the bucket's scan ended before it had delivered every key")
	return nil
}

// LeaderReadAnswer is the size of the answer the stream's leader sends to a read of key from bucket
// (STREAM.MSG.GET by last subject, the read bus.KeyValue.GetFromLeader makes): JSON carrying the
// value in base64, which a server enforcing its max payload on that hop - a leafnode connection
// does - takes only when it fits.
func LeaderReadAnswer(t *testing.T, conn *natsgo.Conn, bucket, key string) int {
	t.Helper()
	request := fmt.Sprintf(`{"last_by_subj":"$KV.%s.%s"}`, bucket, key)
	answer, err := conn.Request("$JS.API.STREAM.MSG.GET.KV_"+bucket, []byte(request), 10*time.Second)
	if err != nil {
		t.Fatalf("read %s from the stream's leader: %v", key, err)
	}
	if !strings.Contains(string(answer.Data), `"data":`) {
		t.Fatalf("the leader's answer to a read of %s carries no value: %.200s", key, answer.Data)
	}
	return len(answer.Data)
}

// Generations is the distinct generations chunk keys name.
func Generations(keys []string) []string {
	var generations []string
	for _, key := range keys {
		generation, _, ok := strings.Cut(strings.TrimPrefix(key, kvchunk.Prefix), ".")
		if ok && !slices.Contains(generations, generation) {
			generations = append(generations, generation)
		}
	}
	return generations
}

// HeadFailing is a bucket handle whose writes of one key fail as a connection lost under them
// does, so a write of that key's record stops between its chunks and its head: the crash of a
// writer at that step, which stores nothing more.
type HeadFailing struct {
	natsgo.KeyValue
	Key string
}

func (kv *HeadFailing) Put(key string, value []byte) (uint64, error) {
	if key == kv.Key {
		return 0, natsgo.ErrConnectionClosed
	}
	return kv.KeyValue.Put(key, value)
}

func (kv *HeadFailing) Create(key string, value []byte) (uint64, error) {
	if key == kv.Key {
		return 0, natsgo.ErrConnectionClosed
	}
	return kv.KeyValue.Create(key, value)
}

func (kv *HeadFailing) Update(key string, value []byte, revision uint64) (uint64, error) {
	if key == kv.Key {
		return 0, natsgo.ErrConnectionClosed
	}
	return kv.KeyValue.Update(key, value, revision)
}

// ChunkFailing is a bucket handle whose write of the FailAt-th chunk and every chunk after it fails
// as a connection lost under them does, a crash between two chunks, when FailAt is above 0. Before
// each chunk write it runs Before, when set, which a case uses to move the store's clock.
type ChunkFailing struct {
	natsgo.KeyValue
	Before  func()
	FailAt  int
	written int
}

func (kv *ChunkFailing) Create(key string, value []byte) (uint64, error) {
	if kvchunk.IsChunkKey(key) {
		kv.written++
		if kv.Before != nil {
			kv.Before()
		}
		if kv.FailAt > 0 && kv.written >= kv.FailAt {
			return 0, natsgo.ErrConnectionClosed
		}
	}
	return kv.KeyValue.Create(key, value)
}
