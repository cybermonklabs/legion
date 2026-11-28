package store

// The cases of a chunked interest record that no write of this build makes, or that a delete reads
// back changed: forged, damaged, borrowed and copied manifests, a removal naming a chunk's key, and
// the cache after a delete that read nothing or found a record stored since. The storage and
// rewrite cases are in interest_chunks_test.go, whose helpers these share.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/kvchunk"
	"github.com/sjawhar/envoy/internal/kvchunk/kvchunktest"
	"github.com/sjawhar/envoy/internal/kvwatch"
)

// historyFailingWatch is the interest bucket's watch handle with every watch of one key failing, as
// a history read that does not answer fails; the watcher's own watch, of every key, is unaffected.
type historyFailingWatch struct {
	jetstream.KeyValue
	key string
}

func (kv historyFailingWatch) Watch(ctx context.Context, keys string, opts ...jetstream.WatchOpt) (jetstream.KeyWatcher, error) {
	if keys == kv.key {
		return nil, context.DeadlineExceeded
	}
	return kv.KeyValue.Watch(ctx, keys, opts...)
}

// A delete that read nothing and so deleted nothing, whose read of the key's history then fails,
// leaves the cache's row as it is: the delete changed nothing in the bucket, and evicting the row
// would leave the chunks of a record that still stands to the collection, past which the record is
// a manifest no listener joins or reaps. The control is the same record after the collection: it
// still reads whole.
func TestADeleteThatReadNothingAndCannotReadHistoryKeepsTheRow(t *testing.T) {
	conn := kvchunktest.SmallPayloadNATS(t)
	clock := &manualClock{at: time.Now()}
	reg, err := Open(t.Context(), conn, WithReplicas(1), WithClock(clock.Now))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(reg.StopWatch)
	waitFor(t, 30*time.Second, reg.watcher.Ready)
	topics := kvchunktest.Topics("unread", 400)
	if _, err := reg.Upsert(Interest{SessionID: "ses_unread", MachineID: "m1"}, topics); err != nil {
		t.Fatalf("store the record: %v", err)
	}
	base := reg.watcher.KV()
	reg.watcher.Stop()
	reg.watcher = kvwatch.New("interest registry", bus.NewKeyValue(missingGetKeyValue{KeyValue: base.Legacy(), key: "ses_unread", leader: base},
		historyFailingWatch{KeyValue: base.WatchHandle(), key: "ses_unread"}, base.Streams()), interestCache{reg})
	reg.watcher.Start()
	waitFor(t, 30*time.Second, reg.watcher.Ready)
	if err := reg.deleteInterest(t.Context(), "ses_unread"); err != nil {
		t.Fatalf("the delete whose read found nothing: %v", err)
	}
	if _, ok := reg.Cached("ses_unread"); !ok {
		t.Fatal("after a delete that deleted nothing and could not read the history, the cache holds no row for the record that still stands")
	}
	clock.Set(time.Now().Add(kvchunk.Grace + time.Minute))
	if collected, err := reg.collectInterestChunks(t.Context()); err != nil || collected != 0 {
		t.Fatalf("the collection took %d chunks (%v) of a record that still stands, want none", collected, err)
	}
	row, _, err := openRegistry(t, conn).RefreshAt(t.Context(), "ses_unread")
	if err != nil {
		t.Fatalf("read the record after the collection: %v", err)
	}
	requireTopics(t, "the record after the collection", row, sortedTopics(topics))
}

// writingAfterDeleteKeyValue is the interest bucket with after run once a delete of key has landed:
// another listener's registration storing the record again before the deleting listener reads the
// key's history back.
type writingAfterDeleteKeyValue struct {
	natsgo.KeyValue
	key   string
	after func()
}

func (kv *writingAfterDeleteKeyValue) Delete(key string, opts ...natsgo.DeleteOpt) error {
	err := kv.KeyValue.Delete(key, opts...)
	if err == nil && key == kv.key && kv.after != nil {
		kv.after()
		kv.after = nil
	}
	return err
}

