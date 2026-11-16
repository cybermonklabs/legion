package kvwatch_test

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/nats-io/nats.go/jetstream"
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/kvwatch"
	"github.com/sjawhar/envoy/internal/testnats"
)

// scheduledEvery is the scheduled read's interval these tests set (kvwatch.WithScheduledRead). No
// other wait the watcher arms has it as its delay -- a stream probe's 2 s, a re-read's 0 or 5 s, a
// scan idle wait's 10 s, a repair wait's minute -- so a test finds the scheduled waits among those
// a pending keeps by that delay alone.
const scheduledEvery = 45 * time.Second

// putKeys writes each of keys into the bucket kv is a handle on.
func putKeys(t *testing.T, kv bus.KeyValue, keys ...string) {
	t.Helper()
	for _, key := range keys {
		if _, err := kv.Put(key, []byte("1")); err != nil {
			t.Fatalf("put %s: %v", key, err)
		}
	}
}

// bucketKeys is every key the bucket kv is a handle on holds, sorted.
func bucketKeys(t *testing.T, kv bus.KeyValue) []string {
	t.Helper()
	keys, err := kv.Legacy().Keys()
	if err != nil {
		t.Fatalf("list the bucket's keys: %v", err)
	}
	slices.Sort(keys)
	return keys
}

// remove takes key out of the copy for a reason of the cache's own (kvwatch.Copy.Remove), so the
// copy lacks a key its bucket may still hold until a read delivers it again.
func (s *seen) remove(key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.keys.Remove(key)
}

// put hands the copy key, written at revision through a handle of the watcher's current epoch, as
// a store's write-through does once its write answered (kvwatch.Copy.Put), and returns what
// became of it.
func (s *seen) put(w *kvwatch.Watcher, key string, revision uint64) kvwatch.Outcome {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.keys.Put(w, w.Epoch(), key, true, revision)
}

// keyList is every key the copy holds, sorted.
func (s *seen) keyList() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var keys []string
	for key := range s.keys.All() {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	return keys
}

// scheduledReadsAre fails the test, naming when, unless the watcher has armed count scheduled
// reads in all and has stopped every one of them, but the last when lastArmed.
func scheduledReadsAre(t *testing.T, timers *pending, count int, lastArmed bool, when string) {
	t.Helper()
	waits := timers.armed(scheduledEvery)
	if len(waits) != count {
		t.Fatalf("%s: the watcher armed %d scheduled reads, want %d", when, len(waits), count)
	}
	for i, wait := range waits {
		stillArmed := !wait.stopped.Load()
		if want := lastArmed && i == len(waits)-1; stillArmed != want {
			t.Fatalf("%s: scheduled read %d of %d still armed %v, want %v", when, i+1, count, stillArmed, want)
		}
	}
}

// heldScan holds one read in the middle of its scan: its watcher delivers the bucket's first
// `after` records, then holds the rest of the scan, its nil among them, until the test lets them
// go. reached closes once it holds, those records handed to the watcher.
type heldScan struct {
	after            int
	reached, release chan struct{}
	once             sync.Once
}

func newHeldScan(t *testing.T, after int) *heldScan {
	h := &heldScan{after: after, reached: make(chan struct{}), release: make(chan struct{})}
	t.Cleanup(h.let)
	return h
}

// let releases the rest of the scan.
func (h *heldScan) let() { h.once.Do(func() { close(h.release) }) }

// step is the watch that holds its scan on h.
func (h *heldScan) step() watchStep {
	return func(ctx context.Context, kv jetstream.KeyValue) (jetstream.KeyWatcher, error) {
		watcher, err := kv.WatchAll(ctx)
		if err != nil {
			return nil, err
		}
		updates := make(chan jetstream.KeyValueEntry)
		go func() {
			defer close(updates)
			forwarded := 0
			for entry := range watcher.Updates() {
				if forwarded == h.after {
					close(h.reached)
					<-h.release
				}
				forwarded++
				updates <- entry
			}
		}()
		return &forwardedWatcher{KeyWatcher: watcher, updates: updates}, nil
	}
}

