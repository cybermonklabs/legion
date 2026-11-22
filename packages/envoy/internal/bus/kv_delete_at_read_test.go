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
	kv, err := EnsureKeyValue(t.Context(), client.Conn, js, &nats.KeyValueConfig{Bucket: bucket, Storage: nats.MemoryStorage})
	if err != nil {
		t.Fatalf("create bucket: %v", err)
	}
	t.Cleanup(func() { _ = js.DeleteKeyValue(bucket) })

	const key = "ses_raced"
	racing := &writingOnMissingGet{KeyValue: kv.Legacy(), value: []byte("registered")}
	revision, err := NewKeyValue(racing, kv.WatchHandle(), kv.Streams()).DeleteAtRead(t.Context(), key, nil)
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

// writingOnGet stores value under the key once a read of it answered a record, which is what a
// registration landing between DeleteAtRead's read and its delete does over a live record. It
// writes once.
type writingOnGet struct {
	nats.KeyValue
	value []byte
	wrote bool
	err   error
}

func (kv *writingOnGet) Get(key string) (nats.KeyValueEntry, error) {
	entry, err := kv.KeyValue.Get(key)
	if err == nil && !kv.wrote {
		kv.wrote = true
		_, kv.err = kv.KeyValue.Put(key, kv.value)
	}
	return entry, err
}

// A record the server stored at a key as its answer to a direct get of another key's record
// carries that record's Nats-Sequence header ahead of its own, so the revision a direct get
// reports for it is the source's, and a delete fenced there is refused: the control. The delete
// reads such a value again from the stream's leader and deletes it at the sequence it is stored
// at, both where the caller's readable refuses the value (ReadStored's second read) and where it
// takes it (the read after the refused delete finds a message the server wrote), and the source
// keeps its record. A delete refused because a writer stored the key's record between the read and
// the delete still answers that refusal and keeps that writer's record: the leader's answer is no
// message the server wrote.
func TestADeleteAtReadDeletesACopyAtItsTrueSequence(t *testing.T) {
	const bucket = "kv_delete_at_read_copy"
	_, kv := leaderReadBucket(t, bucket)
	conn := kv.Streams().Conn()
	source, err := kv.Put("ses_source", []byte("the source's record"))
	if err != nil {
		t.Fatalf("put ses_source: %v", err)
	}
	refused := testnats.ServerCopiesKey(t, conn, bucket, "ses_copy", bucket, "ses_source")
	reported, err := kv.Get("ses_copy")
	if err != nil || reported.Revision() != source {
		t.Fatalf("a direct get of ses_copy = revision %v (%v), want the source's %d: the control shows nothing to correct", reported, err, source)
	}
	if err := kv.Delete("ses_copy", nats.LastRevision(reported.Revision())); !errors.Is(err, nats.ErrKeyExists) {
		t.Fatalf("a delete of ses_copy fenced on the direct get's revision = %v, want %v", err, nats.ErrKeyExists)
	}
	if stored := testnats.LastSequence(t, conn, bucket, "ses_copy"); stored != refused {
		t.Fatalf("ses_copy's newest message is at %d after the refused delete, want the copy at %d", stored, refused)
	}

	for _, tc := range []struct {
		key      string
		readable func(value []byte) bool
	}{
		{"ses_copy", func(value []byte) bool { return string(value) == "ses_copy's own record" }},
		{"ses_copy_taken", nil},
	} {
		copied := refused
		if tc.key != "ses_copy" {
			copied = testnats.ServerCopiesKey(t, conn, bucket, tc.key, bucket, "ses_source")
		}
		revision, err := kv.DeleteAtRead(t.Context(), tc.key, tc.readable)
		if err != nil || revision != copied {
			t.Fatalf("DeleteAtRead(%s) = revision %d, %v; want the copy deleted at %d, the sequence it is stored at", tc.key, revision, err, copied)
		}
		if _, err := kv.Get(tc.key); !errors.Is(err, nats.ErrKeyNotFound) {
			t.Fatalf("Get(%s) after the delete = %v, want %v", tc.key, err, nats.ErrKeyNotFound)
		}
	}
	if entry, err := kv.Get("ses_source"); err != nil || entry.Revision() != source {
		t.Fatalf("ses_source after the deletes is %v (%v), want its record kept at %d", entry, err, source)
	}

	if _, err := kv.Put("ses_raced_live", []byte("read")); err != nil {
		t.Fatalf("put ses_raced_live: %v", err)
	}
	racing := &writingOnGet{KeyValue: kv.Legacy(), value: []byte("registered since")}
	if _, err := NewKeyValue(racing, kv.WatchHandle(), kv.Streams()).DeleteAtRead(t.Context(), "ses_raced_live", nil); !IsCASConflict(err) || racing.err != nil {
		t.Fatalf("DeleteAtRead(ses_raced_live) under a write = %v (the write: %v), want the refusal of a fence a writer moved", err, racing.err)
	}
	if entry, err := kv.Get("ses_raced_live"); err != nil || string(entry.Value()) != "registered since" {
		t.Fatalf("ses_raced_live after the refused delete is %v (%v), want the record written under the delete's read", entry, err)
	}
}