// A delete whose read-back finds that another listener stored the record again since, as a
// manifest, leaves the deleting listener's cache without the row until its watcher delivers that
// manifest after its chunks: it does not join the manifest from chunks the watcher has not
// delivered yet, which would evict the row as a record it cannot decode and log that it did. The
// watcher is stopped here, so no chunk of the new record reaches the copy.
func TestADeleteThatFindsAChunkedRecordStoredSinceLogsNoUndecodableRecord(t *testing.T) {
	conn := kvchunktest.SmallPayloadNATS(t)
	logs := captureRegistryLogs(t)
	reg := openRegistry(t, conn)
	other := openRegistry(t, conn)
	if _, err := reg.Upsert(Interest{SessionID: "ses_stored_again", MachineID: "m1"}, kvchunktest.Topics("first", 10)); err != nil {
		t.Fatalf("store the record: %v", err)
	}
	useKV(t, reg, &writingAfterDeleteKeyValue{KeyValue: reg.watcher.KV().Legacy(), key: "ses_stored_again", after: func() {
		if _, err := other.Upsert(Interest{SessionID: "ses_stored_again", MachineID: "m2"}, kvchunktest.Topics("again", 400)); err != nil {
			t.Errorf("store the record again: %v", err)
		}
	}})
	reg.watcher.Stop()
	if err := reg.deleteInterest(t.Context(), "ses_stored_again"); err != nil {
		t.Fatalf("delete the record: %v", err)
	}
	if item, ok := reg.Cached("ses_stored_again"); ok {
		t.Fatalf("the deleting listener's cache holds a row of %d topics after the delete, want none until its watcher delivers the new record", len(item.Topics))
	}
	if text := logs.String(); strings.Contains(text, "evicted malformed value") {
		t.Fatalf("the delete logged an undecodable record for a manifest whose chunks its watcher had not delivered:\n%s", text)
	}
}

// forgedManifests are values under a record's key that no write of this build makes and any
// writer of the bucket can store: manifests whose numbers no record has, one whose generation is no
// key nats.go accepts, and two whose numbers are a write's shape but claim far more than the bucket
// holds, which only a reader that sizes nothing by a manifest's claim reads without crashing.
var forgedManifests = map[string]string{
	"a negative length":         `{"chunked":{"generation":"AAAAAAAAAAAAAAAAAAAAAAAAAA","chunks":1,"bytes":-1,"sha256":"` + strings.Repeat("0", 64) + `"}}`,
	"more chunks than bytes":    `{"chunked":{"generation":"AAAAAAAAAAAAAAAAAAAAAAAAAA","chunks":4611686018427387904,"bytes":10,"sha256":"` + strings.Repeat("0", 64) + `"}}`,
	"a generation with a space": `{"chunked":{"generation":"bad gen","chunks":1,"bytes":10,"sha256":"` + strings.Repeat("0", 64) + `"}}`,
	"a length no record has":    `{"chunked":{"generation":"AAAAAAAAAAAAAAAAAAAAAAAAAA","chunks":1,"bytes":4611686018427387904,"sha256":"` + strings.Repeat("0", 64) + `"}}`,
	"a chunk count no record has": `{"chunked":{"generation":"AAAAAAAAAAAAAAAAAAAAAAAAAA","chunks":4611686018427387904,"bytes":4611686018427387904,"sha256":"` +
		strings.Repeat("0", 64) + `"}}`,
}

