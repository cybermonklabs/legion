package session

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/kvchunk"
	"github.com/sjawhar/envoy/internal/kvchunk/kvchunktest"
	"github.com/sjawhar/envoy/internal/testwait"
)

// openKnownOn opens the known-session store on conn with the clock now and waits for its cache.
func openKnownOn(t *testing.T, conn *nats.Conn, now func() time.Time) *KnownSessions {
	t.Helper()
	known, err := OpenKnownSessions(conn, WithKnownSessionReplicas(1), WithKnownSessionClock(now))
	if err != nil {
		t.Fatalf("open the known-session store: %v", err)
	}
	t.Cleanup(known.StopWatch)
	if err := known.WaitForCacheReady(context.Background()); err != nil {
		t.Fatalf("the known-session cache did not become ready: %v", err)
	}
	return known
}

// rawKnown is the known-session bucket on conn as a raw handle, which reads what is stored with no
// reassembly of its own.
func rawKnown(t *testing.T, conn *nats.Conn) nats.KeyValue {
	t.Helper()
	js, err := conn.JetStream()
	if err != nil {
		t.Fatalf("jetstream: %v", err)
	}
	kv, err := js.KeyValue(bus.KnownSessionBucket)
	if err != nil {
		t.Fatalf("open the known-session bucket: %v", err)
	}
	return kv
}

// useKnownKV rebuilds the store's watcher over kv, a wrapper of its bucket handle that watches
// through the handle's own jetstream one, so the store writes its records through kv from here on.
// The replaced watcher is stopped first.
func useKnownKV(t *testing.T, known *KnownSessions, kv nats.KeyValue) {
	t.Helper()
	base := known.watch()
	base.Stop()
	known.startWatch(bus.NewKeyValue(kv, base.KV().WatchHandle(), base.KV().Streams()))
	if err := known.WaitForCacheReady(context.Background()); err != nil {
		t.Fatalf("wait for the rebuilt watcher: %v", err)
	}
}

// requireKnownTopics fails the test unless known answers sessionID's record with exactly want.
func requireKnownTopics(t *testing.T, what string, known *KnownSessions, sessionID string, want []string) {
	t.Helper()
	record, ok := known.Get(sessionID)
	if !ok {
		t.Fatalf("%s answers no record for %s", what, sessionID)
	}
	if !slices.Equal(record.Topics, want) {
		t.Fatalf("%s answers %s's record with %d topics, want the %d written", what, sessionID, len(record.Topics), len(want))
	}
}

// A known-session record whose encoding is past the server's max payload is stored in chunks a
// manifest under the session's id names, each under that max payload, and the record is answered
// whole by id and by topic, by the store that wrote it and by another listener's store, which its
// watcher fills from the bucket alone. Before this layout the write was refused for its size and
// the session had no record at all.
func TestAKnownSessionRecordPastTheMaxPayloadIsStoredInChunksAndAnsweredWhole(t *testing.T) {
	conn := kvchunktest.SmallPayloadNATS(t)
	known := openKnownOn(t, conn, time.Now)
	topics := kvchunktest.Topics("wide", 600)

	if err := known.RecordWithin(t.Context(), KnownSession{SessionID: "ses_known_wide", Title: "wide", Topics: topics}); err != nil {
		t.Fatalf("record a known session of %d topics past the max payload: %v", len(topics), err)
	}
	requireKnownTopics(t, "the writer's store", known, "ses_known_wide", topics)
	other := openKnownOn(t, conn, time.Now)
	requireKnownTopics(t, "another listener's store", other, "ses_known_wide", topics)
	if got := matchedIDs(other, topics[len(topics)-1]); !slices.Equal(got, []string{"ses_known_wide"}) {
		t.Fatalf("another listener's store matches %v for the record's last topic, want ses_known_wide alone", got)
	}

	kv := rawKnown(t, conn)
	head, err := kv.Get("ses_known_wide")
	if err != nil {
		t.Fatalf("read the session's key: %v", err)
	}
	if !kvchunk.IsManifest(head.Value()) {
		t.Fatalf("the session's key holds %.60q, want a manifest", head.Value())
	}
	chunks := kvchunktest.LiveChunkKeys(t, kv)
	if len(chunks) < 3 {
		t.Fatalf("the bucket holds %d chunks, want the record in 3 or more", len(chunks))
	}
	for _, key := range chunks {
		chunk, err := kv.Get(key)
		if err != nil {
			t.Fatalf("read chunk %s: %v", key, err)
		}
		if len(chunk.Value()) > kvchunktest.MaxPayload {
			t.Fatalf("chunk %s holds %d bytes, past the server's max payload of %d", key, len(chunk.Value()), kvchunktest.MaxPayload)
		}
	}
}

