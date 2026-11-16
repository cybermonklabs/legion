package kvwatch_test

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/kvwatch"
	"github.com/sjawhar/envoy/internal/testnats"
)

// stamped is a record whose stamp orders it, as a session's registration time orders sessions.
type stamped struct {
	At    int64  `json:"at"`
	Value string `json:"value"`
}

func stampedRecord(t *testing.T, at int64, value string) []byte {
	t.Helper()
	buf, err := json.Marshal(stamped{At: at, Value: value})
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	return buf
}

// standsAt is a write-back's stands for stamped records: the bucket's record stands when it was
// stamped at the held one's time or later.
func standsAt(t *testing.T) func(stored []byte, held kvwatch.Held[int64]) bool {
	return func(stored []byte, held kvwatch.Held[int64]) bool {
		var s stamped
		return json.Unmarshal(stored, &s) == nil && s.At >= held.Value
	}
}

func valueOf(t *testing.T, kv bus.KeyValue, key string) (string, bool) {
	t.Helper()
	entry, err := kv.Get(key)
	if errors.Is(err, natsgo.ErrKeyNotFound) {
		return "", false
	}
	if err != nil {
		t.Fatalf("read %s: %v", key, err)
	}
	var s stamped
	if err := json.Unmarshal(entry.Value(), &s); err != nil {
		t.Fatalf("decode %s: %v", key, err)
	}
	return s.Value, true
}