// heldCreate is a watch that creates its watcher on the bucket and holds it until the test lets it
// go: the watch it serves has read the bucket's stream and armed its watcher, and has not yet taken
// the watcher's locks to install it. created closes once it holds.
type heldCreate struct {
	created, release chan struct{}
	once             sync.Once
}

func newHeldCreate(t *testing.T) *heldCreate {
	h := &heldCreate{created: make(chan struct{}), release: make(chan struct{})}
	t.Cleanup(h.let)
	return h
}

// let hands the created watcher to the watch.
func (h *heldCreate) let() { h.once.Do(func() { close(h.release) }) }

// step is the watch that holds its create on h.
func (h *heldCreate) step() watchStep {
	return func(ctx context.Context, kv jetstream.KeyValue) (jetstream.KeyWatcher, error) {
		watcher, err := kv.WatchAll(ctx)
		close(h.created)
		<-h.release
		return watcher, err
	}
}

// A scheduled read is armed, with the option's interval, when a read completes and only then:
// never while a read is under way, whichever read it is (the start's, a Rewatch's, the scheduled
// read's own), so it never starts inside another read. A watch that installs, the watcher's own
// end, a disconnect and Stop each clear it. A cache that does not ask for it is armed none.
func TestARescanIsArmedAtEachCompletedReadAndNeverDuringOne(t *testing.T) {
	t.Run("the default arms none", func(t *testing.T) {
		uri := testnats.URL(t)
		_, kv := bucket(t, uri)
		putKeys(t, kv, "one")
		arm := &armed{}
		w := kvwatch.New("test cache", kv, newSeen(), kvwatch.WithAfterFunc(arm.afterFunc()), kvwatch.WithStreamProbeInterval(0))
		t.Cleanup(w.Stop)
		w.Start()
		eventually(t, "the first read's completion", w.Following)
		if got := arm.waits(); got != 0 {
			t.Fatalf("a watcher with no scheduled read armed %d waits besides its scan's idle wait, want none", got)
		}
	})

	t.Run("each completed read arms it", func(t *testing.T) {
		uri := testnats.URL(t)
		conn, kv := bucket(t, uri)
		putKeys(t, kv, "one", "two")
		into := newSeen()
		timers := &pending{}
		first := newHeldEnd(t)
		w := kvwatch.New("test cache", scripted(kv, first.step()).handle(), into, kvwatch.WithAfterFunc(timers.afterFunc()),
			kvwatch.WithStreamProbeInterval(0), kvwatch.WithScheduledRead(scheduledEvery))
		t.Cleanup(w.Stop)
		w.Start()

		wait(t, first.reached, "the first read to deliver every record")
		scheduledReadsAre(t, timers, 0, false, "while the first read is under way")
		first.let()
		eventually(t, "the first read's completion", w.Following)
		scheduledReadsAre(t, timers, 1, true, "after the first read completed")

		// A Rewatch's install clears the wait, and its read's completion arms the next. The handle
		// it moves to holds the end of the scheduled read's watch too.
		rewatched, scheduled := newHeldEnd(t), newHeldEnd(t)
		if err := kvwatch.WatchOn(w, scripted(kv, rewatched.step(), scheduled.step()).handle()); err != nil {
			t.Fatalf("the rewatch: %v", err)
		}
		wait(t, rewatched.reached, "the rewatch's read to deliver every record")
		scheduledReadsAre(t, timers, 1, false, "while the rewatch's read is under way")
		rewatched.let()
		eventually(t, "the rewatch's read's completion", w.Following)
		scheduledReadsAre(t, timers, 2, true, "after the rewatch's read completed")

		// The scheduled read itself: a re-read through the current handle, whose install clears
		// the wait that started it and whose completion arms the next.
		timers.last(t, scheduledEvery).fire()
		wait(t, scheduled.reached, "the scheduled read to deliver every record")
		scheduledReadsAre(t, timers, 2, false, "while the scheduled read is under way")
		scheduled.let()
		eventually(t, "the scheduled read's completion", w.Following)
		scheduledReadsAre(t, timers, 3, true, "after the scheduled read completed")
		if reads := into.reads(); len(reads) != 3 || reads[2].Recreated || reads[2].Epoch != 0 {
			t.Fatalf("reads begun %+v; want the start's, the rewatch's and the scheduled one, none of a recreated bucket", reads)
		}

		// A disconnect clears it; the read the reconnect's Rewatch makes arms the next.
		w.Disconnected()
		scheduledReadsAre(t, timers, 3, false, "after a disconnect")
		ending, _ := bucket(t, uri)
		if err := w.Rewatch(ending); err != nil {
			t.Fatalf("the rewatch after the disconnect: %v", err)
		}
		eventually(t, "the read after the disconnect to complete", w.Following)
		scheduledReadsAre(t, timers, 4, true, "after the read that followed the disconnect completed")

		// The watcher's own end clears it: the connection it reads through closes.
		ending.Close()
		eventually(t, "the watcher's own end", func() bool { return w.Err() != nil })
		scheduledReadsAre(t, timers, 4, false, "after the watcher ended on its own")
		if err := w.Rewatch(conn); err != nil {
			t.Fatalf("the rewatch after the watcher's end: %v", err)
		}
		eventually(t, "the read after the watcher's end to complete", w.Following)
		scheduledReadsAre(t, timers, 5, true, "after the read that repaired the watcher's end completed")

		w.Stop()
		scheduledReadsAre(t, timers, 5, false, "after Stop")
	})
}