// A manifest no write of this build makes is a record this build cannot decode, wherever it is
// read, and it never stops a listener: a listener that reads the bucket afresh stores no row for
// it and keeps running, a read from the bucket answers ErrMalformedInterest, the session's next
// write replaces it, as a write replaces any record this build cannot decode, and a deregistration
// of another session holding the same manifest deletes it.
func TestAForgedManifestIsARecordThisBuildCannotDecodeAndTheNextWriteReplacesIt(t *testing.T) {
	for what, manifest := range forgedManifests {
		t.Run(what, func(t *testing.T) {
			// Each forgery is a well-formed manifest whose one fault is its layout, so the store meets
			// that layout rather than a decode error.
			if !json.Valid([]byte(manifest)) {
				t.Fatalf("the forged manifest with %s is not valid JSON, so the case would not reach its layout", what)
			}
			conn := kvchunktest.SmallPayloadNATS(t)
			reg := openRegistry(t, conn)
			kv := rawInterests(t, conn)
			for _, id := range []string{"ses_forged", "ses_forged_deregistered"} {
				if _, err := kv.Put(id, []byte(manifest)); err != nil {
					t.Fatalf("store the forged manifest under %s: %v", id, err)
				}
			}
			fresh := openRegistry(t, conn)
			if item, ok := fresh.Cached("ses_forged"); ok {
				t.Fatalf("a listener that read the bucket afresh stored a row of %d topics for a manifest with %s", len(item.Topics), what)
			}
			if _, _, err := reg.RefreshAt(t.Context(), "ses_forged"); !errors.Is(err, ErrMalformedInterest) {
				t.Fatalf("a bucket read of a manifest with %s answered %v, want ErrMalformedInterest", what, err)
			}
			topics := kvchunktest.Topics("replacing", 10)
			if _, err := reg.Upsert(Interest{SessionID: "ses_forged", MachineID: "m1"}, topics); err != nil {
				t.Fatalf("the write over a manifest with %s answered %v, want it stored", what, err)
			}
			row, _, err := reg.RefreshAt(t.Context(), "ses_forged")
			if err != nil {
				t.Fatalf("read the record that replaced the manifest with %s: %v", what, err)
			}
			requireTopics(t, "the record that replaced the manifest with "+what, row, sortedTopics(topics))
			if _, err := reg.Remove(t.Context(), "ses_forged_deregistered", nil, RoleReleaseAuthority{}); err != nil {
				t.Fatalf("the deregistration of a session holding a manifest with %s answered %v", what, err)
			}
			if ops := kvchunktest.LiveKeys(t, kv); ops["ses_forged_deregistered"] == natsgo.KeyValuePut {
				t.Fatalf("after its deregistration the bucket still holds the manifest with %s", what)
			}
		})
	}
}

// missingGetKeyValue is the interest bucket as a read of key answers it just before a registration
// lands: no record at all, from a direct get and from the stream's leader alike (bus.LeaderReads),
// as the leader read a delete makes (bus.KeyValue.DeleteAtRead) answers when the registration that
// stores the record lands after it. Every other key, its chunks among them, is read from leader,
// the bucket's own handle.
type missingGetKeyValue struct {
	natsgo.KeyValue
	key    string
	leader bus.KeyValue
}

func (kv missingGetKeyValue) Get(key string) (natsgo.KeyValueEntry, error) {
	if key == kv.key {
		return nil, natsgo.ErrKeyNotFound
	}
	return kv.KeyValue.Get(key)
}

func (kv missingGetKeyValue) NewestFromLeader(ctx context.Context, key string) (natsgo.KeyValueEntry, error) {
	if key == kv.key {
		return nil, natsgo.ErrKeyNotFound
	}
	return kv.leader.NewestFromLeader(ctx, key)
}

// A delete whose read of the record found nothing deletes nothing, and its read-back finds the
// manifest the cache already holds: the cache keeps that row, since the chunk copy holds its chunks,
// and so no collection takes the chunks of a record that still stands.
func TestADeleteThatReadNothingKeepsTheChunkedRowItsCacheHolds(t *testing.T) {
	conn := kvchunktest.SmallPayloadNATS(t)
	clock := &manualClock{at: time.Now()}
	reg, err := Open(t.Context(), conn, WithReplicas(1), WithClock(clock.Now))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(reg.StopWatch)
	waitFor(t, 30*time.Second, reg.watcher.Ready)
	topics := kvchunktest.Topics("kept", 400)
	if _, err := reg.Upsert(Interest{SessionID: "ses_kept", MachineID: "m1"}, topics); err != nil {
		t.Fatalf("store the record: %v", err)
	}
	legacy := reg.watcher.KV().Legacy()
	useKV(t, reg, missingGetKeyValue{KeyValue: legacy, key: "ses_kept", leader: reg.watcher.KV()})
	if err := reg.deleteInterest(t.Context(), "ses_kept"); err != nil {
		t.Fatalf("the delete whose read found nothing: %v", err)
	}
	cached, ok := reg.Cached("ses_kept")
	if !ok {
		t.Fatal("after a delete that deleted nothing the cache holds no row for the record that still stands")
	}
	requireTopics(t, "the row the cache kept", cached, sortedTopics(topics))
	useKV(t, reg, legacy)
	clock.Set(time.Now().Add(kvchunk.Grace + time.Minute))
	if collected, err := reg.collectInterestChunks(t.Context()); err != nil || collected != 0 {
		t.Fatalf("the collection took %d chunks (%v) of a record that still stands, want none", collected, err)
	}
	row, _, err := reg.RefreshAt(t.Context(), "ses_kept")
	if err != nil {
		t.Fatalf("read the record: %v", err)
	}
	requireTopics(t, "the record after the collection", row, sortedTopics(topics))
}

