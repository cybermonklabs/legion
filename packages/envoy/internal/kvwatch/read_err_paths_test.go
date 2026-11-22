package kvwatch_test

import (
	"context"
	"errors"
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
)

// The tests of this file pin each path into ReadErr's count, and each way out of it, that no other
// test pins; Watcher.ReadErr's doc names every path and the test that pins it. The others are a
// barrier that is not written and a run that a completed read ends
// (TestReadErrReportsAReadFailureThatOutlastsTheStaleBound), a wait that times out on a stream
// check that answers and a wait still under way (TestAReadsWaitPastItsBarrierIsBoundedAndReported),
// and a wait its generation ends (TestAWaitItsGenerationEndedIsNoWaitToReadErr).

// pastTheBound is how far a test moves the watcher's clock before it reads ReadErr: past the
// 2-minute stale bound.
const pastTheBound = 2*time.Minute + time.Second

// unreadStreams is a bucket's jetstream context whose reads of a stream (STREAM.INFO, the stream
// check's) fail while unread is set, counting those it failed. With unreadOnPublish set, a publish
// it sent sets unread before it returns, so every stream check after a read's barrier fails and
// every one before it answers.
type unreadStreams struct {
	jetstream.JetStream
	unreadOnPublish bool
	unread          *atomic.Bool
	failed          *atomic.Int64
}

func (s unreadStreams) PublishMsg(ctx context.Context, msg *natsgo.Msg, opts ...jetstream.PublishOpt) (*jetstream.PubAck, error) {
	ack, err := s.JetStream.PublishMsg(ctx, msg, opts...)
	if s.unreadOnPublish {
		s.unread.Store(true)
	}
	return ack, err
}

func (s unreadStreams) Stream(ctx context.Context, name string) (jetstream.Stream, error) {
	if s.unread.Load() {
		s.failed.Add(1)
		return nil, errors.New("test: STREAM.INFO did not answer")
	}
	return s.JetStream.Stream(ctx, name)
}

// closedConn is a bucket's jetstream context whose connection, the one the key list
// (bus.KeyValue.StoredKeys) and the leader's reads (bus.KeyValue.HeldFromLeader) send on, is
// closed, while its barrier writes, stream reads and watches go through the open one it embeds.
type closedConn struct {
	jetstream.JetStream
	closed *natsgo.Conn
}

func (s closedConn) Conn() *natsgo.Conn { return s.closed }

// readErrWatcher is a watcher, not yet started, over handle and a copy that holds each of held,
// on clock, with the package's held waits, a long scan idle wait that no other wait has and no
// stream probe.
func readErrWatcher(t *testing.T, handle bus.KeyValue, clock *clock, held ...string) (*kvwatch.Watcher, *seen, *pending) {
	t.Helper()
	into := newSeen()
	for _, key := range held {
		into.hold(key)
	}
	timers := &pending{}
	w := kvwatch.New("test cache", handle, into, kvwatch.WithNow(clock.Now), kvwatch.WithAfterFunc(timers.afterFunc()),
		kvwatch.WithStreamProbeInterval(0), kvwatch.WithScanIdleTimeout(7*time.Hour))
	t.Cleanup(w.Stop)
	return w, into, timers
}

// countedOnce fails the test unless err is ReadErr's report of a run of one failed read whose
// error wraps want and names what.
func countedOnce(t *testing.T, err, want error, what string) {
	t.Helper()
	if !errors.Is(err, kvwatch.ErrUnverified) || !strings.Contains(err.Error(), "over 1 reads") ||
		(want != nil && !errors.Is(err, want)) || !strings.Contains(err.Error(), what) {
		t.Fatalf("ReadErr past the stale bound = %v, want %v over 1 read, wrapping %v and naming %q", err, kvwatch.ErrUnverified, want, what)
	}
}