// The scheduled read repairs a copy that drifted from its bucket while it followed it, in both
// directions: a key the copy lost is delivered again, and a key the copy kept that the bucket does
// not hold is swept, so once the read completes the copy holds what the bucket holds.
func TestARescanRepairsBothDirectionsOfTheCopy(t *testing.T) {
	uri := testnats.URL(t)
	_, kv := bucket(t, uri)
	putKeys(t, kv, "one", "two", "lost-by-the-copy")
	into := newSeen()
	timers := &pending{}
	w := kvwatch.New("test cache", kv, into, kvwatch.WithAfterFunc(timers.afterFunc()),
		kvwatch.WithStreamProbeInterval(0), kvwatch.WithScheduledRead(scheduledEvery))
	t.Cleanup(w.Stop)
	w.Start()
	eventually(t, "the first read's completion", w.Following)

	into.remove("lost-by-the-copy")
	into.hold("gone-from-the-bucket")
	if into.has("lost-by-the-copy") || !into.has("gone-from-the-bucket") || !w.Following() {
		t.Fatalf("the copy before the scheduled read: lost key %v, gone key %v, Following %v; want it drifted both ways and still following",
			into.has("lost-by-the-copy"), into.has("gone-from-the-bucket"), w.Following())
	}

	timers.last(t, scheduledEvery).fire()
	eventually(t, "the scheduled read's completion", w.Following)
	if got, want := into.keyList(), bucketKeys(t, kv); !slices.Equal(got, want) {
		t.Fatalf("after the scheduled read the copy holds %v, want the bucket's %v", got, want)
	}
	if reads, done := into.reads(), into.completions(); len(reads) != 2 || reads[1].Recreated || len(done) != 2 {
		t.Fatalf("reads begun %+v, completed %+v; want the start's and the scheduled one, both completed", reads, done)
	}
	serves(t, w, "after the scheduled read")
}

