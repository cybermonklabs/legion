package kvwatch_test

import (
	"context"
	"errors"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/kvwatch"
	"github.com/sjawhar/envoy/internal/testnats"
)

// The keys of this file's tests.
const (
	// writtenInTheWindow is written into the bucket, and into the copy as a write-through, after
	// the read's watch made its consumer and before the read began: the read's scan does not
	// deliver it, and the bucket holds it.
	writtenInTheWindow = "written-in-the-window"
	// deletedFromTheBucket is written the same way, then deleted from the bucket alone: the copy
	// is not told, so only the read's own look at the bucket can remove it.
	deletedFromTheBucket = "deleted-from-the-bucket"
	// goneFromTheBucket is a row the copy holds whose every record was purged from the bucket
	// before the read's watch read the stream, leaving no delete marker: no read delivers it again.
	goneFromTheBucket = "gone-from-the-bucket"
)

// A write that lands after a read's watch made its consumer and before the read began
// (Cache.BeginRead) is not in that read's scan: the consumer delivers it only after the scan's end.
// The read's completion must keep the row such a write left in the copy, which the bucket holds,
// rather than sweep it as a row the read found no evidence of: a swept row is missing from a copy
// that serves as complete until that later delivery, and TestSessionsHandler_IncludesTitle
// (cmd/listener) saw a session it had just written go missing so (INFRA-4048). Every read path
// begins its read this way: the first read (Start), a reconnect's read (here WatchOn on the handle
// a Rewatch would open, held after its consumer is made), a resync (a scheduled read) and the read
// of a bucket deleted and created again. Each write goes as a store's does: through the epoch the
// watcher's Handle names before the write is made.
//
// The other two keys are the control that the read still removes what the bucket does not hold: a
// key deleted from the bucket in the same window, which the copy was never told of, and a row
// whose records the bucket no longer holds at all. A read that kept every row it did not see would
// fail both.
func TestAReadKeepsAWriteThatLandedAfterItsConsumerWasMade(t *testing.T) {
	for _, path := range []struct {
		name string
		// read is which read the watcher begins is the one under test: 1 for its first.
		read int
		// recreated is whether the read under test reads a bucket deleted and created again.
		recreated bool
		// handle is the handle the watcher starts on, whose watch held, when it holds one, is the
		// read under test's.
		handle func(kv bus.KeyValue, held *heldCreate) bus.KeyValue
		// options are the watcher's options beyond the ones every path sets.
		options []kvwatch.Option
		// begin begins the read under test, whose watch holds in held once it made its consumer,
		// and returns what waits for begin's own call to return once held lets the watch go.
		begin func(t *testing.T, w *kvwatch.Watcher, conn *natsgo.Conn, kv bus.KeyValue, held *heldCreate, timers *pending) func()
	}{
		{
			name:   "the first read",
			read:   1,
			handle: func(kv bus.KeyValue, held *heldCreate) bus.KeyValue { return scripted(kv, held.step()).handle() },
			begin: func(_ *testing.T, w *kvwatch.Watcher, _ *natsgo.Conn, _ bus.KeyValue, _ *heldCreate, _ *pending) func() {
				w.Start()
				return func() {}
			},
		},
		{
			name:   "a reconnect's read",
			read:   2,
			handle: func(kv bus.KeyValue, _ *heldCreate) bus.KeyValue { return kv },
			begin: func(t *testing.T, w *kvwatch.Watcher, _ *natsgo.Conn, kv bus.KeyValue, held *heldCreate, _ *pending) func() {
				w.Disconnected()
				return watchOnInBackground(t, w, scripted(kv, held.step()).handle())
			},
		},
		{
			name: "a resync",
			read: 2,
			handle: func(kv bus.KeyValue, held *heldCreate) bus.KeyValue {
				return scripted(kv, nil, held.step()).handle()
			},
			options: []kvwatch.Option{kvwatch.WithScheduledRead(scheduledEvery)},
			begin: func(t *testing.T, _ *kvwatch.Watcher, _ *natsgo.Conn, _ bus.KeyValue, _ *heldCreate, timers *pending) func() {
				scheduled := timers.last(t, scheduledEvery)
				done := make(chan struct{})
				go func() {
					defer close(done)
					scheduled.fire()
				}()
				return func() {
					t.Helper()
					wait(t, done, "the scheduled read's watch to return")
				}
			},
		},
		{
			name:      "a recreated bucket's read",
			read:      2,
			recreated: true,
			handle:    func(kv bus.KeyValue, _ *heldCreate) bus.KeyValue { return kv },
			begin: func(t *testing.T, w *kvwatch.Watcher, conn *natsgo.Conn, _ bus.KeyValue, held *heldCreate, _ *pending) func() {
				js := jetStream(t, conn)
				deleteTestBucket(t, js)
				recreated := testnats.RecreateKeyValue(t, js, &natsgo.KeyValueConfig{Bucket: testBucketName})
				return watchOnInBackground(t, w, scripted(wrap(t, conn, recreated), held.step()).handle())
			},
		},
	} {
		t.Run(path.name, func(t *testing.T) {
			uri := testnats.URL(t)
			conn, kv := bucket(t, uri)
			goneRevision, err := kv.Put(goneFromTheBucket, []byte("1"))
			if err != nil {
				t.Fatalf("put %s: %v", goneFromTheBucket, err)
			}
			held := newHeldCreate(t)
			cache := newWindowCache(path.read)
			timers := &pending{}
			options := append([]kvwatch.Option{kvwatch.WithAfterFunc(timers.afterFunc()), kvwatch.WithStreamProbeInterval(0)}, path.options...)
			w := kvwatch.New("test cache", path.handle(kv, held), cache, options...)
			t.Cleanup(w.Stop)
			// Runs before Stop: a delivery held after the read under test completed holds the
			// watcher's apply lock.
			t.Cleanup(cache.letGo)

			// The copy holds the gone key before the read under test: the first read's copy as a
			// write-through left it, a later read's as the first read delivered it.
			if path.read == 1 {
				if outcome := cache.put(w, goneFromTheBucket, goneRevision); outcome != kvwatch.Applied {
					t.Fatalf("the write of %s into the copy came to outcome %d, want Applied (%d)", goneFromTheBucket, outcome, kvwatch.Applied)
				}
			} else {
				w.Start()
				eventually(t, "the first read's completion", func() bool { return len(cache.completions()) == 1 })
				if !cache.has(goneFromTheBucket) {
					t.Fatalf("the first read did not deliver %s", goneFromTheBucket)
				}
			}
			purgeKey(t, conn, goneFromTheBucket)
			if _, err := kv.Get(goneFromTheBucket); !errors.Is(err, natsgo.ErrKeyNotFound) {
				t.Fatalf("read %s after the purge: %v, want %v", goneFromTheBucket, err, natsgo.ErrKeyNotFound)
			}

			returned := path.begin(t, w, conn, kv, held, timers)
			wait(t, held.created, "the read's watch to make its consumer")
			// The window: the watch has made its consumer and holds before it takes the watcher's
			// locks, so the read has not begun.
			for _, key := range []string{writtenInTheWindow, deletedFromTheBucket} {
				if outcome := cache.writeThrough(t, w, key); outcome != kvwatch.Applied {
					t.Fatalf("the write of %s into the copy came to outcome %d, want Applied (%d)", key, outcome, kvwatch.Applied)
				}
			}
			if err := kv.Delete(deletedFromTheBucket); err != nil {
				t.Fatalf("delete %s from the bucket: %v", deletedFromTheBucket, err)
			}

			held.let()
			returned()
			eventually(t, "the read's completion", cache.targetDone)
			if reads := cache.reads(); len(reads) != path.read || reads[path.read-1].Recreated != path.recreated {
				t.Fatalf("reads begun %+v, want %d, the last one Recreated %v", reads, path.read, path.recreated)
			}
			if got, want := bucketKeys(t, kv), []string{writtenInTheWindow}; !slices.Equal(got, want) {
				t.Fatalf("the bucket holds %v, want %v", got, want)
			}
			// The control first: the read still removes what the bucket does not hold.
			if cache.has(deletedFromTheBucket) || cache.has(goneFromTheBucket) {
				t.Fatalf("the completed read left the copy holding %v, want %s and %s out of it", cache.keyList(), deletedFromTheBucket, goneFromTheBucket)
			}
			if !cache.has(writtenInTheWindow) {
				t.Fatalf("the completed read swept %s, which the bucket holds: the copy holds %v", writtenInTheWindow, cache.keyList())
			}

			// Once the deliveries after the scan go on, the copy holds what the bucket holds.
			cache.letGo()
			eventually(t, "the copy to hold what the bucket holds", func() bool {
				return slices.Equal(cache.keyList(), bucketKeys(t, kv))
			})
		})
	}
}