// A read whose wait past its barrier times out counts when the stream check after the timeout
// fails, as it does when the check answers: only a check that finds the bucket replaced leaves the
// wait uncounted (TestReadErrCountsNoWaitThatFoundItsBucketReplaced).
func TestReadErrCountsATimedOutWaitWhoseStreamCheckFails(t *testing.T) {
	_, kv := bucket(t, testnats.URL(t))
	clock := newClock()
	unread, failed := &atomic.Bool{}, &atomic.Int64{}
	w, into, timers := readErrWatcher(t, bus.NewKeyValue(kv.Legacy(), kv.WatchHandle(),
		unreadStreams{JetStream: ackedAt{JetStream: kv.Streams(), sequence: 1 << 40}, unread: unread, failed: failed}), clock)
	w.Start()
	eventually(t, "the wait past the barrier to be bounded", func() bool { return len(timers.armed(7*time.Hour)) == 2 })

	unread.Store(true)
	clock.advance(7 * time.Hour)
	timers.last(t, 7*time.Hour).fire()
	eventually(t, "the failed wait's re-read to be armed", func() bool { return len(timers.armed(5*time.Second)) == 1 })
	if failed.Load() == 0 {
		t.Fatal("UNKNOWN: the stream check after the timeout did not fail, so this test says nothing")
	}
	clock.advance(pastTheBound)
	countedOnce(t, w.ReadErr(), nil, "no record at or past the read barrier")
	if done := len(into.completions()); done != 0 {
		t.Fatalf("%d reads completed, want none", done)
	}
}

// A read whose consumer has passed its barrier counts when the list of the bucket's keys it then
// asks for fails (bus.KeyValue.StoredKeys): it removes nothing and is read again.
func TestReadErrCountsAReadWhoseKeyListFailsAfterTheBarrier(t *testing.T) {
	const heldByTheCopy = "held-by-the-copy"
	uri := testnats.URL(t)
	_, kv := bucket(t, uri)
	closed := testnats.Connect(t, uri)
	closed.Close()
	clock := newClock()
	w, into, timers := readErrWatcher(t, bus.NewKeyValue(kv.Legacy(), kv.WatchHandle(), closedConn{JetStream: kv.Streams(), closed: closed}), clock, heldByTheCopy)
	w.Start()
	eventually(t, "the failed read's re-read to be armed", func() bool { return len(timers.armed(5*time.Second)) == 1 })

	clock.advance(pastTheBound)
	countedOnce(t, w.ReadErr(), natsgo.ErrConnectionClosed, "list the keys of KV bucket")
	if done := len(into.completions()); done != 0 || !into.has(heldByTheCopy) {
		t.Fatalf("%d reads completed, the copy holds %v; want none completed and %s kept", done, into.keyList(), heldByTheCopy)
	}
}

// A read whose consumer has passed its barrier counts when a leader read of a key it did not
// deliver fails (bus.KeyValue.HeldFromLeader): it removes nothing and is read again. The row of the
// copy here is no valid subject token, which HeldFromLeader refuses before it sends anything, so
// the failure comes at once and every time.
func TestReadErrCountsAReadWhoseLeaderReadFailsAfterTheBarrier(t *testing.T) {
	const notASubject = "held by the copy"
	_, kv := bucket(t, testnats.URL(t))
	clock := newClock()
	w, into, timers := readErrWatcher(t, kv, clock, notASubject)
	w.Start()
	eventually(t, "the failed read's re-read to be armed", func() bool { return len(timers.armed(5*time.Second)) == 1 })

	clock.advance(pastTheBound)
	countedOnce(t, w.ReadErr(), bus.ErrInvalidSubject, "from the stream's leader")
	if done := len(into.completions()); done != 0 || !into.has(notASubject) {
		t.Fatalf("%d reads completed, the copy holds %v; want none completed and %q kept", done, into.keyList(), notASubject)
	}
}