// The control: a known-session record under the max payload is stored whole under the session's
// id, as every earlier build stores it, with no chunk.
func TestAKnownSessionRecordUnderTheMaxPayloadIsStoredWholeAsBefore(t *testing.T) {
	conn := kvchunktest.SmallPayloadNATS(t)
	known := openKnownOn(t, conn, time.Now)
	topics := kvchunktest.Topics("narrow", 10)
	if err := known.RecordWithin(t.Context(), KnownSession{SessionID: "ses_known_narrow", Topics: topics}); err != nil {
		t.Fatalf("record a known session of %d topics: %v", len(topics), err)
	}
	kv := rawKnown(t, conn)
	head, err := kv.Get("ses_known_narrow")
	if err != nil {
		t.Fatalf("read the session's key: %v", err)
	}
	var stored KnownSession
	if err := json.Unmarshal(head.Value(), &stored); err != nil || !slices.Equal(stored.Topics, topics) {
		t.Fatalf("the session's key holds %.60q (%v), want the record whole as JSON", head.Value(), err)
	}
	if chunks := kvchunktest.LiveChunkKeys(t, kv); len(chunks) != 0 {
		t.Fatalf("the bucket holds chunks %v for a record that fits one message", chunks)
	}
}

// A rewrite that makes a chunked known-session record fit one message stores it whole and deletes
// every chunk it had, and a rewrite that grows it again stores a new set of chunks and deletes the
// old set, so no write leaves a chunk no record names.
func TestAKnownSessionRewriteThatShrinksOrGrowsLeavesNoStaleChunk(t *testing.T) {
	conn := kvchunktest.SmallPayloadNATS(t)
	known := openKnownOn(t, conn, time.Now)
	kv := rawKnown(t, conn)
	wide, narrow := kvchunktest.Topics("wide", 400), kvchunktest.Topics("narrow", 10)

	if err := known.RecordWithin(t.Context(), KnownSession{SessionID: "ses_known_shrink", Topics: wide}); err != nil {
		t.Fatalf("record the chunked record: %v", err)
	}
	if len(kvchunktest.LiveChunkKeys(t, kv)) == 0 {
		t.Fatal("the record past the max payload was stored with no chunk")
	}
	if err := known.RecordWithin(t.Context(), KnownSession{SessionID: "ses_known_shrink", Topics: narrow}); err != nil {
		t.Fatalf("shrink the record: %v", err)
	}
	if chunks := kvchunktest.LiveChunkKeys(t, kv); len(chunks) != 0 {
		t.Fatalf("after the record shrank to one message the bucket still holds chunks %v", chunks)
	}
	requireKnownTopics(t, "the store after the shrink", known, "ses_known_shrink", narrow)

	if err := known.RecordWithin(t.Context(), KnownSession{SessionID: "ses_known_shrink", Topics: wide}); err != nil {
		t.Fatalf("grow the record again: %v", err)
	}
	grown := kvchunktest.LiveChunkKeys(t, kv)
	more := kvchunktest.Topics("more", 500)
	if err := known.RecordWithin(t.Context(), KnownSession{SessionID: "ses_known_shrink", Topics: more}); err != nil {
		t.Fatalf("grow the record further: %v", err)
	}
	further := kvchunktest.LiveChunkKeys(t, kv)
	for _, key := range grown {
		if slices.Contains(further, key) {
			t.Fatalf("after the record grew again the bucket still holds %s, a chunk of the record it replaced", key)
		}
	}
	if generations := kvchunktest.Generations(further); len(generations) != 1 {
		t.Fatalf("the bucket holds chunks of %d records %v, want the one stored", len(generations), generations)
	}
	requireKnownTopics(t, "the store after the growth", known, "ses_known_shrink", more)
}