// A store captures the handle and its epoch before it writes (Watcher.Handle; the session
// registry's PutWithin among them), so a write it began before a bucket was deleted and created
// again carries the deleted bucket's epoch even when it lands in the new bucket. Landed after the
// new bucket's read made its consumer, and taken by the copy before that read was installed, it
// is in no scan, and no epoch can say which bucket took it: the read keeps the row because the
// bucket holds the key.
func TestARecreatedReadKeepsAWriteThroughAHandleCapturedBeforeIt(t *testing.T) {
	uri := testnats.URL(t)
	conn, kv := bucket(t, uri)
	cache := newWindowCache(2)
	timers := &pending{}
	w := kvwatch.New("test cache", kv, cache, kvwatch.WithAfterFunc(timers.afterFunc()), kvwatch.WithStreamProbeInterval(0))
	t.Cleanup(w.Stop)
	t.Cleanup(cache.letGo)
	w.Start()
	eventually(t, "the first read's completion", func() bool { return len(cache.completions()) == 1 })
	oldKV, oldEpoch := w.Handle()

	js := jetStream(t, conn)
	deleteTestBucket(t, js)
	recreated := testnats.RecreateKeyValue(t, js, &natsgo.KeyValueConfig{Bucket: testBucketName})
	held := newHeldCreate(t)
	returned := watchOnInBackground(t, w, scripted(wrap(t, conn, recreated), held.step()).handle())
	wait(t, held.created, "the recreated bucket's watch to make its consumer")
	if outcome := cache.putThrough(t, w, oldKV, oldEpoch, writtenInTheWindow); outcome != kvwatch.Applied {
		t.Fatalf("the write through the handle captured before the recreate came to outcome %d, want Applied (%d)", outcome, kvwatch.Applied)
	}
	held.let()
	returned()
	eventually(t, "the recreated bucket's read to complete", cache.targetDone)

	if reads := cache.reads(); len(reads) != 2 || !reads[1].Recreated {
		t.Fatalf("reads begun %+v, want the first and a recreated one", reads)
	}
	if got, want := bucketKeys(t, kv), []string{writtenInTheWindow}; !slices.Equal(got, want) {
		t.Fatalf("the new bucket holds %v, want %v", got, want)
	}
	if !cache.has(writtenInTheWindow) {
		t.Fatalf("the recreated bucket's read swept %s, which the new bucket holds: the copy holds %v", writtenInTheWindow, cache.keyList())
	}
}