// A read whose scan times out never reaches its barrier: it is read again and counts for nothing.
func TestReadErrCountsNoReadWhoseScanTimedOut(t *testing.T) {
	_, kv := bucket(t, testnats.URL(t))
	clock := newClock()
	w, into, timers := readErrWatcher(t, scripted(kv, stallsAfter(0)).handle(), clock)
	w.Start()
	eventually(t, "the scan's idle wait to be armed", func() bool { return len(timers.armed(7*time.Hour)) == 1 })

	clock.advance(7 * time.Hour)
	timers.last(t, 7*time.Hour).fire()
	eventually(t, "the timed-out read's re-read to be armed", func() bool { return len(timers.armed(5*time.Second)) == 1 })
	clock.advance(pastTheBound)
	if err := w.ReadErr(); err != nil {
		t.Fatalf("ReadErr past the stale bound after a scan that timed out = %v, want nil", err)
	}
	if done := len(into.completions()); done != 0 {
		t.Fatalf("%d reads completed, want none", done)
	}
}

// A read whose consumer has passed its barrier and whose leader reads succeeded, but whose stream
// check at completion fails, completes nothing and is read again, and counts for nothing: only the
// checks after its barrier fail here.
func TestReadErrCountsNoReadWhoseStreamCheckAtCompletionFails(t *testing.T) {
	_, kv := bucket(t, testnats.URL(t))
	clock := newClock()
	unread, failed := &atomic.Bool{}, &atomic.Int64{}
	w, into, timers := readErrWatcher(t, bus.NewKeyValue(kv.Legacy(), kv.WatchHandle(),
		unreadStreams{JetStream: kv.Streams(), unreadOnPublish: true, unread: unread, failed: failed}), clock)
	w.Start()
	eventually(t, "the unconfirmed read's re-read to be armed", func() bool { return len(timers.armed(5*time.Second)) == 1 })
	if failed.Load() == 0 {
		t.Fatal("UNKNOWN: no stream check failed, so this test says nothing")
	}
	if done := len(into.completions()); done != 0 {
		t.Fatalf("%d reads completed while their stream check failed, want none", done)
	}

	clock.advance(pastTheBound)
	if err := w.ReadErr(); err != nil {
		t.Fatalf("ReadErr past the stale bound after a stream check at completion failed = %v, want nil", err)
	}
}

// A read whose wait past its barrier times out on a stream check that finds the bucket deleted,
// or deleted and created again, waits for nothing more, and the wait counts for nothing. A bucket
// created again is read, and one only deleted is read once it is created (the stream probe's).
func TestReadErrCountsNoWaitThatFoundItsBucketReplaced(t *testing.T) {
	for _, row := range []struct {
		name     string
		recreate bool
		// rereads is how many reads of a new bucket the stream check arms.
		rereads int
	}{
		{name: "the bucket deleted", recreate: false, rereads: 0},
		{name: "the bucket deleted and created again", recreate: true, rereads: 1},
	} {
		t.Run(row.name, func(t *testing.T) {
			conn, kv := bucket(t, testnats.URL(t))
			clock := newClock()
			w, _, timers := readErrWatcher(t, bus.NewKeyValue(kv.Legacy(), kv.WatchHandle(), ackedAt{JetStream: kv.Streams(), sequence: 1 << 40}), clock)
			w.Start()
			eventually(t, "the wait past the barrier to be bounded", func() bool { return len(timers.armed(7*time.Hour)) == 2 })

			js := jetStream(t, conn)
			deleteTestBucket(t, js)
			if row.recreate {
				testnats.RecreateKeyValue(t, js, &natsgo.KeyValueConfig{Bucket: testBucketName})
			}
			clock.advance(7 * time.Hour)
			if err := w.ReadErr(); !errors.Is(err, kvwatch.ErrUnverified) || !strings.Contains(err.Error(), "still waiting") {
				t.Fatalf("ReadErr while the wait is past the stale bound = %v, want the wait reported", err)
			}
			timers.last(t, 7*time.Hour).fire()
			eventually(t, "the replaced wait to end uncounted", func() bool { return w.ReadErr() == nil })
			if n := len(timers.armed(0)); n != row.rereads {
				t.Fatalf("%d reads of a new bucket armed, want %d", n, row.rereads)
			}
			clock.advance(pastTheBound)
			if err := w.ReadErr(); err != nil {
				t.Fatalf("ReadErr past the stale bound after the replaced wait ended = %v, want nil", err)
			}
		})
	}
}

