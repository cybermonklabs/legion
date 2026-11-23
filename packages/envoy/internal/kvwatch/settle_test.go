package kvwatch_test

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/kvwatch"
	"github.com/sjawhar/envoy/internal/testnats"
	"github.com/sjawhar/envoy/internal/testwait"
)

// A store captures its handle and epoch before it writes (Watcher.Handle; the session registry's
// PutWithin among them). A write it began before a bucket was deleted and created again can land in
// the new bucket after the new read's consumer was made, and its result reach the copy after the
// install moved the epoch and before the read began: the copy refuses that result for its epoch.
// The read still keeps the key, because its consumer delivers the record before the read
// completes.
func TestARecreatedReadKeepsAnOldHandlesResultTakenAfterTheEpochMoved(t *testing.T) {
	const key = "written-through-the-old-handle"
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
	outcomes := make(chan kvwatch.Outcome, 1)
	cache.beforeTarget(func(kvwatch.Read) {
		if epoch := w.Epoch(); epoch == oldEpoch {
			t.Errorf("before the recreated read began the epoch is still %d: the install did not move it", epoch)
		}
		revision, err := oldKV.Put(key, []byte("1"))
		if err != nil {
			t.Errorf("put %s through the old handle: %v", key, err)
			outcomes <- kvwatch.Applied
			return
		}
		outcomes <- cache.putAt(w, oldEpoch, key, revision)
	})
	if err := kvwatch.WatchOn(w, wrap(t, conn, recreated)); err != nil {
		t.Fatalf("the recreated bucket's watch: %v", err)
	}
	if outcome := <-outcomes; outcome != kvwatch.Dropped {
		t.Fatalf("the old handle's result came to outcome %d after the epoch moved, want Dropped (%d)", outcome, kvwatch.Dropped)
	}
	eventually(t, "the recreated bucket's read to complete", cache.targetDone)
	if got, want := bucketKeys(t, kv), []string{key}; !slices.Equal(got, want) {
		t.Fatalf("the new bucket holds %v, want %v", got, want)
	}
	if !cache.has(key) {
		t.Fatalf("the recreated bucket's read left %s out of the copy, which the new bucket holds: the copy holds %v", key, cache.keyList())
	}
}

// A key the first read delivered can be gone from the bucket by the time a later read completes: a
// write updated it in the window, through the copy, and then a delete removed it from the bucket
// alone, while nothing else delivered that delete (the connection the replaced watcher read
// through is gone, as a reconnect's read finds it). The read's consumer delivers the delete before
// the read completes, so the row is gone.
func TestAReadRemovesAScannedKeyTheBucketDeletedInItsWindow(t *testing.T) {
	const scannedThenDeleted = "scanned-then-deleted"
	uri := testnats.URL(t)
	lost, kv := bucket(t, uri)
	putKeys(t, kv, scannedThenDeleted)
	cache := newWindowCache(2)
	timers := &pending{}
	w := kvwatch.New("test cache", kv, cache, kvwatch.WithAfterFunc(timers.afterFunc()), kvwatch.WithStreamProbeInterval(0))
	t.Cleanup(w.Stop)
	t.Cleanup(cache.letGo)
	w.Start()
	eventually(t, "the first read's completion", func() bool { return len(cache.completions()) == 1 })
	if !cache.has(scannedThenDeleted) {
		t.Fatalf("the first read did not deliver %s", scannedThenDeleted)
	}

	lost.Close()
	eventually(t, "the first watcher's end", func() bool { return w.Err() != nil })
	_, live := bucket(t, uri)
	held := newHeldCreate(t)
	returned := watchOnInBackground(t, w, scripted(live, held.step()).handle())
	wait(t, held.created, "the reconnect's watch to make its consumer")
	if outcome := cache.putThrough(t, w, live, w.Epoch(), scannedThenDeleted); outcome != kvwatch.Applied {
		t.Fatalf("the window write of %s came to outcome %d, want Applied (%d)", scannedThenDeleted, outcome, kvwatch.Applied)
	}
	if err := live.Delete(scannedThenDeleted); err != nil {
		t.Fatalf("delete %s from the bucket: %v", scannedThenDeleted, err)
	}
	held.let()
	returned()
	eventually(t, "the reconnect's read to complete", cache.targetDone)
	if cache.has(scannedThenDeleted) {
		t.Fatalf("the completed read kept %s, which the bucket deleted in its window: the copy holds %v", scannedThenDeleted, cache.keyList())
	}
}