// A deregistration or an unsubscribe naming a chunk's key as its session is refused, as a write of
// one is, so no route deletes a chunk a stored manifest names.
func TestARemovalNamingAChunksKeyIsRefusedAndDeletesNothing(t *testing.T) {
	conn := kvchunktest.SmallPayloadNATS(t)
	reg := openRegistry(t, conn)
	if _, err := reg.Upsert(Interest{SessionID: "ses_named", MachineID: "m1"}, kvchunktest.Topics("named", 400)); err != nil {
		t.Fatalf("store the record: %v", err)
	}
	kv := rawInterests(t, conn)
	chunks := kvchunktest.LiveChunkKeys(t, kv)
	for what, remove := range map[string]func() error{
		"a deregistration": func() error {
			_, err := reg.Remove(t.Context(), chunks[0], nil, RoleReleaseAuthority{})
			return err
		},
		"an unsubscribe": func() error {
			_, err := reg.Remove(t.Context(), chunks[0], []string{"notifications.team.x"}, RoleReleaseAuthority{})
			return err
		},
	} {
		if err := remove(); !errors.Is(err, bus.ErrRefused) {
			t.Fatalf("%s naming chunk %s answered %v, want bus.ErrRefused", what, chunks[0], err)
		}
	}
	if after := kvchunktest.LiveChunkKeys(t, kv); !slices.Equal(after, chunks) {
		t.Fatalf("after the refused removals the bucket holds chunks %v, want %v", after, chunks)
	}
	if _, _, err := reg.RefreshAt(t.Context(), "ses_named"); err != nil {
		t.Fatalf("read the record whose chunk the removals named: %v", err)
	}
}

// A damaged manifest names chunks that were never proved to be its own, so the write that replaces
// it deletes none of them. Here it is the owner's manifest with a SHA-256 its chunks do not join
// into, stored under another session's key: a bucket read of that key answers a record this build
// cannot decode at the manifest's own revision and with no layout (readRow, decodeEntry), that
// session's next write replaces it with the session's own row, and the owner's record still reads
// whole with every chunk it names. The same manifest under an unsubscribe and a deregistration of the
// other session, and the owner's own deregistration as the control, are
// TestACopiedOrBorrowedManifestLeavesTheOwnersChunksAlone's ("a forged SHA-256"), which this test
// does not repeat.
func TestAWriteOverADamagedManifestDeletesNoChunkItNames(t *testing.T) {
	conn := kvchunktest.SmallPayloadNATS(t)
	reg := openRegistry(t, conn)
	kv := rawInterests(t, conn)
	topics := kvchunktest.Topics("owner", 400)
	head, layout := storeOwner(t, reg, kv, Interest{SessionID: "ses_owner", MachineID: "m1"}, topics)
	stored, err := kv.Put("ses_borrower", copiedManifests(t, kv, head, layout)["a forged SHA-256"])
	if err != nil {
		t.Fatalf("store the damaged manifest: %v", err)
	}
	read, revision, err := reg.readRow(t.Context(), "ses_borrower", readBucket)
	if !errors.Is(err, ErrMalformedInterest) || revision != stored || read.layout.Chunked() {
		t.Fatalf("a bucket read of the damaged manifest answered layout %+v at revision %d, %v; want ErrMalformedInterest at %d with no layout",
			read.layout, revision, err, stored)
	}
	borrower := []string{"notifications.team.borrower"}
	if _, err := reg.Upsert(Interest{SessionID: "ses_borrower", MachineID: "m1"}, borrower); err != nil {
		t.Fatalf("the write over the damaged manifest: %v", err)
	}
	row, _, err := reg.RefreshAt(t.Context(), "ses_borrower")
	if err != nil || row.SessionID != "ses_borrower" || !slices.Equal(row.Topics, borrower) {
		t.Fatalf("after the write over the damaged manifest ses_borrower's row reads %+v (%v), want its own row holding %v", row, err, borrower)
	}
	requireOwnerWhole(t, reg, kv, layout.Keys(), sortedTopics(topics), "after the write over the damaged manifest")
}

