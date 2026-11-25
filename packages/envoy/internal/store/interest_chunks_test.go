package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/kvchunk"
	"github.com/sjawhar/envoy/internal/kvchunk/kvchunktest"
)

// rawInterests is the interest bucket on conn as a raw handle, which reads what is stored with
// no reassembly of its own.
func rawInterests(t *testing.T, conn *natsgo.Conn) natsgo.KeyValue {
	t.Helper()
	js, err := conn.JetStream()
	if err != nil {
		t.Fatalf("jetstream: %v", err)
	}
	kv, err := js.KeyValue(bus.InterestBucket)
	if err != nil {
		t.Fatalf("open the interest bucket: %v", err)
	}
	return kv
}

// requireTopics fails the test unless item holds exactly want, in the sorted order a write stores.
func requireTopics(t *testing.T, what string, item Interest, want []string) {
	t.Helper()
	if !slices.Equal(item.Topics, want) {
		t.Fatalf("%s holds %d topics, want the %d stored (first held %q)", what, len(item.Topics), len(want), firstTopic(item.Topics))
	}
}

func firstTopic(topics []string) string {
	if len(topics) == 0 {
		return ""
	}
	return topics[0]
}

func sortedTopics(sets ...[]string) []string {
	return slices.Compact(slices.Sorted(slices.Values(slices.Concat(sets...))))
}

// A record whose encoding is past the server's max payload is stored in chunks a manifest under the
// session's key names, each chunk under that max payload, and every reader gets the record whole:
// the writer's own bucket read, its cache, and another listener's cache, which its watcher fills
// from the bucket alone.
func TestARecordPastTheMaxPayloadIsStoredInChunksAndReadWhole(t *testing.T) {
	conn := kvchunktest.SmallPayloadNATS(t)
	reg := openRegistry(t, conn)
	topics := kvchunktest.Topics("wide", 600)
	want := sortedTopics(topics)

	if _, err := reg.Upsert(Interest{SessionID: "ses_wide", MachineID: "m1", Dir: "/wide"}, topics); err != nil {
		t.Fatalf("store a record of %d topics past the max payload: %v", len(topics), err)
	}
	row, _, err := reg.RefreshAt(t.Context(), "ses_wide")
	if err != nil {
		t.Fatalf("read the record from the bucket: %v", err)
	}
	requireTopics(t, "the bucket's record", row, want)
	if row.MachineID != "m1" || row.Dir != "/wide" {
		t.Fatalf("the bucket's record is on %q in %q, want m1 in /wide", row.MachineID, row.Dir)
	}
	cached, _ := reg.Cached("ses_wide")
	requireTopics(t, "the writer's cache", cached, want)

	other := openRegistry(t, conn)
	waitFor(t, 10*time.Second, func() bool {
		item, ok := other.Cached("ses_wide")
		return ok && slices.Equal(item.Topics, want)
	})
	if matched := other.Match("m1", topics[len(topics)-1]); len(matched) != 1 || matched[0].SessionID != "ses_wide" {
		t.Fatalf("another listener matches %d sessions for the record's last topic, want ses_wide alone", len(matched))
	}

	kv := rawInterests(t, conn)
	head, err := kv.Get("ses_wide")
	if err != nil {
		t.Fatalf("read the session's key: %v", err)
	}
	if !kvchunk.IsManifest(head.Value()) {
		t.Fatalf("the session's key holds %.60q, want a manifest", head.Value())
	}
	chunks := kvchunktest.LiveChunkKeys(t, kv)
	if len(chunks) < 3 {
		encoded, _ := json.Marshal(row)
		t.Fatalf("the bucket holds %d chunks, want the record's %d bytes in 3 or more", len(chunks), len(encoded))
	}
	for _, key := range chunks {
		chunk, err := kv.Get(key)
		if err != nil {
			t.Fatalf("read chunk %s: %v", key, err)
		}
		if len(chunk.Value()) > kvchunktest.MaxPayload {
			t.Fatalf("chunk %s holds %d bytes, past the server's max payload of %d", key, len(chunk.Value()), kvchunktest.MaxPayload)
		}
		if answer := kvchunktest.LeaderReadAnswer(t, conn, bus.InterestBucket, key); answer > kvchunktest.MaxPayload {
			t.Fatalf("the leader's answer to a read of chunk %s is %d bytes, past the server's max payload of %d, which a leafnode hop refuses", key, answer, kvchunktest.MaxPayload)
		}
	}
	t.Logf("a record of %d topics stored as a manifest of %d bytes and %d chunks", len(want), len(head.Value()), len(chunks))
}