// A delete a store made in the deleted bucket can reach the copy late, as the session reaper's
// does: it deletes in the bucket, then takes the cache's lock and evicts
// (SessionRegistry.deleteGone). Reached after a write of the same key in the new bucket, which
// landed after the new read's consumer was made, it removes that write's row, and no scan delivers
// the write. The read sets the key to what the bucket holds before it completes.
func TestARecreatedReadRestoresARowADelayedDeleteOfTheOldBucketRemoved(t *testing.T) {
	const key = "deleted-then-written-again"
	uri := testnats.URL(t)
	conn, kv := bucket(t, uri)
	if _, err := kv.Put(key, []byte("old")); err != nil {
		t.Fatalf("put %s in the old bucket: %v", key, err)
	}
	cache := newWindowCache(2)
	timers := &pending{}
	w := kvwatch.New("test cache", kv, cache, kvwatch.WithAfterFunc(timers.afterFunc()), kvwatch.WithStreamProbeInterval(0))
	t.Cleanup(w.Stop)
	t.Cleanup(cache.letGo)
	w.Start()
	eventually(t, "the first read's completion", func() bool { return len(cache.completions()) == 1 })
	if !cache.has(key) {
		t.Fatalf("the first read did not deliver %s", key)
	}
	// The delete lands in the old bucket; the copy hears of it only below.
	oldKV, oldEpoch := w.Handle()
	if err := oldKV.Delete(key); err != nil {
		t.Fatalf("delete %s from the old bucket: %v", key, err)
	}
	marker := lastRevision(t, oldKV, key)

	js := jetStream(t, conn)
	deleteTestBucket(t, js)
	recreated := testnats.RecreateKeyValue(t, js, &natsgo.KeyValueConfig{Bucket: testBucketName})
	held := newHeldCreate(t)
	returned := watchOnInBackground(t, w, scripted(wrap(t, conn, recreated), held.step()).handle())
	wait(t, held.created, "the recreated bucket's watch to make its consumer")
	// The write lands in the new bucket; whatever the fence makes of it, the late delete then
	// removes the key's row.
	cache.writeThrough(t, w, key)
	if outcome := cache.evictAt(w, oldEpoch, key, marker); outcome != kvwatch.Applied || cache.has(key) {
		t.Fatalf("the late delete came to outcome %d with the row kept %v, want Applied (%d) and the row gone", outcome, cache.has(key), kvwatch.Applied)
	}
	held.let()
	returned()
	eventually(t, "the recreated bucket's read to complete", cache.targetDone)

	if got, want := bucketKeys(t, kv), []string{key}; !slices.Equal(got, want) {
		t.Fatalf("the new bucket holds %v, want %v", got, want)
	}
	if !cache.has(key) {
		t.Fatalf("the recreated bucket's read left %s out of the copy, which the new bucket holds: the copy holds %v", key, cache.keyList())
	}
}