// A key the read has evidence of only through a write the bucket then replaces ahead of the read's
// own consumer: window writes A@1 and K@2 reach the copy before the read begins, the read's barrier
// is B@3, and while the read waits past it another connection writes A@4 and K@5, replacing A@1
// and K@2 before the consumer has reached them. The read keeps A and K, which the bucket held
// throughout.
//
// The order is the real server's, on the consumer under test: that consumer is paused from its
// creation until the replacements are stored (pausedWatch), so its cursor is still behind A@1 when
// the bucket removes it, and the counts its deliveries carry are the server's own.
// TestAReadKeepsRowsAFalseZeroPendingCountWouldSweep is the same order with the barrier's pending
// count the server can misreport.
func TestAReadKeepsAKeyTheBucketReplacedAheadOfItsConsumer(t *testing.T) {
	uri := testnats.URL(t)
	_, kv := bucket(t, uri)
	_, other := bucket(t, uri)
	paused := newPausedWatch(t, kv.Streams(), false)
	barrier := &replacingBarrier{JetStream: kv.Streams(), other: other, replace: []string{"a", "k"}, written: map[string]uint64{}}
	cache := newWindowCache(1)
	timers := &pending{}
	w := kvwatch.New("test cache", bus.NewKeyValue(kv.Legacy(), scripted(kv, paused.step()), barrier), cache,
		kvwatch.WithAfterFunc(timers.afterFunc()), kvwatch.WithStreamProbeInterval(0), kvwatch.WithScanIdleTimeout(7*time.Hour))
	t.Cleanup(w.Stop)
	t.Cleanup(cache.letGo)
	w.Start()

	wait(t, paused.held.created, "the read's watch to make and pause its consumer")
	for _, key := range []string{"a", "k"} {
		if outcome := cache.writeThrough(t, w, key); outcome != kvwatch.Applied {
			t.Fatalf("the window write of %s came to outcome %d, want Applied (%d)", key, outcome, kvwatch.Applied)
		}
	}
	paused.held.let()
	eventually(t, "the read to wait past its barrier", func() bool { return len(timers.armed(7*time.Hour)) == 2 })
	if err := barrier.failure(); err != nil {
		t.Fatalf("the writes after the barrier: %v", err)
	}
	if done := len(cache.completions()); done != 0 {
		t.Fatalf("%d reads completed while the read's consumer was paused, want none", done)
	}
	paused.resume(t)
	eventually(t, "the read's completion", cache.targetDone)
	if got, want := bucketKeys(t, kv), []string{"a", "k"}; !slices.Equal(got, want) {
		t.Fatalf("the bucket holds %v, want %v", got, want)
	}
	if !cache.has("k") || !cache.has("a") {
		t.Fatalf("the completed read left the copy holding %v, want a and k: the bucket replaced them, never removed them", cache.keyList())
	}
}

// A write another writer makes in the read's window - another listener's, which this copy holds
// no trace of - is in no list of keys the copy knows, and not in the read's scan. The read keeps
// it anyway: its consumer delivers the write before the read completes. So does a key longer than
// a filtered watch could name (testnats.LegacyKeys' readable one), which the read's watch of every
// key delivers like any other.
func TestAReadKeepsWritesAnotherWriterMadeInItsWindow(t *testing.T) {
	const written = "written-by-another-listener"
	uri := testnats.URL(t)
	_, kv := bucket(t, uri)
	_, other := bucket(t, uri)
	legacy, _ := testnats.LegacyKeys(testBucketName, "k")
	held := newHeldCreate(t)
	cache := newWindowCache(1)
	timers := &pending{}
	w := kvwatch.New("test cache", scripted(kv, held.step()).handle(), cache, kvwatch.WithAfterFunc(timers.afterFunc()), kvwatch.WithStreamProbeInterval(0))
	t.Cleanup(w.Stop)
	t.Cleanup(cache.letGo)
	w.Start()

	wait(t, held.created, "the first read's watch to make its consumer")
	if _, err := other.Put(written, []byte("1")); err != nil {
		t.Fatalf("put %s on the other connection: %v", written, err)
	}
	if _, err := other.Legacy().Put(legacy, []byte("1")); err != nil {
		t.Fatalf("put the legacy-length key on the other connection: %v", err)
	}
	held.let()
	eventually(t, "the read's completion", cache.targetDone)
	if !cache.has(written) || !cache.has(legacy) {
		t.Fatalf("the completed read left the copy holding %d keys %v, want %s and the %d-byte key another writer wrote in its window",
			len(cache.keyList()), shortKeys(cache.keyList()), written, len(legacy))
	}
}