// A scheduled read whose setup fails (here its watcher's create is refused) installs nothing: the
// watcher it would have replaced goes on following its bucket and delivering its records, the copy
// stays served and needs no read, and the read is tried again after the re-read delay, as any
// re-read whose setup failed is. The failure is one WARN naming the scheduled read.
func TestARescanWhoseSetupFailsIsTriedAgainWhileTheOldWatcherFollows(t *testing.T) {
	const (
		rereadAfter = 5 * time.Second
		failed      = "test cache re-read of a scheduled read failed; it is tried again"
	)
	uri := testnats.URL(t)
	_, kv := bucket(t, uri)
	putKeys(t, kv, "one")
	into := newSeen()
	timers := &pending{}
	logger, records := jsonLogger(t)
	// The start's watch is the bucket's own; the scheduled read's is refused.
	handle := scripted(kv, nil, fails(errors.New("the watch was refused")))
	w := kvwatch.New("test cache", handle.handle(), into, kvwatch.WithLogger(logger), kvwatch.WithAfterFunc(timers.afterFunc()),
		kvwatch.WithStreamProbeInterval(0), kvwatch.WithScheduledRead(scheduledEvery))
	t.Cleanup(w.Stop)
	w.Start()
	eventually(t, "the first read's completion", w.Following)

	timers.last(t, scheduledEvery).fire()
	if got := handle.watches(); got != 2 {
		t.Fatalf("the handle was watched %d times, want the start's and the scheduled read's", got)
	}
	if !w.Following() || w.NeedsRead() || w.Err() != nil || len(into.reads()) != 1 {
		t.Fatalf("after the scheduled read's setup failed: Following %v, NeedsRead %v, Err %v, %d reads begun; want following, nothing needed, no error, only the start's read",
			w.Following(), w.NeedsRead(), w.Err(), len(into.reads()))
	}
	serves(t, w, "after the scheduled read's setup failed")
	if retries := len(timers.armed(rereadAfter)); retries != 1 {
		t.Fatalf("the failed setup armed %d re-reads of %v, want one", retries, rereadAfter)
	}
	if lines := matching(records(), "WARN", failed); len(lines) != 1 || lines[0]["bucket"] != testBucketName {
		t.Fatalf("the failed setup was logged %v, want one WARN naming bucket %s", lines, testBucketName)
	}

	putKeys(t, kv, "after-the-failed-setup")
	eventually(t, "the old watcher's delivery of a record written after the failed setup", func() bool {
		return into.has("after-the-failed-setup")
	})

	timers.last(t, rereadAfter).fire()
	eventually(t, "the retried read's completion", w.Following)
	if done := len(into.completions()); done != 2 {
		t.Fatalf("%d reads completed, want the start's and the retried one", done)
	}
	scheduledReadsAre(t, timers, 2, true, "after the retried read completed")
}

// An armed scheduled read is not a re-read already due: a probe that finds the bucket recreated
// while one is armed starts its own re-read at once, as it does with none armed, rather than leave
// the new bucket unread until the scheduled interval passes. That re-read's install clears the
// scheduled wait, and its completion arms the next.
func TestARescanLeavesARecreatedBucketsReadToTheProbe(t *testing.T) {
	uri := testnats.URL(t)
	conn, kv := bucket(t, uri)
	putKeys(t, kv, "ghost")
	into := newSeen()
	timers := &pending{}
	w := kvwatch.New("test cache", kv, into, kvwatch.WithAfterFunc(timers.afterFunc()), kvwatch.WithScheduledRead(scheduledEvery))
	t.Cleanup(w.Stop)
	w.Start()
	eventually(t, "the first read's completion", w.Following)
	scheduledReadsAre(t, timers, 1, true, "after the first read completed")

	recreate(t, conn, "fresh")
	timers.last(t, probeEvery).fire()
	if rereads := len(timers.armed(rereadNow)); rereads != 1 || w.Following() || w.NeedsRead() {
		t.Fatalf("after the probe found the bucket recreated: %d re-reads of %v armed, Following %v, NeedsRead %v; want one, not following, a re-read due",
			rereads, rereadNow, w.Following(), w.NeedsRead())
	}

	timers.last(t, rereadNow).fire()
	readWholeInEpochOne(t, w, into, "fresh", "ghost")
	scheduledReadsAre(t, timers, 2, true, "after the recreated bucket's read completed")
}