// storeOwner stores item's record of topics through reg and returns the manifest the bucket holds
// under the session's key and the layout it names, which must be chunked.
func storeOwner(t *testing.T, reg *Registry, kv natsgo.KeyValue, item Interest, topics []string) ([]byte, kvchunk.Layout) {
	t.Helper()
	if _, err := reg.Upsert(item, topics); err != nil {
		t.Fatalf("store %s's record: %v", item.SessionID, err)
	}
	head, err := kv.Get(item.SessionID)
	if err != nil {
		t.Fatalf("read %s's manifest: %v", item.SessionID, err)
	}
	layout, err := kvchunk.ReadLayout(head.Value())
	if err != nil || !layout.Chunked() {
		t.Fatalf("%s's key holds %.60q (%v), want a manifest", item.SessionID, head.Value(), err)
	}
	return head.Value(), layout
}

// requireOwnerWhole fails t unless the bucket still holds every one of keys, the chunks of
// ses_owner's live record, and a bucket read through reg answers that record whole with want.
func requireOwnerWhole(t *testing.T, reg *Registry, kv natsgo.KeyValue, keys, want []string, when string) {
	t.Helper()
	live := kvchunktest.LiveChunkKeys(t, kv)
	for _, key := range keys {
		if !slices.Contains(live, key) {
			t.Fatalf("%s the bucket no longer holds %s, a chunk of the owner's live record (it holds %d chunks)", when, key, len(live))
		}
	}
	row, _, err := reg.RefreshAt(t.Context(), "ses_owner")
	if err != nil {
		t.Fatalf("%s a bucket read of the owner answers %v, want its record whole", when, err)
	}
	requireTopics(t, "the owner's record "+when, row, want)
}

// firstChunksManifest is a manifest naming only the first chunks of layout, all of them but the
// last, with their own length and SHA-256: one any writer of the bucket can store, which passes
// every check a read makes (kvchunk.ReadLayout, and join's length and SHA-256 against the manifest's
// own claims) and joins into a prefix of the record layout stores, bytes that decode as no record.
func firstChunksManifest(t *testing.T, kv natsgo.KeyValue, layout kvchunk.Layout) []byte {
	t.Helper()
	if layout.Chunks < 2 {
		t.Fatalf("the record is in %d chunks, want 2 or more so a manifest can name some of them", layout.Chunks)
	}
	var prefix []byte
	for _, key := range layout.Keys()[:layout.Chunks-1] {
		chunk, err := kv.Get(key)
		if err != nil {
			t.Fatalf("read chunk %s: %v", key, err)
		}
		prefix = append(prefix, chunk.Value()...)
	}
	sum := sha256.Sum256(prefix)
	named, err := json.Marshal(kvchunk.Layout{Generation: layout.Generation, Chunks: layout.Chunks - 1, Bytes: len(prefix), SHA256: hex.EncodeToString(sum[:])})
	if err != nil {
		t.Fatalf("encode the manifest of the first chunks: %v", err)
	}
	return append(append([]byte(`{"chunked":`), named...), '}')
}