// A write-back writes each record byte for byte where the bucket holds nothing for its key, writes
// over a record stamped before it, and leaves standing a record stamped as late or later and a
// delete marker: a lagged copy never leaves an older value, and never undoes a delete or a newer
// write. A record whose expiry has passed is left out. It answers the counts and logs one line.
func TestAWriteBackWritesOverOnlyAnOlderRecord(t *testing.T) {
	_, kv := bucket(t, testnats.URL(t))
	if _, err := kv.Put("older", stampedRecord(t, 1, "older in the bucket")); err != nil {
		t.Fatalf("put older: %v", err)
	}
	if _, err := kv.Put("newer", stampedRecord(t, 9, "newer in the bucket")); err != nil {
		t.Fatalf("put newer: %v", err)
	}
	if _, err := kv.Put("deleted", stampedRecord(t, 1, "deleted")); err != nil {
		t.Fatalf("put deleted: %v", err)
	}
	if err := kv.Delete("deleted"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	w := kvwatch.New("test cache", kv, newSeen())
	t.Cleanup(w.Stop)
	held := []kvwatch.Held[int64]{
		{Key: "absent", Record: stampedRecord(t, 5, "held"), Value: 5},
		{Key: "older", Record: stampedRecord(t, 5, "held"), Value: 5},
		{Key: "newer", Record: stampedRecord(t, 5, "held"), Value: 5},
		{Key: "deleted", Record: stampedRecord(t, 5, "held"), Value: 5},
		{Key: "expired", Record: stampedRecord(t, 5, "held"), Value: 5, Expires: time.Now().Add(-time.Second)},
	}
	var mu sync.Mutex
	var took []string
	result, failed := kvwatch.Reassert(w, kvwatch.Read{Epoch: 0, Recreated: true}, held, standsAt(t),
		func(h kvwatch.Held[int64], _ uint64) { mu.Lock(); took = append(took, h.Key); mu.Unlock() })
	if result.Written != 2 || result.Present != 2 || result.Expired != 1 || result.Failed != 0 || len(failed) != 0 {
		t.Fatalf("write-back = %+v, %d failed; want 2 written (absent, older), 2 standing (newer, deleted), 1 expired", result, len(failed))
	}
	for key, want := range map[string]string{"absent": "held", "older": "held", "newer": "newer in the bucket"} {
		if got, ok := valueOf(t, kv, key); !ok || got != want {
			t.Fatalf("%s = %q (present %v), want %q", key, got, ok, want)
		}
	}
	for _, key := range []string{"deleted", "expired"} {
		if _, ok := valueOf(t, kv, key); ok {
			t.Fatalf("%s holds a record after the write-back, want none", key)
		}
	}
	if len(took) != 2 {
		t.Fatalf("took %v, want the two records written", took)
	}
}

// A write-back that could not write a record keeps it for the next read of the same epoch, which
// writes it (Reasserter): here the bucket is gone while the first read writes back, and back for
// the second. A read of another epoch drops what an earlier epoch left.
func TestAWriteBackThatFailedIsWrittenAtTheNextReadOfItsEpoch(t *testing.T) {
	conn, kv := bucket(t, testnats.URL(t))
	w := kvwatch.New("test cache", kv, newSeen())
	t.Cleanup(w.Stop)
	js := jetStream(t, conn)
	deleteTestBucket(t, js)

	var p kvwatch.Reasserter[int64]
	first := kvwatch.Read{Epoch: 0, Recreated: true}
	due := p.Due(first, func() []kvwatch.Held[int64] {
		return []kvwatch.Held[int64]{{Key: "ses_a", Record: stampedRecord(t, 1, "held"), Value: 1}}
	})
	if result := p.Write(w, first, due, standsAt(t), func(kvwatch.Held[int64], uint64) {}); result.Failed != 1 {
		t.Fatalf("the write-back with no bucket = %+v, want 1 failed", result)
	}

	testnats.RecreateKeyValue(t, js, &natsgo.KeyValueConfig{Bucket: testBucketName})
	again := kvwatch.Read{Epoch: 0}
	due = p.Due(again, func() []kvwatch.Held[int64] {
		t.Fatal("a later read of the epoch asked for every owned record")
		return nil
	})
	if len(due) != 1 {
		t.Fatalf("due at the next read of the epoch = %d records, want the 1 that failed", len(due))
	}
	if result := p.Write(w, again, due, standsAt(t), func(kvwatch.Held[int64], uint64) {}); result.Written != 1 {
		t.Fatalf("the write-back at the next read = %+v, want 1 written", result)
	}
	if got, ok := valueOf(t, kv, "ses_a"); !ok || got != "held" {
		t.Fatalf("ses_a = %q (present %v) after the second read, want the held record", got, ok)
	}
	if due := p.Due(kvwatch.Read{Epoch: 0}, nil); len(due) != 0 {
		t.Fatalf("due after the record was written = %d, want none", len(due))
	}
}

// Where the new stream allows per-message TTLs, a written-back record carries what remained of its
// expiry, whole seconds rounded up, so it leaves the bucket when it would have; the control is the
// same write-back into a stream that does not allow them, which carries none and keeps the record.
func TestAWriteBackKeepsTheRecordsExpiryWhereTheStreamAllowsIt(t *testing.T) {
	for _, allows := range []bool{true, false} {
		name := "stream allows per-message TTLs"
		if !allows {
			name = "control: stream does not"
		}
		t.Run(name, func(t *testing.T) {
			conn := testnats.Connect(t, testnats.URL(t))
			t.Cleanup(conn.Close)
			js, err := jetstream.New(conn)
			if err != nil {
				t.Fatalf("jetstream: %v", err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			config := jetstream.KeyValueConfig{Bucket: "kvwatch-ttl", TTL: time.Hour}
			if allows {
				config.LimitMarkerTTL = time.Second
			}
			if _, err := js.CreateKeyValue(ctx, config); err != nil {
				t.Fatalf("create the bucket: %v", err)
			}
			legacy, err := conn.JetStream()
			if err != nil {
				t.Fatalf("legacy jetstream: %v", err)
			}
			kv, err := bus.OpenKeyValue(conn, legacy, "kvwatch-ttl")
			if err != nil {
				t.Fatalf("open the bucket: %v", err)
			}
			w := kvwatch.New("test cache", kv, newSeen())
			t.Cleanup(w.Stop)
			held := []kvwatch.Held[int64]{{Key: "ses_a", Record: stampedRecord(t, 1, "held"), Value: 1, Expires: time.Now().Add(1500 * time.Millisecond)}}
			if result, _ := kvwatch.Reassert(w, kvwatch.Read{Epoch: 0, Recreated: true}, held, standsAt(t), func(kvwatch.Held[int64], uint64) {}); result.Written != 1 {
				t.Fatalf("write-back = %+v, want 1 written", result)
			}
			stream, err := js.Stream(ctx, "KV_kvwatch-ttl")
			if err != nil {
				t.Fatalf("read the stream: %v", err)
			}
			msg, err := stream.GetLastMsgForSubject(ctx, "$KV.kvwatch-ttl.ses_a")
			if err != nil {
				t.Fatalf("read ses_a's message: %v", err)
			}
			ttl := msg.Header.Get("Nats-TTL")
			if allows && ttl != "2" {
				t.Fatalf("Nats-TTL = %q, want 2: 1.5 s of expiry left, rounded up to whole seconds", ttl)
			}
			if !allows && ttl != "" {
				t.Fatalf("Nats-TTL = %q on a stream that does not allow it, want none", ttl)
			}
			if !allows {
				time.Sleep(4 * time.Second)
				if _, present := valueOf(t, kv, "ses_a"); !present {
					t.Fatal("ses_a expired on a stream that does not allow per-message TTLs")
				}
				return
			}
			// The record expires at its TTL: the stream removes it and, as this bucket keeps
			// markers (LimitMarkerTTL), leaves a marker naming why in its place.
			deadline := time.Now().Add(10 * time.Second)
			for {
				msg, err := stream.GetLastMsgForSubject(ctx, "$KV.kvwatch-ttl.ses_a")
				if errors.Is(err, jetstream.ErrMsgNotFound) || (err == nil && msg.Header.Get("Nats-Marker-Reason") != "") {
					return
				}
				if time.Now().After(deadline) {
					t.Fatalf("ses_a still in the bucket 10 s after the write-back (%v), want it expired at its TTL", err)
				}
				time.Sleep(100 * time.Millisecond)
			}
		})
	}
}
