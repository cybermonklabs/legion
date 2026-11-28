package store

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	natsgo "github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/clock"
	"github.com/sjawhar/envoy/internal/testnats"
	"github.com/sjawhar/envoy/internal/testwait"
)

// registryClockStart is where a fake clock a case hands the registry (WithClock, WithTimerClock),
// or the role claim scan (openOnScanClock, role_claim_scan_test.go,
// TestOpenFailsWhenTheRoleRevisionScanStalls), stands until the case moves it. The moment means
// nothing of its own: a case states a row's age, a loop its tick and the scan its deadline against
// that clock and never against the machine's.
var registryClockStart = time.Date(2026, time.November, 22, 9, 0, 0, 0, time.UTC)

// A loop's done channel closes only once a pass in flight when its context ends has finished, and
// no pass starts after the end: the listener's shutdown waits on that channel before it drains
// NATS, so a pass's consumer delete never races the drain.
//
// The loop does no network I/O, so it runs in a testing/synctest bubble, on the real clock the
// bubble fakes: its tick comes once every goroutine of the bubble is blocked, and synctest.Wait
// says when the loop has gone as far as it can once its context has ended, so a read of done then
// proves the loop has not ended while its pass is held (testwait's "Proving an absence").
func TestALoopEndsWithItsContextOnlyAfterItsPassInFlight(t *testing.T) {
	t.Parallel()
	testwait.Bubble(t, func(t testing.TB) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		entered := make(chan struct{})
		release := make(chan struct{})
		var released sync.Once
		releasePass := func() { released.Do(func() { close(release) }) }
		// A case that fails before it releases the pass still lets the loop return, so the bubble
		// ends with no goroutine left blocked in it.
		defer releasePass()
		var passes atomic.Int32
		done := everyInterval(ctx, clock.Real(), time.Minute, func() {
			if passes.Add(1) == 1 {
				close(entered)
				<-release
			}
		})
		testwait.Receive(t, "the loop's first pass", entered)
		cancel()
		// Every goroutine of the bubble but this one is durably blocked once Wait returns, the
		// loop's own in its held pass, so a done closed on the context's end alone is closed by now.
		synctest.Wait()
		select {
		case <-done:
			t.Fatal("the loop reported done with its pass still running")
		default:
		}
		releasePass()
		testwait.Receive(t, "the loop's end once its pass returned", done)
		if got := passes.Load(); got != 1 {
			t.Fatalf("the loop ran %d passes, want none after its context ended", got)
		}
	})
}

// The interest marker collector ends with its context, and by the time it reports done its pass's
// scan has deleted the consumer nats.go created for it: the bucket keeps only the registry's own
// cache watcher's consumer. The collector's ticks and its scan's idle bound run on a fake clock
// (openRegistry's WithTimerClock) that nothing moves: its first pass runs at its start, and its
// scan of a bucket with no key ends at the bucket's end, not at the idle bound.
func TestTheMarkerCollectorDeletesItsScansConsumerBeforeItEnds(t *testing.T) {
	t.Parallel()
	conn, cleanup := connectNATS(t)
	defer cleanup()
	registry := openRegistry(t, conn)
	js, err := conn.JetStream(testnats.MaxWait(t))
	if err != nil {
		t.Fatalf("jetstream: %v", err)
	}
	consumers := func() int {
		count := 0
		for range js.ConsumerNames("KV_" + bus.InterestBucket) {
			count++
		}
		return count
	}
	if got := consumers(); got != 1 {
		t.Fatalf("the interest bucket has %d consumers before the collector, want the cache watcher's one", got)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := registry.StartInterestMarkerCollector(ctx, func() natsgo.JetStreamContext { return js }, time.Hour)
	cancel()
	testwait.Receive(t, "the collector's end after its context ended", done)
	if got := consumers(); got != 1 {
		t.Fatalf("the interest bucket has %d consumers after the collector ended, want the cache watcher's one", got)
	}
}