// copiedManifests are the manifests a writer of the bucket can store under another session's key
// to name the owner's chunks, built from the owner's manifest head and its layout: an exact copy,
// which joins into the owner's record, a record naming the owner (ErrInterestUnbound); one with a
// forged SHA-256, which no read joins (kvchunk.ErrDamaged); and one naming only the owner's first
// chunks, which joins into bytes that are no record (ErrMalformedInterest).
func copiedManifests(t *testing.T, kv natsgo.KeyValue, head []byte, layout kvchunk.Layout) map[string][]byte {
	t.Helper()
	forged := layout
	forged.SHA256 = strings.Repeat("0", 64)
	encoded, err := json.Marshal(forged)
	if err != nil {
		t.Fatalf("encode the forged manifest: %v", err)
	}
	return map[string][]byte{
		"an exact copy":            head,
		"a forged SHA-256":         append(append([]byte(`{"chunked":`), encoded...), '}'),
		"the owner's first chunks": firstChunksManifest(t, kv, layout),
	}
}

// A manifest copied or borrowed under another session's key names the owner's chunks, so neither
// the write that replaces it, nor an unsubscribe of the copy's session, nor the deregistration that
// deletes it deletes them (copiedManifests names the three shapes). Each time the owner's record
// still reads whole and its chunks stay in the bucket. None of the three is a row of the copy's
// session: the cache keeps no row for it (decodeStoredLocked), a bucket read answers it with no
// layout (decodeEntry), so neither the write over it nor the unsubscribe deletes a chunk it names,
// and the deregistration (deleteInterest) deletes chunks only for the cache's row of the session of
// the generation it deleted, which there is none of. The control is the owner's own deregistration,
// which deletes them.
func TestACopiedOrBorrowedManifestLeavesTheOwnersChunksAlone(t *testing.T) {
	conn := kvchunktest.SmallPayloadNATS(t)
	reg := openRegistry(t, conn)
	kv := rawInterests(t, conn)
	topics := kvchunktest.Topics("owner", 400)
	head, layout := storeOwner(t, reg, kv, Interest{SessionID: "ses_owner", MachineID: "m1"}, topics)
	chunks := layout.Keys()
	want := sortedTopics(topics)
	for what, manifest := range copiedManifests(t, kv, head, layout) {
		for _, step := range []struct {
			name string
			do   func() error
		}{
			{"the write over " + what, func() error {
				_, err := reg.Upsert(Interest{SessionID: "ses_copy", MachineID: "m1"}, []string{"notifications.team.copy"})
				return err
			}},
			{"the unsubscribe of one topic of " + what, func() error {
				_, err := reg.Remove(t.Context(), "ses_copy", []string{topics[len(topics)-1]}, RoleReleaseAuthority{})
				return err
			}},
			{"the deregistration of " + what, func() error {
				_, err := reg.Remove(t.Context(), "ses_copy", nil, RoleReleaseAuthority{})
				return err
			}},
		} {
			copied, err := kv.Put("ses_copy", manifest)
			if err != nil {
				t.Fatalf("store %s: %v", what, err)
			}
			if what == "an exact copy" {
				awaitInterestApplied(t, reg, "ses_copy", copied)
				if row, ok := reg.Cached("ses_copy"); ok {
					t.Fatalf("the cache holds %+v for ses_copy, an exact copy of the owner's manifest, want no row: it names another session", row)
				}
			}
			if err := step.do(); err != nil {
				t.Fatalf("%s: %v", step.name, err)
			}
			requireOwnerWhole(t, reg, kv, chunks, want, "after "+step.name)
		}
	}
	if _, err := reg.Remove(t.Context(), "ses_owner", nil, RoleReleaseAuthority{}); err != nil {
		t.Fatalf("the owner's deregistration: %v", err)
	}
	// What the copy's unsubscribes stored and its manifest no longer names is the collection's.
	for _, key := range kvchunktest.LiveChunkKeys(t, kv) {
		if slices.Contains(chunks, key) {
			t.Fatalf("after the owner's own deregistration the bucket still holds %s, a chunk of the owner's record", key)
		}
	}
}

