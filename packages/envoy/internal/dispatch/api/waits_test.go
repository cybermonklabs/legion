package api

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/testwait"
)

// The package's own waits for a fact, beside testwait's. Each ends on its fact, and its one bound
// is the test binary's deadline less testwait's margin, never a span of its own: a loaded machine
// that is merely slow passes, and a fact that never comes fails the test by name, with what was
// last read, before go test's alarm.

// waitUntil asks condition every 10 ms until it holds, failing the test with what when it never
// does (testwait.UntilPolling).
func waitUntil(t *testing.T, what string, condition func() bool) {
	t.Helper()
	testwait.UntilPolling(t, 10*time.Millisecond, what, func() (bool, bool) {
		held := condition()
		return held, held
	})
}

// awaitStill waits until count has not moved for still, reading it every 10 ms, and answers its
// value then: a wait for activity to settle - a hub's recomputes, a pool's acquires, a log's head
// - before a test measures one change alone. still is the length of the calm the test needs, not a
// bound: the wait ends only once count holds still that long, however long the activity runs.
func awaitStill(t *testing.T, what string, still time.Duration, count func() int64) int64 {
	t.Helper()
	last, since := count(), time.Now()
	return testwait.UntilPolling(t, 10*time.Millisecond, fmt.Sprintf("%s to hold still for %s (its last reading)", what, still), func() (int64, bool) {
		if n := count(); n != last {
			last, since = n, time.Now()
		}
		return last, time.Since(since) >= still
	})
}

// pastTheRun is a span longer than any run of a test binary of this package: longer than the
// longest -timeout a command of this repository gives it (the API race step's 70 minutes,
// .github/workflows/ci-job-envoy-go-race-api.yaml). A test sets a product's window or interval to it
// where the row needs that window never to close during the run (a settle it triggers itself, a
// liveness it does not exercise), so a loaded machine cannot close it before the binary's deadline
// ends the test.
const pastTheRun = 24 * time.Hour

// runLeft is what is left of the test binary's run: the time until its deadline, or pastTheRun
// when it runs with none (-timeout 0). A mark set past a product's allowance by runLeft is still
// past it when the server reads it, however late in the run that is, and past it by no more than
// the run itself, so the row still tests the allowance and not a day beyond it.
func runLeft(t *testing.T) time.Duration {
	t.Helper()
	if deadline, ok := t.Deadline(); ok {
		return time.Until(deadline)
	}
	return pastTheRun
}

// queuedWaiters is what releaseOnceQueued last read: the backends queued behind the held lock, or
// the error that ended the reading.
type queuedWaiters struct {
	count int
	err   error
}

// releaseOnceQueued runs release from a goroutine of its own once want backends queue behind the
// lock holder's transaction owns (storetest.LockWaiters), and answers a channel that then receives
// what it last read. It is for a row whose own goroutine is held until the calls it raced answer,
// which they do only once the lock is released, so the wait cannot run on the test's goroutine.
// That goroutine calls no t method: at the test binary's deadline less binaryDeadlineMargin
// (binaryDeadlineGuard) it releases with the count it has, and the row's goroutine fails by name
// on a count short of want.
func releaseOnceQueued(t *testing.T, holder pgx.Tx, want int, release func()) <-chan queuedWaiters {
	t.Helper()
	guard := binaryDeadlineGuard(t)
	queued := make(chan queuedWaiters, 1)
	go func() {
		defer release()
		for {
			count, err := storetest.LockWaiters(context.Background(), holder)
			if err != nil || count >= want {
				queued <- queuedWaiters{count: count, err: err}
				return
			}
			select {
			case <-guard:
				queued <- queuedWaiters{count: count}
				return
			default:
			}
			time.Sleep(10 * time.Millisecond)
		}
	}()
	return queued
}

// stallTick is how often a stallMeter's goroutine wakes.
const stallTick = 5 * time.Millisecond