// knownClock is a store clock (WithKnownSessionClock) the test sets by hand, read by the store's
// own goroutines too.
type knownClock struct {
	mu sync.Mutex
	at time.Time
}

func (c *knownClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.at
}

func (c *knownClock) set(at time.Time) {
	c.mu.Lock()
	c.at = at
	c.mu.Unlock()
}

// A known-session write that stops at any step - between two chunks, between its chunks and its
// manifest, or after chunk writes that took past the write's bound - leaves the record it would
// have replaced answered whole, by the store and by another listener's store. What it left behind
// is chunks no record names, which the store's sweep collects once they are older than the
// collection grace and not before, and which no collection judges while the cache does not follow
// its bucket.
func TestAKnownSessionWriteStoppedAtAnyStepLeavesTheOldRecordAndItsChunksCollected(t *testing.T) {
	conn := kvchunktest.SmallPayloadNATS(t)
	clock := &knownClock{at: time.Now()}
	known := openKnownOn(t, conn, clock.Now)
	other := openKnownOn(t, conn, time.Now)
	kv := rawKnown(t, conn)
	legacy := known.watch().KV().Legacy()
	old := kvchunktest.Topics("old", 400)
	if err := known.RecordWithin(t.Context(), KnownSession{SessionID: "ses_known_crashed", Topics: old}); err != nil {
		t.Fatalf("record the old record: %v", err)
	}
	served := kvchunktest.LiveChunkKeys(t, kv)

	for _, stopped := range []struct {
		step, label string
		kv          nats.KeyValue
	}{
		{"between two chunks", "chunks", &kvchunktest.ChunkFailing{KeyValue: legacy, FailAt: 2}},
		{"between its chunks and its manifest", "manifest", &kvchunktest.HeadFailing{KeyValue: legacy, Key: "ses_known_crashed"}},
		{"after its chunks took past the write's bound", "bound", &kvchunktest.ChunkFailing{KeyValue: legacy, Before: func() {
			clock.set(clock.Now().Add(kvchunk.WriteBound))
		}}},
	} {
		useKnownKV(t, known, stopped.kv)
		err := known.RecordWithin(t.Context(), KnownSession{SessionID: "ses_known_crashed", Topics: kvchunktest.Topics(stopped.label, 300)})
		if err == nil {
			t.Fatalf("a write stopped %s answered success", stopped.step)
		}
		t.Logf("a write stopped %s answered: %v", stopped.step, err)
		// The clock the cutoff reads moved with the bound case; the retention reads it too.
		clock.set(time.Now())
		requireKnownTopics(t, "the store after a write stopped "+stopped.step, known, "ses_known_crashed", old)
		testwait.WithinPolling(t, 10*time.Second, testwait.Poll, "another listener's store to answer the old record", func() bool {
			record, ok := other.Get("ses_known_crashed")
			return ok && slices.Equal(record.Topics, old)
		})
	}
	useKnownKV(t, known, legacy)
	orphans := slices.DeleteFunc(kvchunktest.LiveChunkKeys(t, kv), func(key string) bool { return slices.Contains(served, key) })
	if len(orphans) == 0 {
		t.Fatal("the stopped writes left no chunk behind, want the chunks of the write whose manifest failed")
	}

	if collected, err := known.collectChunks(t.Context()); err != nil || collected != 0 {
		t.Fatalf("a collection inside the grace collected %d chunks (%v), want none", collected, err)
	}
	clock.set(time.Now().Add(kvchunk.Grace + 10*time.Minute))
	known.Disconnected()
	if collected, err := known.collectChunks(t.Context()); err != nil || collected != 0 {
		t.Fatalf("a collection while the cache does not follow its bucket collected %d chunks (%v), want none", collected, err)
	}
	if err := known.Rewatch(t.Context(), conn); err != nil {
		t.Fatalf("Rewatch: %v", err)
	}
	testwait.WithinPolling(t, 10*time.Second, testwait.Poll, "the store's cache to follow its bucket again", known.watch().Following)
	collected, err := known.collectChunks(t.Context())
	if err != nil {
		t.Fatalf("collect the chunks past the grace: %v", err)
	}
	if collected != len(orphans) {
		t.Fatalf("the collection past the grace collected %d chunks, want the %d no record names", collected, len(orphans))
	}
	if after := kvchunktest.LiveChunkKeys(t, kv); !slices.Equal(after, served) {
		t.Fatalf("after the collection the bucket holds chunks %v, want the served record's %v", after, served)
	}
	clock.set(time.Now())
	requireKnownTopics(t, "the store after the collection", known, "ses_known_crashed", old)
}