// A write over a manifest under another session's key that names the owner's chunks deletes none of
// them, also where this listener's cache is behind the owner's record, which no read of the cache can
// guard: the writer's watcher is stopped while a second listener rewrites the owner's record to a
// new generation, so the writer's cache still holds the owner's older row and no row of it names the
// generation the manifest names. An exact copy joins into the owner's record, which names the owner
// (ErrInterestUnbound); a manifest naming only the owner's first chunks joins into bytes that are no
// record (ErrMalformedInterest). Neither is a row of the copy's session, so a bucket read answers both
// with no layout (decodeEntry) and no write over them, merging or planned, and no unsubscribe deletes
// a chunk they name. The deregistration never reads them that way: deleteInterest deletes chunks only
// when this listener's cache holds the session's row of the generation the deleted manifest names,
// and the writer's cache holds no row for the copy's session. Each path's own outcome is checked
// too: the merging and the planned write answer, and store, the copy's session's own one-topic row,
// the unsubscribe leaves the record it cannot decode as it is, and the deregistration deletes it. The
// merging and the planned write once
// took the joined layout as the one they replaced and, once their head landed, deleted the owner's
// live chunks, after which every read of the owner answered a damaged record. The control is the
// owner's record read whole, and every chunk its manifest names in the bucket, before the manifest is
// written over.
func TestAWriteOverACopyOfANewerManifestThanTheCacheHoldsLeavesTheOwnerWhole(t *testing.T) {
	shapes := map[string]func(t *testing.T, kv natsgo.KeyValue, head []byte, layout kvchunk.Layout) []byte{
		"an exact copy": func(_ *testing.T, _ natsgo.KeyValue, head []byte, _ kvchunk.Layout) []byte { return head },
		"the owner's first chunks": func(t *testing.T, kv natsgo.KeyValue, _ []byte, layout kvchunk.Layout) []byte {
			return firstChunksManifest(t, kv, layout)
		},
	}
	copyRow := Interest{SessionID: "ses_copy", MachineID: "m1"}
	copyTopics := []string{"notifications.team.copy"}
	// Each path answers the row it wrote, or none for a path that writes no row.
	paths := map[string]func(t *testing.T, writer *Registry) (*Interest, error){
		"upsert": func(_ *testing.T, writer *Registry) (*Interest, error) {
			row, err := writer.Upsert(copyRow, copyTopics)
			return &row, err
		},
		"planned": func(t *testing.T, writer *Registry) (*Interest, error) {
			plan, err := writer.PlanInterest(t.Context(), copyRow.SessionID, copyTopics)
			if err != nil {
				return nil, err
			}
			row, err := writer.UpsertPlanned(t.Context(), copyRow, plan)
			return &row, err
		},
		"unsubscribe": func(t *testing.T, writer *Registry) (*Interest, error) {
			_, err := writer.Remove(t.Context(), copyRow.SessionID, copyTopics, RoleReleaseAuthority{})
			return nil, err
		},
		"deregistration": func(t *testing.T, writer *Registry) (*Interest, error) {
			_, err := writer.Remove(t.Context(), copyRow.SessionID, nil, RoleReleaseAuthority{})
			return nil, err
		},
	}
	for shape, manifestOf := range shapes {
		for path, do := range paths {
			t.Run(shape+"/"+path, func(t *testing.T) {
				conn := kvchunktest.SmallPayloadNATS(t)
				writer := openRegistry(t, conn)
				other := openRegistry(t, conn)
				kv := rawInterests(t, conn)
				first := kvchunktest.Topics("owner", 400)
				storeOwner(t, writer, kv, Interest{SessionID: "ses_owner", MachineID: "m1"}, first)
				cachedOwner := func() (storedRow, bool) {
					writer.mu.RLock()
					defer writer.mu.RUnlock()
					return writer.cache.Get("ses_owner")
				}
				older, ok := cachedOwner()
				if !ok || !older.layout.Chunked() {
					t.Fatalf("the writer's cache holds %+v for the owner (%t), want a chunked row", older.layout, ok)
				}
				writer.watcher.Stop()
				second := kvchunktest.Topics("rewritten", 400)
				head, newer := storeOwner(t, other, kv, Interest{SessionID: "ses_owner", MachineID: "m2"}, second)
				if lagging, _ := cachedOwner(); lagging.layout.Generation == newer.Generation {
					t.Fatalf("the writer's cache already holds the owner's generation %s, so the case is not the lagging one", newer.Generation)
				}
				want := sortedTopics(first, second)
				requireOwnerWhole(t, other, kv, newer.Keys(), want, "before the "+path+" over "+shape)
				manifest := manifestOf(t, kv, head, newer)
				if _, err := kv.Put(copyRow.SessionID, manifest); err != nil {
					t.Fatalf("store %s under %s: %v", shape, copyRow.SessionID, err)
				}
				answered, err := do(t, writer)
				if err != nil {
					t.Fatalf("the %s over %s: %v", path, shape, err)
				}
				if answered != nil && (answered.SessionID != copyRow.SessionID || !slices.Equal(answered.Topics, copyTopics)) {
					t.Fatalf("the %s over %s answered %+v, want %s's own row holding only %v", path, shape, *answered, copyRow.SessionID, copyTopics)
				}
				requireOwnerWhole(t, other, kv, newer.Keys(), want, "after the "+path+" over "+shape)
				requireCopyRow(t, other, kv, path, manifest, copyRow.SessionID, copyTopics)
				t.Logf("the writer's cache held generation %s of the owner, the bucket %s; the %s over %s left all %d of the owner's chunks",
					older.layout.Generation, newer.Generation, path, shape, len(newer.Keys()))
			})
		}
	}
}