// The pending count a delivery carries is the server's cached count, and it can read 1 when nothing
// is left to deliver: a removal the consumer's cursor had already skipped is not counted
// (nats-server v2.15.1 consumer.go:5688-5689, 7020-7022). The window tests caught it under load,
// on the real server: a reconnect's consumer delivered its window writes, then its barrier with a
// pending count of 1, and nothing more, so a read that waited for a count of 0 waited out its
// timeout and failed. The read stops waiting at its consumer's first delivery at or past the
// barrier, whatever that delivery's count, and completes. The barrier is written to the bucket,
// so the list of its keys the read takes before it completes is answered through the barrier.
func TestAReadStopsWaitingAtTheBarrierWhateverItsPendingCount(t *testing.T) {
	uri := testnats.URL(t)
	_, kv := bucket(t, uri)
	armed := make(chan struct{})
	updates := make(chan jetstream.KeyValueEntry, 2)
	updates <- nil
	go func() {
		// Once the read waits past its barrier (its bound is armed), the consumer delivers the
		// barrier, the bucket's last record, with a pending count of 1, and then nothing more.
		<-armed
		state, err := kv.ReadStreamState(context.Background())
		if err != nil {
			t.Errorf("read the bucket's stream for its barrier: %v", err)
			return
		}
		updates <- scriptedRecord{key: bus.BarrierKey, revision: state.LastSeq, delta: 1, op: jetstream.KeyValueDelete}
	}()
	main := scripted(kv, func(context.Context, jetstream.KeyValue) (jetstream.KeyWatcher, error) {
		return &scriptedWatcher{updates: updates}, nil
	})
	into := newSeen()
	into.hold("held-by-the-copy")
	timers := &pending{}
	var once sync.Once
	afterFunc := timers.afterFunc()
	w := kvwatch.New("test cache", bus.NewKeyValue(kv.Legacy(), main, kv.Streams()), into,
		kvwatch.WithStreamProbeInterval(0), kvwatch.WithScanIdleTimeout(7*time.Hour),
		kvwatch.WithAfterFunc(func(d time.Duration, f func()) kvwatch.Timer {
			timer := afterFunc(d, f)
			if d == 7*time.Hour && len(timers.armed(d)) == 2 {
				once.Do(func() { close(armed) })
			}
			return timer
		}))
	t.Cleanup(w.Stop)
	w.Start()
	eventually(t, "the read's completion", func() bool { return len(into.completions()) == 1 })
	if into.has("held-by-the-copy") {
		t.Fatalf("the read completed with the copy holding %v, want the row the leader holds no record of removed", into.keyList())
	}
}