// A watch whose stream read is answered late, with a stream since replaced, is discarded once a
// newer watch has installed (watch's older test). It must leave nothing behind that changes how
// the copy takes a write: a write-through older than a record the watcher delivered is still
// fenced out, and a write that lands in the window of a later read through the public Rewatch is
// still kept.
func TestAReadAfterAStaleWatchWasDiscardedKeepsItsWindowWrite(t *testing.T) {
	const deliveredTwice = "delivered-twice"
	uri := testnats.URL(t)
	conn, kv := bucket(t, uri)
	cache := newWindowCache(3)
	timers := &pending{}
	w := kvwatch.New("test cache", kv, cache, kvwatch.WithAfterFunc(timers.afterFunc()), kvwatch.WithStreamProbeInterval(0))
	t.Cleanup(w.Stop)
	t.Cleanup(cache.letGo)
	w.Start()
	eventually(t, "the first read's completion", func() bool { return len(cache.completions()) == 1 })

	// The stale watch reads the old stream and is held with that answer in hand.
	stale := &heldStreamRead{JetStream: kv.Streams(), answered: make(chan struct{}), release: make(chan struct{})}
	t.Cleanup(stale.let)
	staleReturned := watchOnInBackground(t, w, bus.NewKeyValue(kv.Legacy(), kv.WatchHandle(), stale))
	wait(t, stale.answered, "the stale watch's stream read")

	js := jetStream(t, conn)
	deleteTestBucket(t, js)
	testnats.RecreateKeyValue(t, js, &natsgo.KeyValueConfig{Bucket: testBucketName})
	if err := w.Rewatch(context.Background(), conn); err != nil {
		t.Fatalf("the Rewatch onto the new bucket: %v", err)
	}
	eventually(t, "the new bucket's read to complete", func() bool { return len(cache.completions()) == 2 })
	stale.let()
	staleReturned()
	if reads := cache.reads(); len(reads) != 2 || !reads[1].Recreated {
		t.Fatalf("reads begun %+v, want the first and the new bucket's, the stale watch discarded", reads)
	}

	// A write-through older than what the watcher delivered is fenced out.
	first, err := kv.Put(deliveredTwice, []byte("1"))
	if err != nil {
		t.Fatalf("put %s: %v", deliveredTwice, err)
	}
	second, err := kv.Put(deliveredTwice, []byte("2"))
	if err != nil {
		t.Fatalf("put %s again: %v", deliveredTwice, err)
	}
	eventually(t, "the watcher's delivery of the second write", func() bool { return cache.fenceOf(w, deliveredTwice) == second })
	_, epoch := w.Handle()
	if outcome := cache.putAt(w, epoch, deliveredTwice, first); outcome != kvwatch.Fenced {
		t.Fatalf("a write-through at revision %d, after the watcher delivered %d, came to outcome %d, want Fenced (%d)", first, second, outcome, kvwatch.Fenced)
	}

	// A write in the window of a read through the public Rewatch is kept.
	cache.beforeTarget(func(kvwatch.Read) {
		if outcome := cache.writeThrough(t, w, writtenInTheWindow); outcome != kvwatch.Applied {
			t.Errorf("the write of %s into the copy came to outcome %d, want Applied (%d)", writtenInTheWindow, outcome, kvwatch.Applied)
		}
	})
	if err := w.Rewatch(context.Background(), conn); err != nil {
		t.Fatalf("the Rewatch whose window holds the write: %v", err)
	}
	eventually(t, "the Rewatch's read to complete", cache.targetDone)
	if !cache.has(writtenInTheWindow) {
		t.Fatalf("the Rewatch's read swept %s, which the bucket holds: the copy holds %v", writtenInTheWindow, cache.keyList())
	}
}