// A key written into the copy while the scheduled read is under way, after its scan passed that
// key, survives the read's completion: the read repairs the copy through the copy's read protocol
// (BeginRead, Delivered, CompleteRead), so the write marks the key for that read and the sweep keeps
// it, while a key the copy held from before the read, which the bucket does not hold, is swept. A
// read that set the copy to what its scan saw would roll the write back.
func TestARepairMidScanKeepsAKeyWrittenAfterTheScanPassedIt(t *testing.T) {
	uri := testnats.URL(t)
	_, kv := bucket(t, uri)
	putKeys(t, kv, "one", "two")
	into := newSeen()
	timers := &pending{}
	held := newHeldEnd(t)
	// The start's watch is the bucket's own; the scheduled read's holds its end.
	w := kvwatch.New("test cache", scripted(kv, nil, held.step()).handle(), into, kvwatch.WithAfterFunc(timers.afterFunc()),
		kvwatch.WithStreamProbeInterval(0), kvwatch.WithScheduledRead(scheduledEvery))
	t.Cleanup(w.Stop)
	w.Start()
	eventually(t, "the first read's completion", w.Following)
	into.hold("gone-from-the-bucket")

	timers.last(t, scheduledEvery).fire()
	wait(t, held.reached, "the scheduled read to deliver every record")
	// The write reaches the copy alone, as a write-through's result does: the bucket is not
	// written, so no record the watcher delivers after the read can put the key back, and only the
	// read's marks can keep it through the sweep.
	if outcome := into.put(w, "written-mid-scan", 1); outcome != kvwatch.Applied {
		t.Fatalf("the write into the copy came to outcome %d, want Applied (%d)", outcome, kvwatch.Applied)
	}
	held.let()
	wait(t, held.handled, "the scheduled read's end to be handled")
	if !w.Following() || len(into.completions()) != 2 {
		t.Fatalf("after the scheduled read's end: Following %v, %d reads completed; want following, the start's and the scheduled read completed",
			w.Following(), len(into.completions()))
	}
	if !into.has("written-mid-scan") || into.has("gone-from-the-bucket") || !into.has("one") || !into.has("two") {
		t.Fatalf("after the scheduled read: written key %v, gone key %v, bucket keys %v and %v; want the written key and the bucket's kept, the gone key swept",
			into.has("written-mid-scan"), into.has("gone-from-the-bucket"), into.has("one"), into.has("two"))
	}
}