// A read's wait past its barrier is bounded: a consumer that never reaches the barrier fails the
// read the idle timeout after the stream check that follows the barrier's acknowledgement, which
// removes nothing and reads the bucket again. ReadErr measures from when the wait began, so it
// reports a wait that has outlasted the stale bound while it is still under way, and the failed
// run after it.
func TestAReadsWaitPastItsBarrierIsBoundedAndReported(t *testing.T) {
	const heldByTheCopy = "held-by-the-copy"
	uri := testnats.URL(t)
	_, kv := bucket(t, uri)
	clock := newClock()
	timers := &pending{}
	into := newSeen()
	into.hold(heldByTheCopy)
	w := kvwatch.New("test cache", bus.NewKeyValue(kv.Legacy(), kv.WatchHandle(), ackedAt{JetStream: kv.Streams(), sequence: 1 << 40}), into,
		kvwatch.WithNow(clock.Now), kvwatch.WithAfterFunc(timers.afterFunc()), kvwatch.WithStreamProbeInterval(0), kvwatch.WithScanIdleTimeout(7*time.Hour))
	t.Cleanup(w.Stop)
	w.Start()
	eventually(t, "the wait past the barrier to be bounded", func() bool { return len(timers.armed(7*time.Hour)) == 2 })

	if err := w.ReadErr(); err != nil {
		t.Fatalf("ReadErr as the wait begins = %v, want nil", err)
	}
	clock.advance(2*time.Minute + time.Second)
	if err := w.ReadErr(); !errors.Is(err, kvwatch.ErrUnverified) || !strings.Contains(err.Error(), "still waiting") {
		t.Fatalf("ReadErr once a wait still under way has outlasted the 2m stale bound = %v, want %v naming the wait", err, kvwatch.ErrUnverified)
	}
	// The wait's own deadline has gone once it fires: a signal before it ends nothing
	// (TestARetiredScanWaitDoesNotEndTheWaitPastTheBarrier).
	clock.advance(7 * time.Hour)
	timers.last(t, 7*time.Hour).fire()
	eventually(t, "the failed wait's re-read to be armed", func() bool { return len(timers.armed(5*time.Second)) == 1 })
	if err := w.ReadErr(); !errors.Is(err, kvwatch.ErrUnverified) || !strings.Contains(err.Error(), "over 1 reads") {
		t.Fatalf("ReadErr after the wait failed = %v, want %v over the one failed read", err, kvwatch.ErrUnverified)
	}
	if done := len(into.completions()); done != 0 || w.Following() || !into.has(heldByTheCopy) {
		t.Fatalf("after the bounded wait failed: %d reads completed, Following %v, copy %v; want none completed, not following, %s kept",
			done, w.Following(), into.keyList(), heldByTheCopy)
	}
}

// A read's wait past its barrier ends with its generation, and ReadErr then measures nothing
// from it. The read here waits on a barrier its consumer never reaches. Then a new watch replaces
// it, whose scan has not ended, or its watcher ends on its own when the bucket is deleted, or the
// watcher stops. Past the stale bound, ReadErr is nil: no read is waiting past a barrier, and none
// has failed. Before, the replaced wait's start stayed, and ReadErr answered "the read is still
// waiting", which /healthz reports as a 503.
func TestAWaitItsGenerationEndedIsNoWaitToReadErr(t *testing.T) {
	for _, row := range []struct {
		name string
		end  func(t *testing.T, w *kvwatch.Watcher, conn *natsgo.Conn, kv bus.KeyValue)
	}{
		{name: "a new watch replaces the read", end: func(t *testing.T, w *kvwatch.Watcher, _ *natsgo.Conn, kv bus.KeyValue) {
			if err := kvwatch.WatchOn(w, scripted(kv, stallsAfter(0)).handle()); err != nil {
				t.Fatalf("install the new watch: %v", err)
			}
		}},
		{name: "its watcher ends on its own", end: func(t *testing.T, w *kvwatch.Watcher, conn *natsgo.Conn, _ bus.KeyValue) {
			deleteTestBucket(t, jetStream(t, conn))
			// nats.go's ordered consumer notices a deleted stream only after about 10 s
			// (stopNoticeBudget).
			testwait.WithinPolling(t, stopNoticeBudget, 50*time.Millisecond, "the watcher to end on its own", func() bool { return w.Err() != nil })
		}},
		{name: "the watcher stops", end: func(_ *testing.T, w *kvwatch.Watcher, _ *natsgo.Conn, _ bus.KeyValue) {
			w.Stop()
		}},
	} {
		t.Run(row.name, func(t *testing.T) {
			uri := testnats.URL(t)
			conn, kv := bucket(t, uri)
			clock := newClock()
			timers := &pending{}
			w := kvwatch.New("test cache", bus.NewKeyValue(kv.Legacy(), kv.WatchHandle(), ackedAt{JetStream: kv.Streams(), sequence: 1 << 40}), newSeen(),
				kvwatch.WithNow(clock.Now), kvwatch.WithAfterFunc(timers.afterFunc()), kvwatch.WithStreamProbeInterval(0), kvwatch.WithScanIdleTimeout(7*time.Hour))
			t.Cleanup(w.Stop)
			w.Start()
			eventually(t, "the read to wait past its barrier", func() bool { return len(timers.armed(7*time.Hour)) == 2 })

			row.end(t, w, conn, kv)
			clock.advance(2*time.Minute + time.Second)
			if err := w.ReadErr(); err != nil {
				t.Fatalf("ReadErr past the stale bound, with no read waiting past a barrier = %v, want nil", err)
			}
		})
	}
}