// requireCopyRow fails t unless the bucket holds, under sessionID, what path left there over
// manifest, a record this build cannot decode: the session's own row holding only topics after a
// merging or a planned write (read through reg from the bucket), manifest itself after an
// unsubscribe, which leaves such a record as it is, and nothing after the deregistration.
func requireCopyRow(t *testing.T, reg *Registry, kv natsgo.KeyValue, path string, manifest []byte, sessionID string, topics []string) {
	t.Helper()
	switch path {
	case "upsert", "planned":
		row, _, err := reg.RefreshAt(t.Context(), sessionID)
		if err != nil || row.SessionID != sessionID || !slices.Equal(row.Topics, topics) {
			t.Fatalf("after the %s %s's stored row reads %+v (%v), want its own row holding only %v", path, sessionID, row, err, topics)
		}
	case "unsubscribe":
		entry, err := kv.Get(sessionID)
		if err != nil || !bytes.Equal(entry.Value(), manifest) {
			t.Fatalf("after the unsubscribe %s holds %v (%v), want the record it cannot decode left as it was", sessionID, entry, err)
		}
	case "deregistration":
		if _, err := kv.Get(sessionID); !errors.Is(err, natsgo.ErrKeyNotFound) {
			t.Fatalf("after the deregistration a read of %s answers %v, want %v", sessionID, err, natsgo.ErrKeyNotFound)
		}
	default:
		t.Fatalf("requireCopyRow: no path %q", path)
	}
}

// A delete that reads nothing - its leader read made just before the record's registration landed
// - deletes nothing, so the row the cache holds stays, even when the watcher has not yet delivered
// that row's chunks: a write-through stored it, and the read-back finds its own revision. Evicting it
// would fence the row out of the cache and leave its chunks to a collection that takes chunks no row
// names. The watcher is stopped here, so no chunk of the record reaches the chunk copy.
func TestADeleteThatReadNothingKeepsARowWhoseChunksHaveNotArrived(t *testing.T) {
	conn := kvchunktest.SmallPayloadNATS(t)
	reg := openRegistry(t, conn)
	useKV(t, reg, missingGetKeyValue{KeyValue: reg.watcher.KV().Legacy(), key: "ses_early", leader: reg.watcher.KV()})
	reg.watcher.Stop()
	topics := kvchunktest.Topics("early", 400)
	if _, err := reg.Upsert(Interest{SessionID: "ses_early", MachineID: "m1"}, topics); err != nil {
		t.Fatalf("store the record: %v", err)
	}
	if _, ok := reg.Cached("ses_early"); !ok {
		t.Fatal("the write-through stored no row, so the case has nothing to keep")
	}
	if err := reg.deleteInterest(t.Context(), "ses_early"); err != nil {
		t.Fatalf("the delete whose read found nothing: %v", err)
	}
	cached, ok := reg.Cached("ses_early")
	if !ok {
		t.Fatal("after a delete that deleted nothing the cache holds no row for the record that still stands")
	}
	requireTopics(t, "the row the cache kept", cached, sortedTopics(topics))
}