// One reader fills the copy at a time: a scheduled read that comes due while a Rewatch's read is
// in the middle of its scan never begins a read of its own beside it, so it cannot sweep the keys
// that read has not yet delivered. Whether its wait fires once the Rewatch has installed (the
// install stopped it, and a time.AfterFunc callback already started still runs) or its own watch is
// already in flight when the Rewatch installs, it is discarded, and once the Rewatch's read
// completes the copy holds every key the bucket holds.
func TestARepairOneReaderLeavesEveryKeyWhenARewatchRacesTheScheduledRead(t *testing.T) {
	const keys, deliveredBeforeTheHold = 20, 5
	// start starts a watcher on a bucket of keys keys, whose first watch is the bucket's own and
	// whose later watches go as steps say, and waits for its first read to complete.
	start := func(t *testing.T, steps ...watchStep) (*kvwatch.Watcher, *seen, *pending, bus.KeyValue) {
		t.Helper()
		uri := testnats.URL(t)
		_, kv := bucket(t, uri)
		for i := range keys {
			putKeys(t, kv, fmt.Sprintf("key-%02d", i))
		}
		into := newSeen()
		timers := &pending{}
		w := kvwatch.New("test cache", scripted(kv, append([]watchStep{nil}, steps...)...).handle(), into,
			kvwatch.WithAfterFunc(timers.afterFunc()), kvwatch.WithStreamProbeInterval(0), kvwatch.WithScheduledRead(scheduledEvery))
		t.Cleanup(w.Stop)
		w.Start()
		eventually(t, "the first read's completion", w.Following)
		return w, into, timers, kv
	}
	// rewatchHeldMidScan installs a Rewatch's read whose scan holds after deliveredBeforeTheHold
	// records, and returns it with the handle it reads through.
	rewatchHeldMidScan := func(t *testing.T, w *kvwatch.Watcher, kv bus.KeyValue) (*heldScan, *scriptedKV) {
		t.Helper()
		mid := newHeldScan(t, deliveredBeforeTheHold)
		handle := scripted(kv, mid.step())
		if err := kvwatch.WatchOn(w, handle.handle()); err != nil {
			t.Fatalf("the rewatch: %v", err)
		}
		wait(t, mid.reached, "the rewatch's read to hold mid-scan")
		return mid, handle
	}
	// underWay fails the test unless the Rewatch's read is still under way: only the start's read
	// has completed and the copy does not follow its bucket.
	underWay := func(t *testing.T, w *kvwatch.Watcher, into *seen, when string) {
		t.Helper()
		if done := len(into.completions()); done != 1 || w.Following() {
			t.Fatalf("%s: %d reads completed, Following %v; want the rewatch's read under way", when, done, w.Following())
		}
	}
	// oneReader lets the Rewatch's scan go and fails the test unless the start's read and the
	// Rewatch's are the only reads that began, and the copy holds what the bucket holds once the
	// Rewatch's read completes. It records both key counts.
	oneReader := func(t *testing.T, w *kvwatch.Watcher, into *seen, kv bus.KeyValue, mid *heldScan) {
		t.Helper()
		mid.let()
		eventually(t, "the rewatch's read's completion", w.Following)
		if reads := len(into.reads()); reads != 2 {
			t.Fatalf("%d reads began, want the start's and the rewatch's only", reads)
		}
		inBucket, inCopy := bucketKeys(t, kv), into.keyList()
		t.Logf("the bucket holds %d keys and the copy %d", len(inBucket), len(inCopy))
		if !slices.Equal(inCopy, inBucket) {
			t.Fatalf("the copy holds %v, want the bucket's %v", inCopy, inBucket)
		}
	}

	t.Run("its wait fires while the rewatch's read is under way", func(t *testing.T) {
		w, into, timers, kv := start(t)
		scheduled := timers.last(t, scheduledEvery)
		mid, handle := rewatchHeldMidScan(t, w, kv)
		if !scheduled.stopped.Load() {
			t.Fatal("the rewatch's install left the scheduled read armed")
		}

		scheduled.fire()
		underWay(t, w, into, "after the scheduled wait fired")
		if got := handle.watches(); got != 1 {
			t.Fatalf("the current handle was watched %d times, want the rewatch's only", got)
		}
		oneReader(t, w, into, kv, mid)
	})

	t.Run("its watch is in flight when the rewatch installs", func(t *testing.T) {
		inflight := newHeldCreate(t)
		w, into, timers, kv := start(t, inflight.step())
		scheduled := timers.last(t, scheduledEvery)
		returned := make(chan struct{})
		go func() {
			defer close(returned)
			scheduled.fire()
		}()
		wait(t, inflight.created, "the scheduled read's watch to create its watcher")

		mid, _ := rewatchHeldMidScan(t, w, kv)
		inflight.let()
		wait(t, returned, "the scheduled read's watch to return")
		underWay(t, w, into, "after the scheduled read's watch returned")
		oneReader(t, w, into, kv, mid)
	})
}