// A run of reads whose barrier is never written is read again less and less often: 5, 10, 20 and
// 40 s apart, then once a minute. Nothing is removed and no read completes meanwhile.
func TestReadsThatCannotWriteTheirBarrierAreReadAgainLessOften(t *testing.T) {
	const heldByTheCopy = "held-by-the-copy"
	w, into, timers, _ := failingBarrierWatcher(t, heldByTheCopy, nil)
	w.Start()

	fired := map[*pendingWait]bool{}
	for i, want := range []time.Duration{5 * time.Second, 10 * time.Second, 20 * time.Second, 40 * time.Second, time.Minute, time.Minute} {
		var next *pendingWait
		eventually(t, fmt.Sprintf("re-read %d, after %v", i+1, want), func() bool {
			for _, wait := range timers.armed(want) {
				if !wait.stopped.Load() && !fired[wait] {
					next = wait
					return true
				}
			}
			return false
		})
		fired[next] = true
		next.fire()
	}
	if done := len(into.completions()); done != 0 || !into.has(heldByTheCopy) {
		t.Fatalf("after six failed reads: %d completed, the copy holds %v; want none completed and %s kept", done, into.keyList(), heldByTheCopy)
	}
}

// ReadErr says nothing while a run of reads whose barrier is never written is shorter than the
// copy's stale bound, reports the run once it has lasted the bound, naming the reads' own error,
// and says nothing again once a read completes.
func TestReadErrReportsAReadFailureThatOutlastsTheStaleBound(t *testing.T) {
	const heldByTheCopy = "held-by-the-copy"
	clock := newClock()
	w, into, timers, failing := failingBarrierWatcher(t, heldByTheCopy, clock)
	w.Start()
	eventually(t, "the first re-read to be armed", func() bool { return len(timers.armed(5*time.Second)) == 1 })

	if err := w.ReadErr(); err != nil {
		t.Fatalf("ReadErr right after the first failed read = %v, want nil", err)
	}
	clock.advance(2*time.Minute - time.Second)
	if err := w.ReadErr(); err != nil {
		t.Fatalf("ReadErr a second before the 2m stale bound = %v, want nil", err)
	}
	clock.advance(2 * time.Second)
	err := w.ReadErr()
	if !errors.Is(err, kvwatch.ErrUnverified) || !errors.Is(err, natsgo.ErrConnectionClosed) {
		t.Fatalf("ReadErr past the stale bound = %v, want %v wrapping the reads' own %v", err, kvwatch.ErrUnverified, natsgo.ErrConnectionClosed)
	}

	failing.Store(false)
	timers.last(t, 5*time.Second).fire()
	eventually(t, "the read after the barrier is written to complete", func() bool { return len(into.completions()) == 1 })
	if err := w.ReadErr(); err != nil || into.has(heldByTheCopy) {
		t.Fatalf("after a read completed: ReadErr %v, the copy holds %v; want nil and %s removed", err, into.keyList(), heldByTheCopy)
	}
}

