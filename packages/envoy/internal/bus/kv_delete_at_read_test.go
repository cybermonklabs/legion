package bus

import (
	"errors"
	"testing"

	"github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/testnats"
)

// writingOnMissingGet stores value under the key whose read answered that the bucket holds none,
// which is what a registration landing between DeleteAtRead's read and its delete does. It writes
// once, so the delete's own read of the bucket, if one is made, answers what it stored.
type writingOnMissingGet struct {
	nats.KeyValue
	value []byte
	wrote bool
	err   error
}

func (kv *writingOnMissingGet) Get(key string) (nats.KeyValueEntry, error) {
	entry, err := kv.KeyValue.Get(key)
	if errors.Is(err, nats.ErrKeyNotFound) && !kv.wrote {
		kv.wrote = true
		_, kv.err = kv.KeyValue.Put(key, kv.value)
	}
	return entry, err
}

// A delete whose read found the key absent deletes nothing, so the row a write stored between
// that read and the delete is kept. A delete at no revision would take it: nats.go sends no
// expected-last-subject-sequence header for revision 0, so that delete carries no fence at all
// and the bucket applies it over whatever it holds - a session's live registration, which the
// delete read none of and judged nothing about.
func TestADeleteAtReadKeepsTheRowWrittenAfterItsReadFoundNone(t *testing.T) {
	client, err := Connect([]string{testnats.URL(t)})
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(client.Close)
	js, err := client.Conn.JetStream()
	if err != nil {
		t.Fatalf("jetstream: %v", err)
	}
	const bucket = "kv_delete_at_read"
	kv, err := EnsureKeyValue(client.Conn, js, &nats.KeyValueConfig{Bucket: bucket, Storage: nats.MemoryStorage})
	if err != nil {
		t.Fatalf("create bucket: %v", err)
	}
	t.Cleanup(func() { _ = js.DeleteKeyValue(bucket) })

	const key = "ses_raced"
	racing := &writingOnMissingGet{KeyValue: kv.Legacy(), value: []byte("registered")}
	revision, err := NewKeyValue(racing, kv.WatchHandle(), kv.Streams()).DeleteAtRead(key)
	if err != nil {
		t.Fatalf("delete %s at the revision its read answered: %v", key, err)
	}
	if racing.err != nil {
		t.Fatalf("the write that landed under the delete's read: %v", racing.err)
	}
	if revision != 0 {
		t.Fatalf("the delete read revision %d, want 0: its read found the key absent", revision)
	}

	entry, err := kv.Get(key)
	if err != nil {
		t.Fatalf("%s after the delete: %v, want the row written under its read kept", key, err)
	}
	if got := string(entry.Value()); got != "registered" {
		t.Fatalf("%s holds %q, want %q", key, got, "registered")
	}
}
