package bus

import (
	"errors"
	"testing"

	"github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/testnats"
)

// writingOnDelete stores value under the key as the first delete of it reaches the bucket, and
// then sends that delete: what a registration landing between DeleteAtRead's read and its delete
// does. It writes once, and counts the deletes it was handed.
type writingOnDelete struct {
	nats.KeyValue
	value   []byte
	deletes int
	err     error
}

func (kv *writingOnDelete) Delete(key string, opts ...nats.DeleteOpt) error {
	kv.deletes++
	if kv.deletes == 1 {
		_, kv.err = kv.KeyValue.Put(key, kv.value)
	}
	return kv.KeyValue.Delete(key, opts...)
}

// A delete whose read found the key absent sends no delete, so the row a write stores between that
// read and the delete is kept. A delete at no revision would take it: nats.go sends no
// expected-last-subject-sequence header for revision 0, so that delete carries no fence at all
// and the bucket applies it over whatever it holds - a session's live registration, which the
// delete read none of and judged nothing about. The handle writes the row as any delete reaches
// it, so a delete at no revision would take that row and the row would fail.
func TestADeleteAtReadKeepsTheRowWrittenAfterItsReadFoundNone(t *testing.T) {
	t.Parallel()
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
	racing := &writingOnDelete{KeyValue: kv.Legacy(), value: []byte("registered")}
	revision, err := NewKeyValue(racing, kv.WatchHandle(), kv.Streams()).DeleteAtRead(t.Context(), key)
	if err != nil {
		t.Fatalf("delete %s at the revision its read answered: %v", key, err)
	}
	if racing.deletes != 0 {
		t.Fatalf("the delete sent %d deletes of %s, want none: its read found the key absent", racing.deletes, key)
	}
	if revision != 0 {
		t.Fatalf("the delete read revision %d, want 0: its read found the key absent", revision)
	}
	if _, err := kv.Put(key, []byte("registered")); err != nil {
		t.Fatalf("register %s after the delete's read: %v", key, err)
	}
	entry, err := kv.Get(key)
	if err != nil {
		t.Fatalf("%s after the delete: %v, want the row written after its read kept", key, err)
	}
	if got := string(entry.Value()); got != "registered" {
		t.Fatalf("%s holds %q, want %q", key, got, "registered")
	}
}

// absentCopyKV is a bucket whose direct get of key answers that the bucket holds none, as a copy
// of a replicated bucket that has not applied the key's newest write answers; it counts those
// reads. Every other call is the bucket's own, and a read from the stream's leader does not go
// through this handle.
type absentCopyKV struct {
	nats.KeyValue
	key   string
	reads int
}

func (kv *absentCopyKV) Get(key string) (nats.KeyValueEntry, error) {
	if key == kv.key {
		kv.reads++
		return nil, nats.ErrKeyNotFound
	}
	return kv.KeyValue.Get(key)
}

// A delete takes the record the bucket holds even where a copy of the bucket that has not applied
// that record answers the key's direct get: its read is the stream leader's. On a three-copy
// bucket a direct get is answered by any copy near enough to current, and a stop made just after
// a registration, answered "absent" by such a copy, deleted nothing; the session's record (or its
// interest) then stayed until it expired. Control: over a key the bucket holds no record for, the
// same delete deletes nothing and answers revision 0, so the row fails only for the record kept.
func TestADeleteAtReadTakesTheRecordACopyBehindAnswersAbsent(t *testing.T) {
	const bucket = "kv_delete_at_read_behind"
	_, kv := leaderReadBucket(t, bucket)
	for _, tc := range []struct {
		name string
		held bool
	}{
		{"a record the copy behind answers absent", true},
		{"control: a key the bucket holds no record for", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			key := "ses_behind_" + map[bool]string{true: "held", false: "none"}[tc.held]
			var stored uint64
			if tc.held {
				var err error
				if stored, err = kv.Put(key, []byte("registered")); err != nil {
					t.Fatalf("register %s: %v", key, err)
				}
			}
			behind := &absentCopyKV{KeyValue: kv.Legacy(), key: key}
			if _, err := NewKeyValue(behind, kv.WatchHandle(), kv.Streams()).Get(key); !errors.Is(err, nats.ErrKeyNotFound) || behind.reads != 1 {
				t.Fatalf("a direct get of %s through the copy behind = %v after %d reads, want %v: the copy answers nothing to correct", key, err, behind.reads, nats.ErrKeyNotFound)
			}
			revision, err := NewKeyValue(behind, kv.WatchHandle(), kv.Streams()).DeleteAtRead(t.Context(), key)
			if err != nil || revision != stored {
				t.Fatalf("DeleteAtRead(%s) = revision %d, %v; want revision %d", key, revision, err, stored)
			}
			if _, err := kv.Get(key); !errors.Is(err, nats.ErrKeyNotFound) {
				t.Fatalf("Get(%s) after the delete = %v, want %v: the record the copy behind answered absent was kept", key, err, nats.ErrKeyNotFound)
			}
		})
	}
}

// A record the server stored at a key as its answer to a direct get of another key's record
// carries that record's Nats-Sequence header ahead of its own, so the revision a direct get
// reports for it is the source's, and a delete fenced there is refused: the control. The delete
// reads the key from the stream's leader, which reports the sequence the copy is stored at, and
// deletes it there, and the source keeps its record. A delete refused because a writer stored the
// key's record between the read and the delete still answers that refusal and keeps that writer's
// record: the leader's answer is no message the server wrote.
func TestADeleteAtReadDeletesACopyAtItsTrueSequence(t *testing.T) {
	t.Parallel()
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

	for _, key := range []string{"ses_copy", "ses_copy_taken"} {
		copied := refused
		if key != "ses_copy" {
			copied = testnats.ServerCopiesKey(t, conn, bucket, key, bucket, "ses_source")
		}
		revision, err := kv.DeleteAtRead(t.Context(), key)
		if err != nil || revision != copied {
			t.Fatalf("DeleteAtRead(%s) = revision %d, %v; want the copy deleted at %d, the sequence it is stored at", key, revision, err, copied)
		}
		if _, err := kv.Get(key); !errors.Is(err, nats.ErrKeyNotFound) {
			t.Fatalf("Get(%s) after the delete = %v, want %v", key, err, nats.ErrKeyNotFound)
		}
	}
	if entry, err := kv.Get("ses_source"); err != nil || entry.Revision() != source {
		t.Fatalf("ses_source after the deletes is %v (%v), want its record kept at %d", entry, err, source)
	}

	if _, err := kv.Put("ses_raced_live", []byte("read")); err != nil {
		t.Fatalf("put ses_raced_live: %v", err)
	}
	racing := &writingOnDelete{KeyValue: kv.Legacy(), value: []byte("registered since")}
	if _, err := NewKeyValue(racing, kv.WatchHandle(), kv.Streams()).DeleteAtRead(t.Context(), "ses_raced_live"); !IsCASConflict(err) || racing.err != nil {
		t.Fatalf("DeleteAtRead(ses_raced_live) under a write = %v (the write: %v), want the refusal of a fence a writer moved", err, racing.err)
	}
	if entry, err := kv.Get("ses_raced_live"); err != nil || string(entry.Value()) != "registered since" {
		t.Fatalf("ses_raced_live after the refused delete is %v (%v), want the record written under the delete's read", entry, err)
	}
}