// A chunked known-session record with a chunk missing is a record the store cannot decode: a store
// that reads the bucket afresh answers no record for it, never a partial one.
func TestAChunkedKnownSessionRecordWithAMissingChunkIsNoRecordNotAPartialOne(t *testing.T) {
	conn := kvchunktest.SmallPayloadNATS(t)
	known := openKnownOn(t, conn, time.Now)
	if err := known.RecordWithin(t.Context(), KnownSession{SessionID: "ses_known_damaged", Topics: kvchunktest.Topics("damaged", 400)}); err != nil {
		t.Fatalf("record the record: %v", err)
	}
	kv := rawKnown(t, conn)
	chunks := kvchunktest.LiveChunkKeys(t, kv)
	if err := kv.Delete(chunks[len(chunks)/2]); err != nil {
		t.Fatalf("delete a chunk: %v", err)
	}
	fresh := openKnownOn(t, conn, time.Now)
	if record, ok := fresh.Get("ses_known_damaged"); ok {
		t.Fatalf("a store that read the bucket afresh answered a record of %d topics for a record missing a chunk", len(record.Topics))
	}
}

// A manifest no write of this build makes - here one claiming more chunks than the record has
// bytes, which a store that allocated by the claim would crash on - is a record the store cannot
// decode: a store that reads the bucket afresh keeps running and answers no record, and the
// session's next write replaces it.
func TestAForgedKnownSessionManifestIsNoRecordAndTheNextWriteReplacesIt(t *testing.T) {
	conn := kvchunktest.SmallPayloadNATS(t)
	forged := `chunked {"generation":"AAAAAAAAAAAAAAAAAAAAAAAAAA","chunks":4611686018427387904,"bytes":10,"sha256":"` + strings.Repeat("0", 64) + `"}`
	known := openKnownOn(t, conn, time.Now)
	if _, err := rawKnown(t, conn).Put("ses_known_forged", []byte(forged)); err != nil {
		t.Fatalf("store the forged manifest: %v", err)
	}
	fresh := openKnownOn(t, conn, time.Now)
	if record, ok := fresh.Get("ses_known_forged"); ok {
		t.Fatalf("a store that read the bucket afresh answered a record of %d topics for a forged manifest", len(record.Topics))
	}
	topics := kvchunktest.Topics("replacing", 10)
	if err := known.RecordWithin(t.Context(), KnownSession{SessionID: "ses_known_forged", Topics: topics}); err != nil {
		t.Fatalf("the write over the forged manifest: %v", err)
	}
	testwait.WithinPolling(t, 10*time.Second, testwait.Poll, "a store that read the bucket afresh to answer the record that replaced it", func() bool {
		record, ok := fresh.Get("ses_known_forged")
		return ok && slices.Equal(record.Topics, topics)
	})
}