// A JetStream restore of a snapshot can keep the stream's creation time (nats-server v2.15.1
// server/stream.go:9622-9623 sets it from the snapshot's metadata when that names one), and then
// the watcher reads the restored bucket as the same one, with a sequence space that moved back.
// A row the copy took after the snapshot, at a revision past the restored stream's last sequence,
// is a key the restored bucket holds no record of: every read of the restored bucket removes it.
//
// The in-process server this rig runs stamps the restored stream with the time of the restore for
// the snapshot taken here, so the watcher's handle reports every stream read with the original
// creation time (createdAs), which is what it would read from a restore that kept it; each read is
// installed through WatchOn on that handle, as a Rewatch installs one on the handle it opens.
func TestReadsOfARestoredSnapshotRemoveARowTheSnapshotLacks(t *testing.T) {
	const (
		stream        = "KV_" + testBucketName
		accountKeeper = "kvwatch-test-account-keeper"
		inSnapshot    = "in-the-snapshot"
		afterSnapshot = "after-the-snapshot"
	)
	_, uri := testnats.Start(t)
	conn := testnats.Connect(t, uri)
	t.Cleanup(conn.Close)
	js := jetStream(t, conn)
	// The keeper keeps the account's streams directory from emptying on the delete below, which
	// the restore would race (TestARewatchOntoABucketRestoredWithAnOlderStreamRefillsTheCache).
	if _, err := js.CreateKeyValue(&natsgo.KeyValueConfig{Bucket: accountKeeper}); err != nil {
		t.Fatalf("create the account keeper bucket: %v", err)
	}
	legacy, err := js.CreateKeyValue(&natsgo.KeyValueConfig{Bucket: testBucketName})
	if err != nil {
		t.Fatalf("create the bucket: %v", err)
	}
	kv := wrap(t, conn, legacy)
	original, err := kv.ReadStreamState(context.Background())
	if err != nil {
		t.Fatalf("read the bucket's stream: %v", err)
	}
	handle := bus.NewKeyValue(kv.Legacy(), kv.WatchHandle(), createdAs{JetStream: kv.Streams(), created: original.Created})
	if _, err := kv.Put(inSnapshot, []byte("1")); err != nil {
		t.Fatalf("put %s: %v", inSnapshot, err)
	}
	snap := snapshotStream(t, conn, stream)
	if _, err := kv.Put(afterSnapshot, []byte("1")); err != nil {
		t.Fatalf("put %s: %v", afterSnapshot, err)
	}
	into := newSeen()
	timers := &pending{}
	w := kvwatch.New("test cache", handle, into, kvwatch.WithAfterFunc(timers.afterFunc()), kvwatch.WithStreamProbeInterval(0))
	t.Cleanup(w.Stop)
	w.Start()
	eventually(t, "the first read's completion", func() bool { return len(into.completions()) == 1 })
	if !into.has(inSnapshot) || !into.has(afterSnapshot) {
		t.Fatalf("the first read left the copy holding %v, want both keys", into.keyList())
	}

	if err := js.DeleteKeyValue(testBucketName); err != nil {
		t.Fatalf("delete the bucket: %v", err)
	}
	restoreStream(t, conn, stream, snap)
	restored, err := handle.ReadStreamState(context.Background())
	if err != nil || !restored.Created.Equal(original.Created) || restored.LastSeq >= 2 {
		t.Fatalf("the restored stream reads %+v, %v; want the original creation time %v and a last sequence below the copy's row at 2",
			restored, err, original.Created)
	}
	for read := 2; read <= 3; read++ {
		if err := kvwatch.WatchOn(w, handle); err != nil {
			t.Fatalf("read %d of the restored bucket: %v", read, err)
		}
		eventually(t, "the restored bucket's read to complete", func() bool { return len(into.completions()) == read })
		if reads := into.reads(); len(reads) != read || reads[read-1].Recreated {
			t.Fatalf("reads begun %+v, want %d, the last one read as the same bucket", reads, read)
		}
		if into.has(afterSnapshot) || !into.has(inSnapshot) {
			t.Fatalf("read %d of the restored bucket left the copy holding %v, want only %s", read, into.keyList(), inSnapshot)
		}
	}
}

