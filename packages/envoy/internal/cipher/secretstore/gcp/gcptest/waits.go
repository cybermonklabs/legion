// The waits these tests bound by the test binary's deadline, and the stall meter that times code
// against its own deadline less the time this process was held up (StartStallMeter,
// EndsByItsDeadline). They build on every platform: a deadline row times code that runs anywhere.

package gcptest

import (
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/testwait"
)

// cleanupShare is how long a cleanup that runs after a wait gave up may wait for what it has just
// ended (a call it released) before it reports it: a third of testwait.Margin, so two such steps
// and the test's report still fit before go test's alarm. It bounds no verdict of a row that has
// not failed already.
const cleanupShare = testwait.Margin / 3

// untilGiveUp is how long is left before the moment every wait for a fact in these tests gives up:
// the test binary's deadline less testwait.Margin, the clamp testwait.Bounded gives. The day passed
// to it stands for no bound of the wait's own: Bounded answers what is left before that moment when
// it is sooner, and the day itself where there is no deadline (-timeout 0). It is negative once
// that moment has passed.
func untilGiveUp(t *testing.T) time.Duration {
	return testwait.Bounded(t, 24*time.Hour, testwait.Margin)
}

// noteGaveUp writes to stderr, at once, that a wait of t gave up at the binary's deadline less
// testwait.Margin, as testwait's own waits do: without -v a failed subtest's text is printed only
// when its parent test ends, and go test's alarm, should it come first, prints no running test's
// log, so this line is what still names the test whose wait gave up. The caller then fails the
// test with the text that says what it waited for.
func noteGaveUp(t *testing.T) {
	_, _ = fmt.Fprintf(os.Stderr, "testwait: %s: gave up waiting %s before the test binary's deadline\n", t.Name(), testwait.Margin)
}

// heldBeforeGiveUp is testwait.Holds bounded by the moment every wait for a fact in these tests
// gives up (untilGiveUp): whether condition held by then. When it did not, it notes so on stderr
// (noteGaveUp), and the caller fails the test with its own text.
func heldBeforeGiveUp(t *testing.T, condition func() bool) bool {
	if testwait.Holds(untilGiveUp(t), testwait.Poll, condition) {
		return true
	}
	noteGaveUp(t)
	return false
}

// received is the next value ch receives, and true; or, when the moment every wait for a fact in
// these tests gives up passes first, the zero value and false, having noted so on stderr
// (noteGaveUp), and the caller fails the test with its own text. A value ch has by then is still
// taken.
func received[T any](t *testing.T, ch <-chan T) (T, bool) {
	t.Helper()
	giveUp := time.NewTimer(untilGiveUp(t))
	defer giveUp.Stop()
	select {
	case v := <-ch:
		return v, true
	case <-giveUp.C:
		select {
		case v := <-ch:
			return v, true
		default:
			noteGaveUp(t)
			var zero T
			return zero, false
		}
	}
}

// stallTick is how long each sleep of a StallMeter asks for.
const stallTick = 5 * time.Millisecond

// StallMeter adds up how long this process was held up while it runs: a goroutine sleeps
// stallTick at a time and counts how much longer each sleep took than that. A loaded machine, a
// CPU quota or a frozen cgroup holds the whole process up, the code a row times with it, and the
// meter counts that time; code that is late on its own does not move it.
type StallMeter struct {
	mu       sync.Mutex
	held     time.Duration
	lastWake time.Time
}

// StartStallMeter starts a StallMeter, which runs until t ends.
func StartStallMeter(t *testing.T) *StallMeter {
	t.Helper()
	m := &StallMeter{lastWake: time.Now()}
	stop, stopped := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(stopped)
		tick := time.NewTimer(stallTick)
		defer tick.Stop()
		for {
			select {
			case <-stop:
				return
			case <-tick.C:
			}
			m.mu.Lock()
			now := time.Now()
			if over := now.Sub(m.lastWake) - stallTick; over > 0 {
				m.held += over
			}
			m.lastWake = now
			m.mu.Unlock()
			tick.Reset(stallTick)
		}
	}()
	t.Cleanup(func() {
		close(stop)
		<-stopped
	})
	return m
}

// heldUp is how long the meter has counted this process held up so far, the sleep it is in now
// included: when a frozen process wakes, the code it timed can end before the meter's goroutine
// has run again.
func (m *StallMeter) heldUp() time.Duration {
	m.mu.Lock()
	defer m.mu.Unlock()
	held := m.held
	if over := time.Since(m.lastWake) - stallTick; over > 0 {
		held += over
	}
	return held
}

// lateSlack is how much later than its own deadline code in this process may end, beyond the time
// this process was held up meanwhile (StallMeter). What such code still does once its deadline has
// passed takes milliseconds at the machine's own speed, and the meter counts the time the machine
// took from it. Code whose own deadline ends late, by seconds, is past it.
const lateSlack = time.Second

// EndsByItsDeadline waits for done, the end of code in this process started at start under a
// deadline of its own, deadline after start, and answers what done received. The wait is bounded
// only by the moment every wait for a fact in these tests gives up (received), so a machine that
// held the code up does not fail the row; the row fails where the code was late on its own
// (requireOnTime). what names the code for the failure's text.
func EndsByItsDeadline(t *testing.T, meter *StallMeter, what string, start time.Time, deadline time.Duration, done <-chan error) error {
	t.Helper()
	err, ok := received(t, done)
	if !ok {
		t.Fatalf("%s had not ended %s after it started, past its %s deadline, %s before the test binary's deadline", what, time.Since(start).Round(time.Millisecond), deadline, testwait.Margin)
	}
	requireOnTime(t, meter, what, start, deadline)
	return err
}

// requireOnTime fails t where code in this process, started at start under a deadline of its own,
// deadline after start (0 for code that should not wait at all), and ended just now, ended later
// than that deadline by more than lateSlack plus the time meter, started before start, counted
// this process held up: that is code whose own deadline came late, or that waited for something it
// should not. It logs how late it was. what names the code for the failure's text.
func requireOnTime(t *testing.T, meter *StallMeter, what string, start time.Time, deadline time.Duration) {
	t.Helper()
	took := time.Since(start)
	held := meter.heldUp()
	late := took - deadline - held
	if late > lateSlack {
		t.Fatalf("%s ended %s after it started, past its %s deadline; this process was held up %s of that, so the code was %s late on its own, more than %s",
			what, took.Round(time.Millisecond), deadline, held.Round(time.Millisecond), late.Round(time.Millisecond), lateSlack)
	}
	t.Logf("on time: %s took %s against its %s deadline, %s held up, %s late on its own (slack %s)",
		what, took.Round(time.Millisecond), deadline, held.Round(time.Millisecond), late.Round(time.Millisecond), lateSlack)
}