// A read the connection dropped under may no longer complete: its wait still counts while it is
// under way, and its failure, once the wait times out, counts for nothing and arms no re-read of a
// failed read; the reconnect's Rewatch reads the bucket again.
func TestReadErrCountsNoFailureOfAReadTheConnectionDroppedUnder(t *testing.T) {
	_, kv := bucket(t, testnats.URL(t))
	clock := newClock()
	w, _, timers := readErrWatcher(t, bus.NewKeyValue(kv.Legacy(), kv.WatchHandle(), ackedAt{JetStream: kv.Streams(), sequence: 1 << 40}), clock)
	w.Start()
	eventually(t, "the wait past the barrier to be bounded", func() bool { return len(timers.armed(7*time.Hour)) == 2 })

	w.Disconnected()
	clock.advance(7 * time.Hour)
	if err := w.ReadErr(); !errors.Is(err, kvwatch.ErrUnverified) || !strings.Contains(err.Error(), "still waiting") {
		t.Fatalf("ReadErr while the dropped read still waits past the stale bound = %v, want the wait reported", err)
	}
	timers.last(t, 7*time.Hour).fire()
	eventually(t, "the dropped read's wait to end uncounted", func() bool { return w.ReadErr() == nil })
	if n := len(timers.armed(5 * time.Second)); n != 0 {
		t.Fatalf("%d re-reads of a failed read armed, want none", n)
	}
}

// A wait that no failed read precedes ends with its read: once the read completes, ReadErr
// measures nothing from it.
func TestReadErrEndsAWaitWhoseReadCompletes(t *testing.T) {
	_, kv := bucket(t, testnats.URL(t))
	clock := newClock()
	w, into, _ := readErrWatcher(t, kv, clock)
	w.Start()
	eventually(t, "the read's completion", func() bool { return len(into.completions()) == 1 })

	clock.advance(pastTheBound)
	if err := w.ReadErr(); err != nil {
		t.Fatalf("ReadErr past the stale bound after the read completed = %v, want nil", err)
	}
}

// replacedBeforeAck is a bucket's jetstream context whose first publish, a read's barrier, tells
// the test on published and waits on replaced before the context it embeds answers it, so the test
// can delete and create the bucket again between the barrier's write and its acknowledgement.
type replacedBeforeAck struct {
	jetstream.JetStream
	published chan<- struct{}
	replaced  <-chan struct{}
	once      *sync.Once
}

func (s replacedBeforeAck) PublishMsg(ctx context.Context, msg *natsgo.Msg, opts ...jetstream.PublishOpt) (*jetstream.PubAck, error) {
	s.once.Do(func() {
		s.published <- struct{}{}
		<-s.replaced
	})
	return s.JetStream.PublishMsg(ctx, msg, opts...)
}

// A read whose barrier's acknowledgement arrives before its consumer has reached the barrier
// checks the bucket's stream then (consume's acknowledgement branch). A check that finds the
// bucket deleted and created again ends the wait before its bound is armed, and the wait counts
// for nothing: the new bucket is read. TestReadErrCountsNoWaitThatFoundItsBucketReplaced is the
// same finding by the check after a timed-out wait. A bucket only deleted has no row here. Deleted
// before the barrier write, it fails that write, since no stream takes it, and that is a counted
// failure; deleted between the acknowledgement and this check, it is a case no test holds. The
// wait's end (kvwatch.Waiting) is enough to read ReadErr after: settleReplaced does nothing after
// it ends the wait but release readiness.
func TestReadErrCountsNoWaitThatFoundItsBucketReplacedAtItsAcknowledgement(t *testing.T) {
	conn, kv := bucket(t, testnats.URL(t))
	clock := newClock()
	published, replaced := make(chan struct{}), make(chan struct{})
	var release sync.Once
	t.Cleanup(func() { release.Do(func() { close(replaced) }) })
	w, _, timers := readErrWatcher(t, bus.NewKeyValue(kv.Legacy(), kv.WatchHandle(), replacedBeforeAck{
		JetStream: ackedAt{JetStream: kv.Streams(), sequence: 1 << 40}, published: published, replaced: replaced, once: &sync.Once{},
	}), clock)
	w.Start()
	wait(t, published, "the read's barrier write")

	js := jetStream(t, conn)
	deleteTestBucket(t, js)
	testnats.RecreateKeyValue(t, js, &natsgo.KeyValueConfig{Bucket: testBucketName})
	release.Do(func() { close(replaced) })
	eventually(t, "the replaced wait to end", func() bool { return !kvwatch.Waiting(w) })
	clock.advance(pastTheBound)
	if err := w.ReadErr(); err != nil {
		t.Fatalf("ReadErr past the stale bound after the stream check at the acknowledgement found the bucket replaced = %v, want nil", err)
	}
	if n := len(timers.armed(7 * time.Hour)); n != 1 {
		t.Fatalf("%d waits of the idle timeout armed, want only the scan's: the check at the acknowledgement did not end the wait", n)
	}
	if n := len(timers.armed(0)); n != 1 {
		t.Fatalf("%d reads of the new bucket armed, want the one the stream check arms", n)
	}
}