// failingBarrierWatcher is a watcher, not yet started, on a bucket whose read barriers
// (bus.KeyValue.WriteBarrier) fail while the returned flag is set (it is at first), over a copy
// that holds held, a key the bucket does not. clock is the watcher's clock when not nil. Its waits
// are the returned pending's, its scans' idle wait a long one no other wait has, and its stream
// probe off.
func failingBarrierWatcher(t *testing.T, held string, clock *clock) (*kvwatch.Watcher, *seen, *pending, *atomic.Bool) {
	t.Helper()
	uri := testnats.URL(t)
	_, kv := bucket(t, uri)
	failing := &atomic.Bool{}
	failing.Store(true)
	handle := bus.NewKeyValue(kv.Legacy(), kv.WatchHandle(), failingBarriers{JetStream: kv.Streams(), failing: failing})
	into := newSeen()
	into.hold(held)
	timers := &pending{}
	options := []kvwatch.Option{kvwatch.WithAfterFunc(timers.afterFunc()), kvwatch.WithStreamProbeInterval(0), kvwatch.WithScanIdleTimeout(7 * time.Hour)}
	if clock != nil {
		options = append(options, kvwatch.WithNow(clock.Now))
	}
	w := kvwatch.New("test cache", handle, into, options...)
	t.Cleanup(w.Stop)
	return w, into, timers, failing
}

// replacingBarrier is a bucket's jetstream context that, once it has published the first read
// barrier, writes each of replace through other before the barrier's acknowledgement returns: the
// writes another writer makes while the read waits past its barrier. written is each one's
// revision.
type replacingBarrier struct {
	jetstream.JetStream
	other   bus.KeyValue
	replace []string

	mu      sync.Mutex
	barrier uint64
	written map[string]uint64
	err     error
}

func (s *replacingBarrier) PublishMsg(ctx context.Context, msg *natsgo.Msg, opts ...jetstream.PublishOpt) (*jetstream.PubAck, error) {
	ack, err := s.JetStream.PublishMsg(ctx, msg, opts...)
	if err != nil || !strings.HasSuffix(msg.Subject, "."+bus.BarrierKey) {
		return ack, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.barrier != 0 {
		return ack, nil
	}
	s.barrier = ack.Sequence
	for _, key := range s.replace {
		revision, err := s.other.Put(key, []byte("2"))
		if err != nil {
			s.err = fmt.Errorf("put %s on the other connection: %w", key, err)
			return ack, nil
		}
		s.written[key] = revision
	}
	return ack, nil
}

func (s *replacingBarrier) failure() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.err
}

// scriptedRecord is a record a scripted watch delivers, with the pending count it is delivered
// with.
type scriptedRecord struct {
	key      string
	revision uint64
	delta    uint64
	op       jetstream.KeyValueOp
}

func (r scriptedRecord) Bucket() string                  { return testBucketName }
func (r scriptedRecord) Key() string                     { return r.key }
func (r scriptedRecord) Value() []byte                   { return []byte("1") }
func (r scriptedRecord) Revision() uint64                { return r.revision }
func (r scriptedRecord) Created() time.Time              { return time.Now() }
func (r scriptedRecord) Delta() uint64                   { return r.delta }
func (r scriptedRecord) Operation() jetstream.KeyValueOp { return r.op }

// scriptedWatcher is a watcher whose updates are a channel the test fills; Stop closes it once.
type scriptedWatcher struct {
	updates chan jetstream.KeyValueEntry
	stopped sync.Once
}

func (w *scriptedWatcher) Updates() <-chan jetstream.KeyValueEntry { return w.updates }

func (w *scriptedWatcher) Stop() error {
	w.stopped.Do(func() { close(w.updates) })
	return nil
}

// ackedAt is a jetstream context whose publishes are acknowledged at sequence without being sent.
type ackedAt struct {
	jetstream.JetStream
	sequence uint64
}

func (s ackedAt) PublishMsg(context.Context, *natsgo.Msg, ...jetstream.PublishOpt) (*jetstream.PubAck, error) {
	return &jetstream.PubAck{Sequence: s.sequence}, nil
}

// shortKeys is keys with any key past 40 bytes shortened to its length, for a failure message.
func shortKeys(keys []string) []string {
	short := make([]string, len(keys))
	for i, key := range keys {
		if len(key) > 40 {
			key = fmt.Sprintf("<%d-byte key>", len(key))
		}
		short[i] = key
	}
	return short
}