// A store's delete of a key the bucket does not hold makes no record (bus.KeyValue.DeleteAtRead
// answers 0), and the copy is fenced at the revision past it, as the session registry's
// reconcileDeleted fences it. No record stands behind such a fence: the next complete read removes
// it, however low the bucket's sequence, so one fence per deleted key cannot pile up.
func TestACompleteReadReclaimsTheFencesOfDeletesOfAbsentKeys(t *testing.T) {
	absent := []string{"absent-a", "absent-b", "absent-c"}
	uri := testnats.URL(t)
	_, kv := bucket(t, uri)
	into := newSeen()
	timers := &pending{}
	w := kvwatch.New("test cache", kv, into, kvwatch.WithAfterFunc(timers.afterFunc()), kvwatch.WithStreamProbeInterval(0),
		kvwatch.WithScheduledRead(scheduledEvery))
	t.Cleanup(w.Stop)
	w.Start()
	eventually(t, "the first read's completion", func() bool { return len(into.completions()) == 1 })

	for _, key := range absent {
		revision, err := kv.DeleteAtRead(key)
		if err != nil || revision != 0 {
			t.Fatalf("delete absent %s: revision %d, %v; want 0 and no error", key, revision, err)
		}
		if outcome := into.evictAt(w, w.Epoch(), key, revision+1); outcome != kvwatch.Applied {
			t.Fatalf("the delete of %s came to outcome %d in the copy, want Applied (%d)", key, outcome, kvwatch.Applied)
		}
		if fence := into.fenceOf(w, key); fence != 1 {
			t.Fatalf("the copy's fence for %s is %d after its delete, want 1", key, fence)
		}
	}

	timers.last(t, scheduledEvery).fire()
	eventually(t, "the scheduled read's completion", func() bool { return len(into.completions()) == 2 })
	for _, key := range absent {
		if fence := into.fenceOf(w, key); fence != 0 {
			t.Fatalf("the copy's fence for %s is %d after a complete read of a bucket that holds no record of it, want it reclaimed", key, fence)
		}
	}
}

// Two recreations in a row: a read of the first new bucket is installed after the second new
// bucket's read made its consumer, and a write lands in the second new bucket in that time. The
// read of the second new bucket keeps it.
func TestTwoRecreationsInARowKeepTheLaterBucketsWindowWrite(t *testing.T) {
	uri := testnats.URL(t)
	conn, kv := bucket(t, uri)
	cache := newWindowCache(3)
	timers := &pending{}
	w := kvwatch.New("test cache", kv, cache, kvwatch.WithAfterFunc(timers.afterFunc()), kvwatch.WithStreamProbeInterval(0))
	t.Cleanup(w.Stop)
	t.Cleanup(cache.letGo)
	w.Start()
	eventually(t, "the first read's completion", func() bool { return len(cache.completions()) == 1 })

	js := jetStream(t, conn)
	deleteTestBucket(t, js)
	first := testnats.RecreateKeyValue(t, js, &natsgo.KeyValueConfig{Bucket: testBucketName})
	heldFirst := newHeldCreate(t)
	firstReturned := watchOnInBackground(t, w, scripted(wrap(t, conn, first), heldFirst.step()).handle())
	wait(t, heldFirst.created, "the first new bucket's watch to make its consumer")

	deleteTestBucket(t, js)
	second := testnats.RecreateKeyValue(t, js, &natsgo.KeyValueConfig{Bucket: testBucketName})
	heldSecond := newHeldCreate(t)
	secondReturned := watchOnInBackground(t, w, scripted(wrap(t, conn, second), heldSecond.step()).handle())
	wait(t, heldSecond.created, "the second new bucket's watch to make its consumer")

	if outcome := cache.writeThrough(t, w, writtenInTheWindow); outcome != kvwatch.Applied {
		t.Fatalf("the write of %s into the copy came to outcome %d, want Applied (%d)", writtenInTheWindow, outcome, kvwatch.Applied)
	}
	heldFirst.let()
	firstReturned()
	heldSecond.let()
	secondReturned()
	eventually(t, "the second new bucket's read to complete", cache.targetDone)

	if reads := cache.reads(); len(reads) != 3 || !reads[1].Recreated || !reads[2].Recreated {
		t.Fatalf("reads begun %+v, want the first and two recreated ones", reads)
	}
	if got, want := bucketKeys(t, kv), []string{writtenInTheWindow}; !slices.Equal(got, want) {
		t.Fatalf("the second new bucket holds %v, want %v", got, want)
	}
	if !cache.has(writtenInTheWindow) {
		t.Fatalf("the second new bucket's read swept %s, which that bucket holds: the copy holds %v", writtenInTheWindow, cache.keyList())
	}
}

