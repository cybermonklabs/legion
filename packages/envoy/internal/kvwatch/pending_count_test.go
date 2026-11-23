package kvwatch_test

import (
	"context"
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
)

// The order the round-4 reviews of #4238 derived from nats-server v2.15.1's pending count. Window
// writes A@1 and K@2 reach the copy before the read begins, so the read has no evidence of them;
// the read's barrier is B@3; and while the read waits past it another writer stores A@4 and K@5,
// which replace A@1 and K@2 before the read's consumer has reached them. The consumer skips the
// removed records and delivers B@3, A@4 and K@5 in that order. The server counts each removal of
// an undelivered record at once (consumer.go:7020-7026) and each new record only when its signal
// is processed on another goroutine, after the write was acknowledged (stream.go:7505-7526,
// 8416-8449; consumer.go:7084-7102), and reports a count below zero as 0 (consumer.go:5709-5713):
// B@3's delivery can carry a pending count of 0 while A@4 and K@5 are stored and undelivered. The
// read must still keep A and K, which the bucket holds throughout.
//
// The order the bucket stores and the consumer delivers in is the real server's: the read's own
// consumer is paused from its creation (pausedWatch) until the replacements are stored, so the
// bucket replaces A@1 and K@2 ahead of its cursor. The one value the test supplies is B@3's pending
// count of 0. Which count a delivery carries depends on whether the server's signal goroutine has
// run between a store and a delivery, and nothing outside nats-server can hold that goroutine, so
// the test hands the read the count the server reports when it has not.
func TestAReadKeepsRowsAFalseZeroPendingCountWouldSweep(t *testing.T) {
	uri := testnats.URL(t)
	_, kv := bucket(t, uri)
	_, other := bucket(t, uri)
	paused := newPausedWatch(t, kv.Streams(), true)
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
	// The scan's idle wait and then the wait past the barrier, armed once its write was answered:
	// by then the replacements are stored.
	eventually(t, "the read to wait past its barrier", func() bool { return len(timers.armed(7*time.Hour)) == 2 })
	if err := barrier.failure(); err != nil {
		t.Fatalf("the writes after the barrier: %v", err)
	}
	paused.resume(t)
	eventually(t, "the read's completion", cache.targetDone)

	if got, want := paused.barrierDeliveries(), 1; got != want {
		t.Fatalf("the consumer delivered the barrier %d times, want %d", got, want)
	}
	if got, want := bucketKeys(t, kv), []string{"a", "k"}; !slices.Equal(got, want) {
		t.Fatalf("the bucket holds %v, want %v", got, want)
	}
	if !cache.has("a") || !cache.has("k") {
		t.Fatalf("the completed read left the copy holding %v, want a and k: the bucket replaced them, never removed them", cache.keyList())
	}
}

// The order the round-5 deep review of #4238 derived. The read's consumer is made on an empty
// bucket and paused, so nats.go queues the scan's end at once, having found nothing pending
// (nats.go v1.53.1 jetstream/kv.go:1369-1377). Another listener then writes K@1 in the window,
// through its own connection, so the copy never holds K; the read's barrier is B@2; and the other
// listener writes K@3, which replaces K@1 ahead of the paused consumer. Once the server counts the
// two records the consumer has left, B@2 and K@3, the consumer is resumed: its first delivery is
// B@2 with a true pending count of 1. The read must hold K when it completes, before K@3 is
// delivered: the bucket held K at the read's barrier, and holds it still.
func TestAReadHoldsAWindowKeyOnlyAnotherWriterWrote(t *testing.T) {
	uri := testnats.URL(t)
	_, kv := bucket(t, uri)
	_, other := bucket(t, uri)
	readHoldsAWindowKeyOnlyAnotherWriterWrote(t, kv, other)
}

// readHoldsAWindowKeyOnlyAnotherWriterWrote runs TestAReadHoldsAWindowKeyOnlyAnotherWriterWrote's
// order on the test bucket: kv is the handle the watcher reads, and other a handle on the same
// bucket through another connection, which another listener writes through.
func readHoldsAWindowKeyOnlyAnotherWriterWrote(t *testing.T, kv, other bus.KeyValue) {
	paused := newPausedWatch(t, kv.Streams(), false)
	barrier := &replacingBarrier{JetStream: kv.Streams(), other: other, replace: []string{"k"}, written: map[string]uint64{}}
	cache := newWindowCache(1)
	timers := &pending{}
	w := kvwatch.New("test cache", bus.NewKeyValue(kv.Legacy(), scripted(kv, paused.step()), barrier), cache,
		kvwatch.WithAfterFunc(timers.afterFunc()), kvwatch.WithStreamProbeInterval(0), kvwatch.WithScanIdleTimeout(7*time.Hour))
	t.Cleanup(w.Stop)
	t.Cleanup(cache.letGo)
	w.Start()

	wait(t, paused.held.created, "the read's watch to make and pause its consumer")
	if _, err := other.Put("k", []byte("1")); err != nil {
		t.Fatalf("the other listener's window write of k: %v", err)
	}
	paused.held.let()
	// The scan's idle wait and then the wait past the barrier, armed once its write was answered:
	// by then K@3 is stored.
	eventually(t, "the read to wait past its barrier", func() bool { return len(timers.armed(7*time.Hour)) == 2 })
	if err := barrier.failure(); err != nil {
		t.Fatalf("the write after the barrier: %v", err)
	}
	eventually(t, "the server to count B@2 and K@3 as the consumer's pending records", func() bool { return paused.consumerPending(t) == 2 })
	paused.resume(t)
	eventually(t, "the read's completion", cache.targetDone)

	if got, want := paused.barrierPending(), uint64(1); got != want {
		t.Fatalf("the consumer delivered the barrier with a pending count of %d, want %d: K@3, stored after it", got, want)
	}
	if got, want := bucketKeys(t, kv), []string{"k"}; !slices.Equal(got, want) {
		t.Fatalf("the bucket holds %v, want %v", got, want)
	}
	if !cache.has("k") {
		t.Fatalf("the completed read left the copy holding %v, want k: the bucket held it at the read's barrier", cache.keyList())
	}
}