// stallMeter measures how late this test process runs a goroutine that is ready: a goroutine of
// its own wakes every stallTick, and a wake that comes more than two ticks after the one before
// records how late it came. A row that bounds a product's span by the product's own stated time (a
// close within a heartbeat of the refusal that orders it, a heartbeat's call within a beat of the
// one before) reads its span from the product's records and allows, for each goroutine wake inside
// that span that the row names, the longest single delay the meter saw there: one scheduling delay,
// or one stop of the whole process (a long GC pause). It never allows the sum of the meter's own
// late wakes, which on a loaded machine grows with the span and would let a product that waits on
// one timer pass. A product that waits on its own fails the bound by its wait, unless a single
// stall in the span is as long as that wait divided by the row's wakes.
type stallMeter struct {
	mu       sync.Mutex
	stalls   [][2]time.Time
	lastWake time.Time
}

// startStallMeter starts a stallMeter that runs until the test ends.
func startStallMeter(t *testing.T) *stallMeter {
	t.Helper()
	m := &stallMeter{}
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(stallTick)
		defer ticker.Stop()
		last := time.Now()
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				now := time.Now()
				m.mu.Lock()
				if gap := now.Sub(last); gap > 2*stallTick {
					m.stalls = append(m.stalls, [2]time.Time{last.Add(stallTick), now})
				}
				m.lastWake = now
				m.mu.Unlock()
				last = now
			}
		}
	}()
	t.Cleanup(func() {
		close(stop)
		<-done
	})
	return m
}

// longestStallBetween is the longest single stall the meter saw that overlaps [from, to], the part
// inside it.
func (m *stallMeter) longestStallBetween(from, to time.Time) time.Duration {
	m.mu.Lock()
	defer m.mu.Unlock()
	var longest time.Duration
	for _, s := range m.stalls {
		start, end := s[0], s[1]
		if start.Before(from) {
			start = from
		}
		if end.After(to) {
			end = to
		}
		if d := end.Sub(start); d > longest {
			longest = d
		}
	}
	return longest
}

// wokeAfter says whether the meter has woken after at, and so recorded every stall that ended
// before it: a stall is recorded only at the wake that ends it.
func (m *stallMeter) wokeAfter(at time.Time) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.lastWake.After(at)
}

// requireClosedWithin fails the test unless closedAt comes within bound of orderedAt, both read
// from the product's records, allowing for each of the close's hops - the goroutine wakes between
// the record of the order and the record of the close, which the row counts - the longest single
// stall the meter saw between them: what names the close and its order, for the failure. It reads
// the meter only once the meter has woken after closedAt, so a stop of the process that ended just
// before the close is counted.
func requireClosedWithin(t *testing.T, m *stallMeter, what string, orderedAt, closedAt time.Time, bound time.Duration, hops int) {
	t.Helper()
	waitUntil(t, "the stall meter to wake after the close", func() bool { return m.wokeAfter(closedAt) })
	took := closedAt.Sub(orderedAt)
	longest := m.longestStallBetween(orderedAt, closedAt)
	if allowed := bound + time.Duration(hops)*longest; took > allowed {
		t.Fatalf("%s: closed %s after the refusal that orders it, want within %s plus %d times the longest stall this process saw meanwhile (%s): the product waited on its own", what, took, bound, hops, longest)
	}
}

// requireGapWithin fails the test unless the product's span from from to to, both read from the
// product's records, is within bound plus the longest single stall the meter saw inside it: one
// goroutine wake, such as a heartbeat's next call on its tick. It reads the meter only once the
// meter has woken after to. what names the span, for the failure.
func requireGapWithin(t *testing.T, m *stallMeter, what string, from, to time.Time, bound time.Duration) {
	t.Helper()
	if gap, allowed := gapWithin(t, m, from, to, bound); gap > allowed {
		t.Fatalf("%s: %s, want within %s plus the longest stall this process saw meanwhile (%s)", what, gap, bound, allowed-bound)
	}
}

// gapWithin is the span from from to to and what it is allowed: bound plus the longest single stall
// the meter saw inside it, read once the meter has woken after to.
func gapWithin(t *testing.T, m *stallMeter, from, to time.Time, bound time.Duration) (gap, allowed time.Duration) {
	t.Helper()
	waitUntil(t, "the stall meter to wake after the span", func() bool { return m.wokeAfter(to) })
	return to.Sub(from), bound + m.longestStallBetween(from, to)
}