// A read whose barrier cannot be written removes nothing and does not complete: it is read again,
// and only a read that reads its consumer past a barrier removes the row the bucket does not hold.
func TestAReadWhoseBarrierCannotBeWrittenRemovesNothing(t *testing.T) {
	const (
		inTheBucket    = "in-the-bucket"
		heldByTheCopy  = "held-by-the-copy"
		rereadAfter    = 5 * time.Second
		readsFailUntil = "the barrier writes fail until the test lets them"
	)
	uri := testnats.URL(t)
	_, kv := bucket(t, uri)
	putKeys(t, kv, inTheBucket)
	failing := &atomic.Bool{}
	failing.Store(true)
	handle := bus.NewKeyValue(kv.Legacy(), kv.WatchHandle(), failingBarriers{JetStream: kv.Streams(), failing: failing})
	into := newSeen()
	into.hold(heldByTheCopy)
	timers := &pending{}
	w := kvwatch.New("test cache", handle, into, kvwatch.WithAfterFunc(timers.afterFunc()), kvwatch.WithStreamProbeInterval(0))
	t.Cleanup(w.Stop)
	w.Start()

	eventually(t, "the read to complete or to be read again", func() bool {
		return len(into.completions()) > 0 || len(timers.armed(rereadAfter)) > 0
	})
	if done := len(into.completions()); done != 0 || w.Following() || !into.has(heldByTheCopy) || !into.has(inTheBucket) {
		t.Fatalf("after the barrier write failed (%s): %d reads completed, Following %v, copy %v; want none completed, not following, and both keys kept",
			readsFailUntil, done, w.Following(), into.keyList())
	}

	failing.Store(false)
	timers.last(t, rereadAfter).fire()
	eventually(t, "the read after the barrier is written to complete", func() bool { return len(into.completions()) == 1 })
	if into.has(heldByTheCopy) || !into.has(inTheBucket) {
		t.Fatalf("after the barrier was written, the copy holds %v, want only %s", into.keyList(), inTheBucket)
	}
}

// windowCache is a seen that watches one read, the target'th the watcher begins: it runs before
// that read's BeginRead what beforeTarget set, and once the read has completed holds every record
// delivered after it until letGo, so the copy that read's completion left can be read before any
// later delivery changes it. Each test makes its window writes while the read's consumer is held
// after it was made (heldCreate), so none of them is in the read's scan.
type windowCache struct {
	*seen
	target  int
	let     chan struct{}
	letOnce sync.Once

	mu     sync.Mutex
	begun  int
	done   bool
	before func(kvwatch.Read)
}

func newWindowCache(target int) *windowCache {
	return &windowCache{seen: newSeen(), target: target, let: make(chan struct{})}
}

// beforeTarget sets what runs before the target read's BeginRead: after its watch made its
// consumer and installed, before the copy begins the read.
func (c *windowCache) beforeTarget(before func(kvwatch.Read)) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.before = before
}

func (c *windowCache) BeginRead(read kvwatch.Read) {
	c.mu.Lock()
	c.begun++
	var before func(kvwatch.Read)
	if c.begun == c.target {
		before = c.before
	}
	c.mu.Unlock()
	if before != nil {
		before(read)
	}
	c.seen.BeginRead(read)
}

func (c *windowCache) Apply(entry kvwatch.Entry, read kvwatch.Read) {
	c.mu.Lock()
	done := c.done
	c.mu.Unlock()
	if done {
		<-c.let
	}
	c.seen.Apply(entry, read)
}

func (c *windowCache) CompleteRead(read kvwatch.Read) {
	c.seen.CompleteRead(read)
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.begun == c.target {
		c.done = true
	}
}

// targetDone reports whether the target read has completed.
func (c *windowCache) targetDone() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.done
}

func (c *windowCache) letGo() { c.letOnce.Do(func() { close(c.let) }) }

// putThrough writes key into the bucket through kv and hands the copy that write as a store's
// write-through does once it answered, through epoch, the epoch the store captured with kv
// (kvwatch.Copy.Put). It returns what became of the write in the copy.
func (s *seen) putThrough(t *testing.T, w *kvwatch.Watcher, kv bus.KeyValue, epoch uint64, key string) kvwatch.Outcome {
	t.Helper()
	revision, err := kv.Put(key, []byte("1"))
	if err != nil {
		t.Fatalf("put %s: %v", key, err)
	}
	return s.putAt(w, epoch, key, revision)
}

// writeThrough is putThrough through the handle and epoch the watcher's Handle names now.
func (s *seen) writeThrough(t *testing.T, w *kvwatch.Watcher, key string) kvwatch.Outcome {
	t.Helper()
	kv, epoch := w.Handle()
	return s.putThrough(t, w, kv, epoch, key)
}