// runSteps is what TestAnUncountedReadLeavesARunAsItWas hands each row's uncounted step: the
// watcher, the bucket it reads and its connection, the watcher's clock and waits, the copy, and how
// many stream checks failed.
type runSteps struct {
	w      *kvwatch.Watcher
	conn   *natsgo.Conn
	kv     bus.KeyValue
	clock  *clock
	timers *pending
	into   *seen
	failed *atomic.Int64
}

// replacedWaitAfterARun is the uncounted step of TestAnUncountedReadLeavesARunAsItWas whose second
// read waits past its barrier while the test deletes the bucket, and creates it again when
// recreate is set, and then times the wait out: the stream check after the timeout finds the
// bucket replaced. rereads is how many reads of a new bucket that check arms. The wait's end
// (kvwatch.Waiting) is enough to read ReadErr after here: settleReplaced does nothing after it
// ends the wait but release readiness, which ReadErr does not read.
func replacedWaitAfterARun(recreate bool, rereads int) func(*testing.T, runSteps) {
	return func(t *testing.T, r runSteps) {
		eventually(t, "the second read's wait past its barrier to be bounded", func() bool { return len(r.timers.armed(7*time.Hour)) == 3 })
		js := jetStream(t, r.conn)
		deleteTestBucket(t, js)
		if recreate {
			testnats.RecreateKeyValue(t, js, &natsgo.KeyValueConfig{Bucket: testBucketName})
		}
		r.clock.advance(7 * time.Hour)
		r.timers.last(t, 7*time.Hour).fire()
		eventually(t, "the replaced wait to end", func() bool { return !kvwatch.Waiting(r.w) })
		if n := len(r.timers.armed(0)); n != rereads {
			t.Fatalf("%d reads of a new bucket armed, want %d", n, rereads)
		}
	}
}