// A row the read has no evidence of is read from the stream's leader before the read removes it,
// once the read has listed the keys the bucket's stream holds, and only the leader's answer that
// the bucket holds no record of it removes it. A read either of whose questions cannot be answered
// - the connection they go through is closed, for the list and the leader's read, or for the
// leader's read alone - keeps every row and does not complete: it is read again, and a read the
// stream answers removes the row.
func TestARowTheLeaderCannotBeAskedAboutIsKeptAndTheReadFails(t *testing.T) {
	const heldByTheCopy = "held-by-the-copy"
	for _, row := range []struct {
		name            string
		listGoesThrough bool
		failed          string
	}{
		{name: "the list of the bucket's keys", failed: "list the keys of KV bucket " + testBucketName},
		{name: "the leader's read of the row", listGoesThrough: true, failed: "read from the stream's leader a key of KV bucket " + testBucketName},
	} {
		t.Run(row.name, func(t *testing.T) {
			uri := testnats.URL(t)
			_, kv := bucket(t, uri)
			closed := testnats.Connect(t, uri)
			closed.Close()
			failing := &atomic.Bool{}
			streams := leaderReadsThrough{JetStream: kv.Streams(), closed: closed, failing: failing, listGoesThrough: row.listGoesThrough, asked: &atomic.Int32{}}
			// bus.NewKeyValue looks the connection up once, for the client that owns it, so the
			// handle is built before the connection fails: the read's list is then the first
			// lookup made while it fails.
			handle := bus.NewKeyValue(kv.Legacy(), kv.WatchHandle(), streams)
			failing.Store(true)
			into := newSeen()
			into.hold(heldByTheCopy)
			timers := &pending{}
			logger, records := jsonLogger(t)
			w := kvwatch.New("test cache", handle, into, kvwatch.WithAfterFunc(timers.afterFunc()), kvwatch.WithStreamProbeInterval(0),
				kvwatch.WithLogger(logger))
			t.Cleanup(w.Stop)
			w.Start()

			eventually(t, "the read to be read again", func() bool { return len(timers.armed(5*time.Second)) == 1 })
			if done := len(into.completions()); done != 0 || w.Following() || !into.has(heldByTheCopy) {
				t.Fatalf("after the stream could not be asked: %d reads completed, Following %v, copy %v; want none completed, not following, %s kept",
					done, w.Following(), into.keyList(), heldByTheCopy)
			}
			var errs []string
			for _, record := range records() {
				if record["level"] == "WARN" {
					errs = append(errs, fmt.Sprint(record["error"]))
				}
			}
			if len(errs) != 1 || !strings.Contains(errs[0], row.failed) {
				t.Fatalf("the failed read logged %q at WARN, want one error naming %q", errs, row.failed)
			}
			failing.Store(false)
			timers.last(t, 5*time.Second).fire()
			eventually(t, "the read after the stream answers to complete", func() bool { return len(into.completions()) == 1 })
			if into.has(heldByTheCopy) {
				t.Fatalf("after the leader answered that the bucket holds no record of %s, the copy still holds %v", heldByTheCopy, into.keyList())
			}
		})
	}
}

// leaderReadsThrough is a bucket's jetstream context whose connection, while failing is set, is
// closed: the list of the bucket's keys (bus.KeyValue.StoredKeys) and the leader's read of a key
// (HeldFromLeader) build their requests on that connection, so they fail, while the read barrier,
// every stream read and every watch go on as usual. With listGoesThrough, the first connection
// asked for while failing is the live one, and the read's list is what asks for it first: the
// test sets failing only once the handle is built, since bus.NewKeyValue looks the connection up
// for the client that owns it. StoredKeys looks the connection up for each request it sends
// (bus.KeyValue.subjectsPageAt), and the test bucket's list is one page, so the list makes one
// lookup and only the leader's reads fail. A list of more pages would make its second lookup on
// the closed connection and fail as the list, and each row checks that its read's WARN names the
// step that failed, so that would fail the leader-read row rather than pass it for another
// reason.
type leaderReadsThrough struct {
	jetstream.JetStream
	closed          *natsgo.Conn
	failing         *atomic.Bool
	listGoesThrough bool
	asked           *atomic.Int32
}