// putAt hands the copy key at revision through a handle of epoch (kvwatch.Copy.Put).
func (s *seen) putAt(w *kvwatch.Watcher, epoch uint64, key string, revision uint64) kvwatch.Outcome {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.keys.Put(w, epoch, key, true, revision)
}

// evictAt hands the copy the delete of key at revision through a handle of epoch, as a store's
// delete does when it cannot read its own record back (kvwatch.Copy.Evict).
func (s *seen) evictAt(w *kvwatch.Watcher, epoch uint64, key string, revision uint64) kvwatch.Outcome {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.keys.Evict(w, epoch, key, revision)
}

// fenceOf is the revision the copy's fence holds for key in the watcher's current epoch.
func (s *seen) fenceOf(w *kvwatch.Watcher, key string) uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.keys.FenceFor(w.Epoch(), key)
}

// watchOnInBackground installs a read on kv through WatchOn, as a Rewatch does once it has opened
// its handle, from a goroutine of its own, since kv's watch may hold; it returns what waits for
// that call to return.
func watchOnInBackground(t *testing.T, w *kvwatch.Watcher, kv bus.KeyValue) func() {
	done := make(chan error, 1)
	go func() { done <- kvwatch.WatchOn(w, kv) }()
	return func() {
		t.Helper()
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("the watch: %v", err)
			}
		case <-time.After(10 * time.Second):
			t.Fatal("timed out waiting for the watch to return")
		}
	}
}

// heldStreamRead is a bucket's jetstream context whose first stream read, once answered, is held
// until the test lets it go: a watch that reads its bucket's stream through it holds with that
// answer in hand, before it makes its consumer. answered closes once it holds.
type heldStreamRead struct {
	jetstream.JetStream
	answered, release chan struct{}
	first, released   sync.Once
}

func (s *heldStreamRead) Stream(ctx context.Context, name string) (jetstream.Stream, error) {
	stream, err := s.JetStream.Stream(ctx, name)
	s.first.Do(func() {
		close(s.answered)
		<-s.release
	})
	return stream, err
}

func (s *heldStreamRead) let() { s.released.Do(func() { close(s.release) }) }

// createdAs is a bucket's jetstream context whose every stream read reports the stream's creation
// time as created, as a server that keeps it across a restore of a snapshot reports it: the rest of
// what the read reports, its sequences among it, is the server's.
type createdAs struct {
	jetstream.JetStream
	created time.Time
}

func (s createdAs) Stream(ctx context.Context, name string) (jetstream.Stream, error) {
	stream, err := s.JetStream.Stream(ctx, name)
	if err != nil {
		return nil, err
	}
	return createdStream{Stream: stream, created: s.created}, nil
}

// createdStream is a stream read whose cached information reports created as its creation time.
type createdStream struct {
	jetstream.Stream
	created time.Time
}

func (s createdStream) CachedInfo() *jetstream.StreamInfo {
	info := *s.Stream.CachedInfo()
	info.Created = s.created
	return &info
}

// failingBarriers is a bucket's jetstream context whose publishes, while failing is set, fail as a
// closed connection's do: a read's barrier (bus.KeyValue.WriteBarrier) is the one publish through
// it, so that read fails, while every stream read and watch goes on as usual.
type failingBarriers struct {
	jetstream.JetStream
	failing *atomic.Bool
}

func (s failingBarriers) PublishMsg(ctx context.Context, msg *natsgo.Msg, opts ...jetstream.PublishOpt) (*jetstream.PubAck, error) {
	if s.failing.Load() {
		return nil, natsgo.ErrConnectionClosed
	}
	return s.JetStream.PublishMsg(ctx, msg, opts...)
}

// purgeKey removes every record of key from the test bucket's stream and leaves no marker in
// their place, so no read of the bucket delivers key again.
func purgeKey(t *testing.T, conn *natsgo.Conn, key string) {
	t.Helper()
	subject := "$KV." + testBucketName + "." + key
	if err := jetStream(t, conn).PurgeStream("KV_"+testBucketName, &natsgo.StreamPurgeRequest{Subject: subject}); err != nil {
		t.Fatalf("purge %s: %v", subject, err)
	}
}

// lastRevision is the revision of key's latest record in the bucket kv is a handle on: a delete's
// own marker once key was deleted.
func lastRevision(t *testing.T, kv bus.KeyValue, key string) uint64 {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	history, err := kv.ReadHistory(ctx, key)
	if err != nil || len(history) == 0 {
		t.Fatalf("read %s's history: %d records, %v", key, len(history), err)
	}
	return history[len(history)-1].Revision()
}