// The control: a record under the max payload is stored whole under the session's key, as every
// earlier build stores it, with no chunk.
func TestARecordUnderTheMaxPayloadIsStoredWholeAsBefore(t *testing.T) {
	conn := kvchunktest.SmallPayloadNATS(t)
	reg := openRegistry(t, conn)
	topics := kvchunktest.Topics("narrow", 10)

	if _, err := reg.Upsert(Interest{SessionID: "ses_narrow", MachineID: "m1"}, topics); err != nil {
		t.Fatalf("store a record of %d topics: %v", len(topics), err)
	}
	kv := rawInterests(t, conn)
	head, err := kv.Get("ses_narrow")
	if err != nil {
		t.Fatalf("read the session's key: %v", err)
	}
	var stored Interest
	if err := json.Unmarshal(head.Value(), &stored); err != nil {
		t.Fatalf("the session's key holds %.60q, want the record whole as JSON: %v", head.Value(), err)
	}
	requireTopics(t, "the record stored whole", stored, sortedTopics(topics))
	if chunks := kvchunktest.LiveChunkKeys(t, kv); len(chunks) != 0 {
		t.Fatalf("the bucket holds chunks %v for a record that fits one message", chunks)
	}
}

// A rewrite that makes a chunked record fit one message stores it whole and deletes every chunk
// the record had, a rewrite that grows it again stores a new set of chunks and deletes the old
// set, and a deregistration deletes the manifest and its chunks, so no write leaves a chunk that
// no record names.
func TestARewriteThatShrinksGrowsOrDeletesARecordLeavesNoStaleChunk(t *testing.T) {
	conn := kvchunktest.SmallPayloadNATS(t)
	reg := openRegistry(t, conn)
	kv := rawInterests(t, conn)
	base := kvchunktest.Topics("base", 400)

	if _, err := reg.Upsert(Interest{SessionID: "ses_shrink", MachineID: "m1"}, base); err != nil {
		t.Fatalf("store the chunked record: %v", err)
	}
	first := kvchunktest.LiveChunkKeys(t, kv)
	if len(first) == 0 {
		t.Fatal("the record past the max payload was stored with no chunk")
	}

	if err := reg.removeInterestTopics(t.Context(), "ses_shrink", base[10:]); err != nil {
		t.Fatalf("remove all but 10 topics: %v", err)
	}
	if chunks := kvchunktest.LiveChunkKeys(t, kv); len(chunks) != 0 {
		t.Fatalf("after the record shrank to one message the bucket still holds chunks %v", chunks)
	}
	row, _, err := reg.RefreshAt(t.Context(), "ses_shrink")
	if err != nil {
		t.Fatalf("read the shrunk record: %v", err)
	}
	requireTopics(t, "the shrunk record", row, sortedTopics(base[:10]))

	if _, err := reg.Upsert(Interest{SessionID: "ses_shrink", MachineID: "m1"}, base); err != nil {
		t.Fatalf("grow the record again: %v", err)
	}
	grown := kvchunktest.LiveChunkKeys(t, kv)
	if _, err := reg.Upsert(Interest{SessionID: "ses_shrink", MachineID: "m1"}, kvchunktest.Topics("more", 200)); err != nil {
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

	if err := reg.removeInterestTopics(t.Context(), "ses_shrink", nil); err != nil {
		t.Fatalf("deregister: %v", err)
	}
	for key, op := range kvchunktest.LiveKeys(t, kv) {
		if op == natsgo.KeyValuePut {
			t.Fatalf("after the deregistration the bucket still holds %s", key)
		}
	}
}

// A reader of a record that another write is replacing gets the old record whole or the new one
// whole and never a mix: a bucket read that meets a chunk the rewrite deleted reads the new
// record, and a cache fed by its watcher stores a row only once it holds every chunk the manifest
// names. Each reader must have read both records over the run, so a reader that saw only one of
// them cannot pass for having seen no mix.
func TestAReaderDuringRewritesGetsOneWholeRecord(t *testing.T) {
	conn := kvchunktest.SmallPayloadNATS(t)
	writer := openRegistry(t, conn)
	other := openRegistry(t, conn)
	base, extra := kvchunktest.Topics("base", 400), kvchunktest.Topics("extra", 400)
	small, large := sortedTopics(base), sortedTopics(base, extra)
	if _, err := writer.Upsert(Interest{SessionID: "ses_rewritten", MachineID: "m1"}, base); err != nil {
		t.Fatalf("store the first record: %v", err)
	}
	waitFor(t, 10*time.Second, func() bool { _, ok := other.Cached("ses_rewritten"); return ok })

	ctx, stop := context.WithCancel(t.Context())
	var wg sync.WaitGroup
	type reading struct {
		small, large int
		failure      string
	}
	readers := map[string]func() (Interest, bool, error){
		"the writer's bucket read": func() (Interest, bool, error) {
			item, _, err := writer.RefreshAt(ctx, "ses_rewritten")
			return item, true, err
		},
		"another listener's bucket read": func() (Interest, bool, error) {
			item, _, err := other.RefreshAt(ctx, "ses_rewritten")
			return item, true, err
		},
		"another listener's cache": func() (Interest, bool, error) {
			item, ok := other.Cached("ses_rewritten")
			return item, ok, nil
		},
	}
	readings := map[string]*reading{}
	var mu sync.Mutex
	for name, read := range readers {
		got := &reading{}
		readings[name] = got
		wg.Go(func() {
			for ctx.Err() == nil {
				item, ok, err := read()
				mu.Lock()
				switch {
				case ctx.Err() != nil:
				case err != nil:
					got.failure = err.Error()
				case !ok:
					got.failure = "the cache lost the record"
				case slices.Equal(item.Topics, small):
					got.small++
				case slices.Equal(item.Topics, large):
					got.large++
				default:
					got.failure = fmt.Sprintf("a record of %d topics, neither the small one's %d nor the large one's %d", len(item.Topics), len(small), len(large))
				}
				mu.Unlock()
			}
		})
	}
	for range 12 {
		if _, err := writer.Upsert(Interest{SessionID: "ses_rewritten", MachineID: "m1"}, extra); err != nil {
			t.Fatalf("grow the record: %v", err)
		}
		time.Sleep(20 * time.Millisecond)
		if err := writer.removeInterestTopics(t.Context(), "ses_rewritten", extra); err != nil {
			t.Fatalf("shrink the record: %v", err)
		}
		time.Sleep(20 * time.Millisecond)
	}
	stop()
	wg.Wait()
	for name, got := range readings {
		t.Logf("%s read the small record %d times and the large one %d times", name, got.small, got.large)
		if got.failure != "" {
			t.Fatalf("%s read %s", name, got.failure)
		}
		if got.small == 0 || got.large == 0 {
			t.Fatalf("%s read the small record %d times and the large one %d times, want both", name, got.small, got.large)
		}
	}
}

// A write that stops at any step - between two chunks, between its chunks and its manifest, or
// after a chunk write that took past the write's bound - leaves the record it would have replaced
// served whole, from the bucket and from another listener's cache. What it left behind is chunks no
// record names, which the reaper's collection deletes once they are older than the collection
// grace and not before, and which no collection runs while the cache does not follow its bucket.
func TestAWriteStoppedAtAnyStepLeavesTheOldRecordServedAndItsChunksCollected(t *testing.T) {
	conn := kvchunktest.SmallPayloadNATS(t)
	clock := &manualClock{at: time.Now()}
	reg, err := Open(t.Context(), conn, WithReplicas(1), WithClock(clock.Now))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(reg.StopWatch)
	waitFor(t, 30*time.Second, reg.watcher.Ready)
	other := openRegistry(t, conn)
	kv := rawInterests(t, conn)
	legacy := reg.watcher.KV().Legacy()
	base := kvchunktest.Topics("base", 400)
	old := sortedTopics(base)
	if _, err := reg.Upsert(Interest{SessionID: "ses_crashed", MachineID: "m1"}, base); err != nil {
		t.Fatalf("store the record: %v", err)
	}
	served := kvchunktest.LiveChunkKeys(t, kv)

	requireOldServed := func(step string) {
		t.Helper()
		row, _, err := reg.RefreshAt(t.Context(), "ses_crashed")
		if err != nil {
			t.Fatalf("after a write stopped %s the record reads %v, want the old one", step, err)
		}
		requireTopics(t, "the bucket's record after a write stopped "+step, row, old)
		waitFor(t, 10*time.Second, func() bool {
			item, ok := other.Cached("ses_crashed")
			return ok && slices.Equal(item.Topics, old)
		})
	}
	for _, stopped := range []struct {
		step, label string
		kv          natsgo.KeyValue
	}{
		{"between two chunks", "chunks", &kvchunktest.ChunkFailing{KeyValue: legacy, FailAt: 2}},
		{"between its chunks and its manifest", "manifest", &kvchunktest.HeadFailing{KeyValue: legacy, Key: "ses_crashed"}},
		{"after its chunks took past the write's bound", "bound", &kvchunktest.ChunkFailing{KeyValue: legacy, Before: func() {
			clock.Set(clock.Now().Add(kvchunk.WriteBound))
		}}},
	} {
		useKV(t, reg, stopped.kv)
		if _, err := reg.Upsert(Interest{SessionID: "ses_crashed", MachineID: "m1"}, kvchunktest.Topics(stopped.label, 300)); err == nil {
			t.Fatalf("a write stopped %s answered success", stopped.step)
		} else {
			t.Logf("a write stopped %s answered: %v", stopped.step, err)
		}
		requireOldServed(stopped.step)
	}
	useKV(t, reg, legacy)
	left := kvchunktest.LiveChunkKeys(t, kv)
	orphans := slices.DeleteFunc(slices.Clone(left), func(key string) bool { return slices.Contains(served, key) })
	if len(orphans) == 0 {
		t.Fatalf("the stopped writes left no chunk behind, want the chunks they wrote: %v", left)
	}

	clock.Set(time.Now())
	if collected, err := reg.collectInterestChunks(t.Context()); err != nil || collected != 0 {
		t.Fatalf("a collection inside the grace collected %d chunks (%v), want none", collected, err)
	}
	// Past the grace for every chunk the copy holds, whatever the stopped writes moved the clock to.
	clock.Set(time.Now().Add(kvchunk.Grace + 10*time.Minute))
	reg.watcher.Disconnected()
	if collected, err := reg.collectInterestChunks(t.Context()); err != nil || collected != 0 {
		t.Fatalf("a collection while the cache does not follow its bucket collected %d chunks (%v), want none", collected, err)
	}
	if err := reg.Rewatch(t.Context(), conn); err != nil {
		t.Fatalf("Rewatch: %v", err)
	}
	waitFor(t, 10*time.Second, reg.watcher.Following)
	collected, err := reg.collectInterestChunks(t.Context())
	if err != nil {
		t.Fatalf("collect the chunks past the grace: %v", err)
	}
	if collected != len(orphans) {
		t.Fatalf("the collection past the grace collected %d chunks, want the %d no record names", collected, len(orphans))
	}
	if after := kvchunktest.LiveChunkKeys(t, kv); !slices.Equal(after, served) {
		t.Fatalf("after the collection the bucket holds chunks %v, want the served record's %v", after, served)
	}
	requireOldServed("and its chunks were collected")
}

// A manifest whose chunks are not all in the bucket, or whose chunks do not join into the record
// it names, is a record this build cannot decode, never a partial row: a bucket read answers
// ErrMalformedInterest, and a listener that reads the bucket afresh stores no row for it.
func TestAChunkedRecordWithAMissingOrChangedChunkIsUnreadableNotPartial(t *testing.T) {
	for _, damage := range []struct {
		what string
		do   func(t *testing.T, kv natsgo.KeyValue, key string)
	}{
		{"a chunk deleted", func(t *testing.T, kv natsgo.KeyValue, key string) {
			if err := kv.Delete(key); err != nil {
				t.Fatalf("delete chunk %s: %v", key, err)
			}
		}},
		{"a chunk rewritten", func(t *testing.T, kv natsgo.KeyValue, key string) {
			entry, err := kv.Get(key)
			if err != nil {
				t.Fatalf("read chunk %s: %v", key, err)
			}
			changed := slices.Clone(entry.Value())
			changed[len(changed)/2] ^= 1
			if _, err := kv.Put(key, changed); err != nil {
				t.Fatalf("rewrite chunk %s: %v", key, err)
			}
		}},
	} {
		t.Run(damage.what, func(t *testing.T) {
			conn := kvchunktest.SmallPayloadNATS(t)
			reg := openRegistry(t, conn)
			if _, err := reg.Upsert(Interest{SessionID: "ses_damaged", MachineID: "m1"}, kvchunktest.Topics("damaged", 400)); err != nil {
				t.Fatalf("store the record: %v", err)
			}
			kv := rawInterests(t, conn)
			chunks := kvchunktest.LiveChunkKeys(t, kv)
			damage.do(t, kv, chunks[len(chunks)/2])

			if row, _, err := reg.RefreshAt(t.Context(), "ses_damaged"); !errors.Is(err, ErrMalformedInterest) {
				t.Fatalf("a bucket read of the record with %s answered %d topics (%v), want ErrMalformedInterest", damage.what, len(row.Topics), err)
			}
			fresh := openRegistry(t, conn)
			if item, ok := fresh.Cached("ses_damaged"); ok {
				t.Fatalf("a listener that read the bucket afresh stored a row of %d topics for the record with %s", len(item.Topics), damage.what)
			}
		})
	}
}

// A chunk's key is no session's: the bucket's chunk keys share one prefix, and an interest write
// for a session id under that prefix is refused as a key this store does not take (bus.ErrRefused,
// which the listener answers 400), so no session record can stand where a chunk does. A session
// id outside it is the control.
func TestAnInterestWriteForASessionIDInTheChunkKeysIsRefused(t *testing.T) {
	conn := kvchunktest.SmallPayloadNATS(t)
	reg := openRegistry(t, conn)
	if _, err := reg.Upsert(Interest{SessionID: "ses_ordinary", MachineID: "m1"}, []string{"notifications.team.a"}); err != nil {
		t.Fatalf("store an ordinary session's record: %v", err)
	}
	_, err := reg.Upsert(Interest{SessionID: kvchunk.Prefix + "ABC.0", MachineID: "m1"}, []string{"notifications.team.a"})
	if !errors.Is(err, bus.ErrRefused) || errors.Is(err, bus.ErrTooLarge) {
		t.Fatalf("an interest write for a chunk's key answered %v, want a bus.ErrRefused that is not bus.ErrTooLarge", err)
	}
	if !strings.Contains(err.Error(), kvchunk.Prefix) {
		t.Fatalf("the refusal %q does not name the chunk keys' prefix %q", err, kvchunk.Prefix)
	}
}

// racingHeadKeyValue is the interest bucket with race run once, just before the first
// compare-and-swap of key's record is sent: another writer storing the record between a write's
// read and its manifest, so the bucket refuses that manifest.
type racingHeadKeyValue struct {
	natsgo.KeyValue
	key   string
	race  func()
	raced bool
}

func (kv *racingHeadKeyValue) Update(key string, value []byte, revision uint64) (uint64, error) {
	if key == kv.key && !kv.raced {
		kv.raced = true
		kv.race()
	}
	return kv.KeyValue.Update(key, value, revision)
}

// A write whose manifest the bucket refuses, because another writer stored the record under it,
// deletes the chunks it wrote before it reads and writes again, so once both writes have landed the
// bucket holds the chunks of the one record stored and of no write it refused.
func TestAWriteTheBucketRefusedLeavesNoChunkOfItsOwn(t *testing.T) {
	conn := kvchunktest.SmallPayloadNATS(t)
	reg := openRegistry(t, conn)
	other := openRegistry(t, conn)
	kv := rawInterests(t, conn)
	base := kvchunktest.Topics("base", 400)
	if _, err := reg.Upsert(Interest{SessionID: "ses_refused", MachineID: "m1"}, base); err != nil {
		t.Fatalf("store the record: %v", err)
	}
	racing := &racingHeadKeyValue{KeyValue: reg.watcher.KV().Legacy(), key: "ses_refused", race: func() {
		if _, err := other.Upsert(Interest{SessionID: "ses_refused", MachineID: "m2"}, kvchunktest.Topics("other", 10)); err != nil {
			t.Errorf("the write that lands under the refused one: %v", err)
		}
	}}
	useKV(t, reg, racing)
	stored, err := reg.Upsert(Interest{SessionID: "ses_refused", MachineID: "m1"}, kvchunktest.Topics("mine", 10))
	if err != nil {
		t.Fatalf("the write the bucket refused once, then took: %v", err)
	}
	if !racing.raced {
		t.Fatal("the other writer never stored the record under this write, so nothing was refused")
	}
	layout := kvchunktest.HeadLayout(t, kv, "ses_refused")
	if chunks := kvchunktest.LiveChunkKeys(t, kv); !slices.Equal(chunks, slices.Sorted(slices.Values(layout.Keys()))) {
		t.Fatalf("the bucket holds chunks %v, want only the %d its manifest names", chunks, layout.Chunks)
	}
	requireTopics(t, "the record both writes merged into", stored, sortedTopics(base, kvchunktest.Topics("other", 10), kvchunktest.Topics("mine", 10)))
}

// The two deletes fenced on a revision the caller read - an unsubscribe that removes every topic a
// record holds, and the reaper's delete of a gone session's record - delete the record's chunks
// with it, as the delete that reads its own revision does (the deregistration of
// TestARewriteThatShrinksGrowsOrDeletesARecordLeavesNoStaleChunk).
func TestAFencedDeleteOfAChunkedRecordLeavesNoChunk(t *testing.T) {
	conn := kvchunktest.SmallPayloadNATS(t)
	clock := &manualClock{at: time.Now()}
	reg, err := Open(t.Context(), conn, WithReplicas(1), WithClock(clock.Now))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(reg.StopWatch)
	waitFor(t, 30*time.Second, reg.watcher.Ready)
	kv := rawInterests(t, conn)
	requireNoKey := func(how string) {
		t.Helper()
		for key, op := range kvchunktest.LiveKeys(t, kv) {
			if op == natsgo.KeyValuePut {
				t.Fatalf("after %s the bucket still holds %s", how, key)
			}
		}
	}

	topics := kvchunktest.Topics("unsubscribed", 400)
	if _, err := reg.Upsert(Interest{SessionID: "ses_unsubscribed", MachineID: "m1"}, topics); err != nil {
		t.Fatalf("store the record: %v", err)
	}
	if err := reg.removeInterestTopics(t.Context(), "ses_unsubscribed", topics); err != nil {
		t.Fatalf("remove every topic the record holds: %v", err)
	}
	requireNoKey("an unsubscribe of every topic")

	if _, err := reg.Upsert(Interest{SessionID: "ses_reaped", MachineID: "m1"}, kvchunktest.Topics("reaped", 400)); err != nil {
		t.Fatalf("store the record: %v", err)
	}
	clock.Set(clock.Now().Add(InterestReapAllowance(InterestReapGrace) + time.Minute))
	reaped, err := reg.Reap(func(string) (bool, error) { return false, nil }, InterestReapGrace)
	if err != nil || reaped != 1 {
		t.Fatalf("the reaper reaped %d records (%v), want the gone session's one", reaped, err)
	}
	requireNoKey("the reaper's delete")
}

// laggingCopyKeyValue is the interest bucket as a copy of its stream that is behind the leader
// answers a direct get: it has not applied the chunks a write stored a moment ago, so a direct get
// of one answers that it is not there (bus.KeyValue.Get says why a direct get can be answered by
// such a copy).
type laggingCopyKeyValue struct{ natsgo.KeyValue }

func (kv laggingCopyKeyValue) Get(key string) (natsgo.KeyValueEntry, error) {
	if kvchunk.IsChunkKey(key) {
		return nil, natsgo.ErrKeyNotFound
	}
	return kv.KeyValue.Get(key)
}

// A read of a chunked record from the bucket reads its chunks from the stream's leader, so a copy
// of the stream behind the leader, which answers a direct get of a chunk it has not applied as no
// chunk at all, cannot make the record read as damaged.
func TestAChunkedRecordReadsWholeWhereADirectGetOfItsChunksWouldMissThem(t *testing.T) {
	conn := kvchunktest.SmallPayloadNATS(t)
	reg := openRegistry(t, conn)
	topics := kvchunktest.Topics("lagging", 400)
	if _, err := reg.Upsert(Interest{SessionID: "ses_lagging", MachineID: "m1"}, topics); err != nil {
		t.Fatalf("store the record: %v", err)
	}
	useKV(t, reg, laggingCopyKeyValue{KeyValue: reg.watcher.KV().Legacy()})
	row, _, err := reg.RefreshAt(t.Context(), "ses_lagging")
	if err != nil {
		t.Fatalf("a read whose direct gets miss the chunks answered %v, want the record", err)
	}
	requireTopics(t, "the record read past a lagging copy", row, sortedTopics(topics))
}

// deleteFailingKeyValue is the interest bucket whose deletes of key fail as a bus that stopped
// answering does.
type deleteFailingKeyValue struct {
	natsgo.KeyValue
	key string
}

func (kv deleteFailingKeyValue) Delete(key string, opts ...natsgo.DeleteOpt) error {
	if key == kv.key {
		return natsgo.ErrTimeout
	}
	return kv.KeyValue.Delete(key, opts...)
}

// The reaper's pass collects the chunks no record names even when its reap of dead sessions failed,
// so a bucket whose reap keeps failing still loses its stale chunks.
func TestAReaperPassCollectsChunksWhenItsReapFailed(t *testing.T) {
	conn := kvchunktest.SmallPayloadNATS(t)
	clock := &manualClock{at: time.Now()}
	reg, err := Open(t.Context(), conn, WithReplicas(1), WithClock(clock.Now))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(reg.StopWatch)
	waitFor(t, 30*time.Second, reg.watcher.Ready)
	kv := rawInterests(t, conn)
	legacy := reg.watcher.KV().Legacy()
	if _, err := reg.Upsert(Interest{SessionID: "ses_dead", MachineID: "m1"}, []string{"notifications.team.dead"}); err != nil {
		t.Fatalf("store the dead session's record: %v", err)
	}
	useKV(t, reg, &kvchunktest.HeadFailing{KeyValue: legacy, Key: "ses_orphaning"})
	if _, err := reg.Upsert(Interest{SessionID: "ses_orphaning", MachineID: "m1"}, kvchunktest.Topics("orphaned", 400)); err == nil {
		t.Fatal("the write whose manifest failed answered success")
	}
	if len(kvchunktest.LiveChunkKeys(t, kv)) == 0 {
		t.Fatal("the write whose manifest failed left no chunk, so there is nothing to collect")
	}
	useKV(t, reg, deleteFailingKeyValue{KeyValue: legacy, key: "ses_dead"})
	clock.Set(time.Now().Add(InterestReapAllowance(InterestReapGrace) + kvchunk.Grace))
	if _, err := reg.Reap(func(string) (bool, error) { return false, nil }, InterestReapGrace); err == nil {
		t.Fatal("the reap whose delete fails answered no error, so the pass below is not the failing one")
	}
	ctx, stop := context.WithCancel(t.Context())
	done := reg.StartReaper(ctx, func(string) (bool, error) { return false, nil }, 20*time.Millisecond, InterestReapGrace)
	t.Cleanup(func() { stop(); <-done })
	waitFor(t, 10*time.Second, func() bool { return len(kvchunktest.LiveChunkKeys(t, kv)) == 0 })
}

// headRefusingKeyValue is the interest bucket whose writes of key's record the server refuses, as a
// stream past its storage bound refuses a message (JetStream's insufficient resources), so a write
// of that record stops at its head with an answer the server gave.
type headRefusingKeyValue struct {
	natsgo.KeyValue
	key string
}

var errStorageRefused = &natsgo.APIError{Code: 503, ErrorCode: 10047, Description: "insufficient storage resources available"}

func (kv headRefusingKeyValue) Create(key string, value []byte) (uint64, error) {
	if key == kv.key {
		return 0, errStorageRefused
	}
	return kv.KeyValue.Create(key, value)
}

func (kv headRefusingKeyValue) Update(key string, value []byte, revision uint64) (uint64, error) {
	if key == kv.key {
		return 0, errStorageRefused
	}
	return kv.KeyValue.Update(key, value, revision)
}

// A write whose head the bus refused stored nothing that names its chunks, so it deletes them
// before it answers, whatever the refusal: a server's (a stream past its storage bound) or this
// client's own before sending (a session id too long for the head's subject, which the chunks'
// short keys did not meet). The write's error is the refusal itself.
func TestAWriteWhoseHeadTheBusRefusedLeavesNoChunkOfItsOwn(t *testing.T) {
	conn := kvchunktest.SmallPayloadNATS(t)
	reg := openRegistry(t, conn)
	kv := rawInterests(t, conn)
	legacy := reg.watcher.KV().Legacy()
	longID := "ses_" + strings.Repeat("l", 4100)
	for what, write := range map[string]func() error{
		"by the server": func() error {
			useKV(t, reg, headRefusingKeyValue{KeyValue: legacy, key: "ses_head_refused"})
			_, err := reg.Upsert(Interest{SessionID: "ses_head_refused", MachineID: "m1"}, kvchunktest.Topics("refused", 400))
			if !errors.Is(err, errStorageRefused) {
				t.Fatalf("the write the server refused answered %v, want its refusal", err)
			}
			return err
		},
		"by this client": func() error {
			useKV(t, reg, legacy)
			_, err := reg.Upsert(Interest{SessionID: longID, MachineID: "m1"}, kvchunktest.Topics("long", 400))
			if !errors.Is(err, bus.ErrRefused) {
				t.Fatalf("the write of a session id of %d bytes answered %v, want a bus.ErrRefused", len(longID), err)
			}
			return err
		},
	} {
		err := write()
		t.Logf("the head refused %s: %v", what, err)
		if chunks := kvchunktest.LiveChunkKeys(t, kv); len(chunks) != 0 {
			t.Fatalf("after its head was refused %s the bucket still holds chunks %v", what, chunks)
		}
	}
}

// The chunk copy's revision fence holds one entry per chunk the bucket holds, however many times a
// chunked record is rewritten: each rewrite deletes the chunks it replaced, and a chunk key, never
// written again, leaves no fence entry behind its delete (kvwatch.Copy.Forget). Without that, the
// fence kept every replaced chunk's key until a completed read of the bucket, which this watcher
// makes only on a reconnect: about 576 entries a day for a session whose record is two chunks and
// is rewritten every 5 minutes. The control is the count it reaches without the drop, which the
// mutation survey's fence-forgets-chunk-deletes run shows growing with the rewrites.
func TestTheChunkCopysFenceStaysBoundedAcrossRewrites(t *testing.T) {
	conn := kvchunktest.SmallPayloadNATS(t)
	reg := openRegistry(t, conn)
	kv := rawInterests(t, conn)
	const rewrites = 40
	for rewrite := range rewrites {
		label := []string{"even", "odd"}[rewrite%2]
		if _, err := reg.Upsert(Interest{SessionID: "ses_rewritten_often", MachineID: "m1"}, kvchunktest.Topics(label, 400)); err != nil {
			t.Fatalf("rewrite %d: %v", rewrite, err)
		}
		if err := reg.removeInterestTopics(t.Context(), "ses_rewritten_often", kvchunktest.Topics(label, 400)[200:]); err != nil {
			t.Fatalf("rewrite %d, its shrink: %v", rewrite, err)
		}
	}
	live := len(kvchunktest.LiveChunkKeys(t, kv))
	if live == 0 {
		t.Fatal("the bucket holds no chunk, so the case rewrote no chunked record")
	}
	fenced := func() int {
		reg.mu.RLock()
		defer reg.mu.RUnlock()
		return reg.chunks.Fenced()
	}
	deadline := time.Now().Add(10 * time.Second)
	for fenced() > live && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	t.Logf("after %d rewrites the bucket holds %d chunks and the chunk copy's fence %d keys", 2*rewrites, live, fenced())
	if got := fenced(); got > live {
		t.Fatalf("after %d rewrites the chunk copy's fence holds %d keys, want at most the %d chunks the bucket holds", 2*rewrites, got, live)
	}
}
