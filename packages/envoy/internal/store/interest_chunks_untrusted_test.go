package store

// The cases of a chunked interest record that no write of this build makes, or that a delete reads
// back changed: forged, damaged, borrowed and copied manifests, a removal naming a chunk's key, and
// the cache after a delete that read nothing or found a record stored since. The storage and
// rewrite cases are in interest_chunks_test.go, whose helpers these share.

import (
	"context"
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
	reg.watcher = kvwatch.New("interest registry", bus.NewKeyValue(missingGetKeyValue{KeyValue: base.Legacy(), key: "ses_unread"},
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

// missingGetKeyValue is the interest bucket as a copy behind the leader answers a direct get of
// key that it has not applied: as no record at all.
type missingGetKeyValue struct {
	natsgo.KeyValue
	key string
}

func (kv missingGetKeyValue) Get(key string) (natsgo.KeyValueEntry, error) {
	if key == kv.key {
		return nil, natsgo.ErrKeyNotFound
	}
	return kv.KeyValue.Get(key)
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
	useKV(t, reg, missingGetKeyValue{KeyValue: legacy, key: "ses_kept"})
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
// it deletes none of them: a manifest under one session's key that names another session's live
// chunks with a SHA-256 they do not join into is replaced by that session's next write, and the
// other session's record still reads whole.
func TestAWriteOverADamagedManifestDeletesNoChunkItNames(t *testing.T) {
	conn := kvchunktest.SmallPayloadNATS(t)
	reg := openRegistry(t, conn)
	kv := rawInterests(t, conn)
	topics := kvchunktest.Topics("owner", 400)
	if _, err := reg.Upsert(Interest{SessionID: "ses_owner", MachineID: "m1"}, topics); err != nil {
		t.Fatalf("store the owner's record: %v", err)
	}
	head, err := kv.Get("ses_owner")
	if err != nil {
		t.Fatalf("read the owner's manifest: %v", err)
	}
	layout, err := kvchunk.ReadLayout(head.Value())
	if err != nil || !layout.Chunked() {
		t.Fatalf("the owner's key holds %.60q (%v), want a manifest", head.Value(), err)
	}
	layout.SHA256 = strings.Repeat("0", 64)
	forged, err := json.Marshal(layout)
	if err != nil {
		t.Fatalf("encode the forged manifest: %v", err)
	}
	if _, err := kv.Put("ses_borrower", append(append([]byte(`{"chunked":`), forged...), '}')); err != nil {
		t.Fatalf("store the forged manifest: %v", err)
	}
	if _, _, err := reg.RefreshAt(t.Context(), "ses_borrower"); !errors.Is(err, ErrMalformedInterest) {
		t.Fatalf("a bucket read of the borrowed manifest answered %v, want ErrMalformedInterest", err)
	}
	if _, err := reg.Upsert(Interest{SessionID: "ses_borrower", MachineID: "m1"}, []string{"notifications.team.borrower"}); err != nil {
		t.Fatalf("the write over the borrowed manifest: %v", err)
	}
	row, _, err := reg.RefreshAt(t.Context(), "ses_owner")
	if err != nil {
		t.Fatalf("after the write over the borrowed manifest the owner's record reads %v, want it whole", err)
	}
	requireTopics(t, "the owner's record", row, sortedTopics(topics))
}

// A manifest copied or borrowed under another session's key names the owner's chunks, so neither
// the write that replaces it, nor an unsubscribe of the copy's session, nor the deregistration that
// deletes it deletes them: an exact copy, which the cache joins and holds as a row of its own, and
// one with a forged SHA-256, which no read joins. Each time the owner's record still reads whole and
// its chunks stay in the bucket. An exact copy joins into the owner's own record, naming the owner's
// session, so a registration of the copy's session targets the owner's key at a revision that is
// not its and is refused; its unsubscribe and its deregistration are its steps here. The control is
// the owner's own deregistration, which deletes them.
func TestACopiedOrBorrowedManifestLeavesTheOwnersChunksAlone(t *testing.T) {
	conn := kvchunktest.SmallPayloadNATS(t)
	reg := openRegistry(t, conn)
	kv := rawInterests(t, conn)
	topics := kvchunktest.Topics("owner", 400)
	if _, err := reg.Upsert(Interest{SessionID: "ses_owner", MachineID: "m1"}, topics); err != nil {
		t.Fatalf("store the owner's record: %v", err)
	}
	head, err := kv.Get("ses_owner")
	if err != nil {
		t.Fatalf("read the owner's manifest: %v", err)
	}
	chunks := kvchunktest.LiveChunkKeys(t, kv)
	borrowed, err := kvchunk.ReadLayout(head.Value())
	if err != nil {
		t.Fatalf("read the owner's layout: %v", err)
	}
	borrowed.SHA256 = strings.Repeat("0", 64)
	forged, err := json.Marshal(borrowed)
	if err != nil {
		t.Fatalf("encode the borrowed manifest: %v", err)
	}
	manifests := map[string][]byte{"an exact copy": head.Value(), "a forged SHA-256": append(append([]byte(`{"chunked":`), forged...), '}')}
	requireOwnerWhole := func(after string) {
		t.Helper()
		row, _, err := reg.RefreshAt(t.Context(), "ses_owner")
		if err != nil {
			t.Fatalf("after %s the owner's record reads %v, want it whole", after, err)
		}
		requireTopics(t, "the owner's record after "+after, row, sortedTopics(topics))
		// The copy's own unsubscribe stores chunks of its own beside the owner's; the owner's stay.
		live := kvchunktest.LiveChunkKeys(t, kv)
		for _, key := range chunks {
			if !slices.Contains(live, key) {
				t.Fatalf("after %s the bucket no longer holds %s, a chunk of the owner's record (it holds %v)", after, key, live)
			}
		}
	}
	for what, manifest := range manifests {
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
			if what == "an exact copy" && strings.HasPrefix(step.name, "the write") {
				continue
			}
			if _, err := kv.Put("ses_copy", manifest); err != nil {
				t.Fatalf("store %s: %v", what, err)
			}
			if what == "an exact copy" {
				waitFor(t, 10*time.Second, func() bool { _, ok := reg.Cached("ses_copy"); return ok })
			}
			if err := step.do(); err != nil {
				t.Fatalf("%s: %v", step.name, err)
			}
			requireOwnerWhole(step.name)
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

// A delete that reads nothing - a copy of the stream behind the leader answering its direct get -
// deletes nothing, so the row the cache holds stays, even when the watcher has not yet delivered
// that row's chunks: a write-through stored it, and the read-back finds its own revision. Evicting it
// would fence the row out of the cache and leave its chunks to a collection that takes chunks no row
// names. The watcher is stopped here, so no chunk of the record reaches the chunk copy.
func TestADeleteThatReadNothingKeepsARowWhoseChunksHaveNotArrived(t *testing.T) {
	conn := kvchunktest.SmallPayloadNATS(t)
	reg := openRegistry(t, conn)
	useKV(t, reg, missingGetKeyValue{KeyValue: reg.watcher.KV().Legacy(), key: "ses_early"})
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