func (s leaderReadsThrough) Conn() *natsgo.Conn {
	if s.failing.Load() && (!s.listGoesThrough || s.asked.Add(1) > 1) {
		return s.closed
	}
	return s.JetStream.Conn()
}

// pausedWatch is a first read's watch whose consumer the server holds paused from its creation
// until resume (nats-server v2.11+ consumer pause, which stops delivery before the consumer
// fetches its next record, consumer.go:5455-5459): the watch creates its watcher on the bucket,
// pauses the watcher's consumer, and holds the watcher, as heldCreate does, until the test lets
// it go. Records written meanwhile are stored and not delivered, so the bucket can replace one
// ahead of the consumer's cursor. When zeroBarrier is set, each delivery of a read barrier is
// handed on with a pending count of 0.
type pausedWatch struct {
	held        *heldCreate
	streams     jetstream.JetStream
	zeroBarrier bool

	mu       sync.Mutex
	consumer string
	barriers int
	// pending is the pending count the first delivery of a read barrier carried, as the server
	// sent it.
	pending uint64
}

func newPausedWatch(t *testing.T, streams jetstream.JetStream, zeroBarrier bool) *pausedWatch {
	return &pausedWatch{held: newHeldCreate(t), streams: streams, zeroBarrier: zeroBarrier}
}

func (p *pausedWatch) step() watchStep {
	return func(ctx context.Context, kv jetstream.KeyValue) (jetstream.KeyWatcher, error) {
		watcher, err := kv.WatchAll(ctx)
		if err == nil {
			err = p.pause(ctx)
		}
		close(p.held.created)
		<-p.held.release
		if err != nil {
			return watcher, err
		}
		updates := make(chan jetstream.KeyValueEntry)
		go func() {
			defer close(updates)
			for entry := range watcher.Updates() {
				if entry != nil && entry.Key() == bus.BarrierKey {
					p.mu.Lock()
					p.barriers++
					if p.barriers == 1 {
						p.pending = entry.Delta()
					}
					p.mu.Unlock()
					if p.zeroBarrier {
						entry = zeroPending{entry}
					}
				}
				select {
				case updates <- entry:
				case <-ctx.Done():
					return
				}
			}
		}()
		return &forwardedWatcher{KeyWatcher: watcher, updates: updates}, nil
	}
}

// pause pauses the bucket's one consumer, the watcher's, for an hour.
func (p *pausedWatch) pause(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	stream, err := p.streams.Stream(ctx, "KV_"+testBucketName)
	if err != nil {
		return err
	}
	var names []string
	lister := stream.ConsumerNames(ctx)
	for name := range lister.Name() {
		names = append(names, name)
	}
	if err := lister.Err(); err != nil {
		return err
	}
	if len(names) != 1 {
		return &unexpectedConsumers{names: names}
	}
	if _, err := stream.PauseConsumer(ctx, names[0], time.Now().Add(time.Hour)); err != nil {
		return err
	}
	p.mu.Lock()
	p.consumer = names[0]
	p.mu.Unlock()
	return nil
}

// resume resumes the paused consumer.
func (p *pausedWatch) resume(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	p.mu.Lock()
	consumer := p.consumer
	p.mu.Unlock()
	if _, err := p.streams.ResumeConsumer(ctx, "KV_"+testBucketName, consumer); err != nil {
		t.Fatalf("resume the read's consumer %s: %v", consumer, err)
	}
}

// barrierDeliveries is how many deliveries of a read barrier the watch handed on.
func (p *pausedWatch) barrierDeliveries() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.barriers
}

// barrierPending is the pending count the watch's first delivery of a read barrier carried.
func (p *pausedWatch) barrierPending() uint64 {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.pending
}

// consumerPending is the server's count of the records the paused consumer has left to deliver
// (its CONSUMER.INFO's NumPending).
func (p *pausedWatch) consumerPending(t *testing.T) uint64 {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	p.mu.Lock()
	consumer := p.consumer
	p.mu.Unlock()
	handle, err := p.streams.PushConsumer(ctx, "KV_"+testBucketName, consumer)
	if err != nil {
		t.Fatalf("look up the read's consumer %s: %v", consumer, err)
	}
	info, err := handle.Info(ctx)
	if err != nil {
		t.Fatalf("read the read's consumer %s: %v", consumer, err)
	}
	return info.NumPending
}

// unexpectedConsumers is the error of a pause that found other than the one consumer it pauses.
type unexpectedConsumers struct{ names []string }

func (e *unexpectedConsumers) Error() string {
	return "the bucket's stream has consumers " + strings.Join(e.names, ", ") + ", want only the read's"
}

// zeroPending is a delivery handed on with a pending count of 0.
type zeroPending struct{ jetstream.KeyValueEntry }

func (zeroPending) Delta() uint64 { return 0 }