// A read that counts for nothing leaves a run of failed reads as it was: its count, the start it
// is measured from and its last error. The run here is one read whose barrier write fails; the
// read after it takes an uncounted path, and past the bound ReadErr still reports the one failed
// read and its error. The row of a read the connection dropped under waits on two facts. Its wait
// ends at settleFailed's first step (kvwatch.Waiting turns false there), and a record the test
// writes after that is delivered by the same consume goroutine, so once the copy holds it,
// settleFailed has returned and has counted the failure or not.
func TestAnUncountedReadLeavesARunAsItWas(t *testing.T) {
	for _, row := range []struct {
		name string
		// inner is the bucket's jetstream context under the failing barrier write.
		inner func(kv bus.KeyValue, unread *atomic.Bool, failed *atomic.Int64) jetstream.JetStream
		// stalls is whether the second read's scan stalls.
		stalls bool
		// uncounted drives the second read down its uncounted path and returns once it has ended.
		uncounted func(t *testing.T, r runSteps)
	}{
		{
			name:   "a read whose scan timed out",
			inner:  func(kv bus.KeyValue, _ *atomic.Bool, _ *atomic.Int64) jetstream.JetStream { return kv.Streams() },
			stalls: true,
			uncounted: func(t *testing.T, r runSteps) {
				eventually(t, "the second read's scan idle wait", func() bool { return len(r.timers.armed(7*time.Hour)) == 2 })
				r.clock.advance(7 * time.Hour)
				r.timers.last(t, 7*time.Hour).fire()
				eventually(t, "the timed-out read's re-read to be armed", func() bool { return len(r.timers.armed(5*time.Second)) == 2 })
			},
		},
		{
			name: "a read whose stream check at completion failed",
			inner: func(kv bus.KeyValue, unread *atomic.Bool, failed *atomic.Int64) jetstream.JetStream {
				return unreadStreams{JetStream: kv.Streams(), unreadOnPublish: true, unread: unread, failed: failed}
			},
			uncounted: func(t *testing.T, r runSteps) {
				eventually(t, "the unconfirmed read's re-read to be armed", func() bool { return len(r.timers.armed(5*time.Second)) == 2 })
				if r.failed.Load() == 0 {
					t.Fatal("UNKNOWN: no stream check failed, so this row says nothing")
				}
			},
		},
		{
			name: "a wait that found its bucket deleted",
			inner: func(kv bus.KeyValue, _ *atomic.Bool, _ *atomic.Int64) jetstream.JetStream {
				return ackedAt{JetStream: kv.Streams(), sequence: 1 << 40}
			},
			uncounted: replacedWaitAfterARun(false, 0),
		},
		{
			name: "a wait that found its bucket deleted and created again",
			inner: func(kv bus.KeyValue, _ *atomic.Bool, _ *atomic.Int64) jetstream.JetStream {
				return ackedAt{JetStream: kv.Streams(), sequence: 1 << 40}
			},
			uncounted: replacedWaitAfterARun(true, 1),
		},
		{
			name: "a read the connection dropped under",
			inner: func(kv bus.KeyValue, _ *atomic.Bool, _ *atomic.Int64) jetstream.JetStream {
				return ackedAt{JetStream: kv.Streams(), sequence: 1 << 40}
			},
			uncounted: func(t *testing.T, r runSteps) {
				eventually(t, "the second read's wait past its barrier to be bounded", func() bool { return len(r.timers.armed(7*time.Hour)) == 3 })
				r.w.Disconnected()
				r.clock.advance(7 * time.Hour)
				r.timers.last(t, 7*time.Hour).fire()
				eventually(t, "the dropped read's wait to end", func() bool { return !kvwatch.Waiting(r.w) })
				putKeys(t, r.kv, "fence")
				eventually(t, "the copy to take a record written once the wait ended", func() bool { return r.into.has("fence") })
				if n := len(r.timers.armed(10 * time.Second)); n != 0 {
					t.Fatalf("%d re-reads of a run's second failed read armed, want none", n)
				}
			},
		},
	} {
		t.Run(row.name, func(t *testing.T) {
			conn, kv := bucket(t, testnats.URL(t))
			clock := newClock()
			unread, failed, failing := &atomic.Bool{}, &atomic.Int64{}, &atomic.Bool{}
			failing.Store(true)
			handle := bus.NewKeyValue(kv.Legacy(), kv.WatchHandle(), failingBarriers{JetStream: row.inner(kv, unread, failed), failing: failing})
			if row.stalls {
				handle = scripted(handle, nil, stallsAfter(0)).handle()
			}
			w, into, timers := readErrWatcher(t, handle, clock)
			w.Start()
			eventually(t, "the failed read's re-read to be armed", func() bool { return len(timers.armed(5*time.Second)) == 1 })

			failing.Store(false)
			timers.last(t, 5*time.Second).fire()
			row.uncounted(t, runSteps{w: w, conn: conn, kv: kv, clock: clock, timers: timers, into: into, failed: failed})
			clock.advance(pastTheBound)
			countedOnce(t, w.ReadErr(), natsgo.ErrConnectionClosed, "write the read barrier")
			if done := len(into.completions()); done != 0 {
				t.Fatalf("%d reads completed, want none", done)
			}
		})
	}
}
