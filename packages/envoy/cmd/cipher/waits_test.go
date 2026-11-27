// packages/envoy/cmd/cipher/waits_test.go
//
// The waits this package's tests share. A test waits for a fact it expects: a line on a terminal,
// a process's end, a file, a request a fake saw. Such a wait has one bound, the moment every such
// wait gives up (waitsGaveUp), and no span of its own. A row whose claim is that code honours a
// deadline of its own, or waits for nothing, times that code against its deadline instead,
// allowing for the time this process was held up meanwhile (requireOnTime, endsByItsDeadline).
package main

import (
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/testwait"
)

// waitMargin is how long before the test binary's deadline (go test's -timeout) every wait for a
// fact in these tests gives up, and that moment is the one bound they have (waitsGaveUp). A fact
// that comes ends its wait at once. A fact that never comes fails the test at that moment, with
// the wait's own text, which says what it waited for.
//
// No wait has a span of its own, because a span only says how fast this machine was at that
// moment. On a dev box at a load average of 800 to 1,100 on 288 processors, a 10 s wait for a
// terminal line failed 2 runs of the prompt job family in 25, and one of those tests took 270 s;
// a minute counted from the gaps between polls failed too, when the test process ran and the
// process it waited for was starved.
//
// 30 s is the time a wait that gave up leaves for what must still run before go test's alarm panics
// the binary: the failing test's cleanups (a kill of what it started, and the wait for it), each
// later wait of the test, which then fails at once, and the test's report. The alarm prints no
// running test's log, and without -v a failed subtest's text is printed only once its parent test
// ends, so a margin those steps outlast loses the text that names the wait.
const waitMargin = 30 * time.Second

// cleanupShare is how long a cleanup that runs after a wait gave up may wait for what it has just
// ended (a call it released, a helper it interrupted) before it kills or reports it: a third of
// waitMargin, so two such steps and the test's report still fit before go test's alarm. It bounds
// no verdict of a row that has not failed already.
const cleanupShare = waitMargin / 3

// untilGiveUp is how long is left before the moment every wait for a fact in these tests gives up:
// the test binary's deadline less waitMargin, the clamp testwait.Bounded gives. The day passed to
// it stands for no bound of the wait's own: Bounded answers what is left before that moment when
// it is sooner, and the day itself where there is no deadline (-timeout 0). It is negative once
// that moment has passed.
func untilGiveUp(t *testing.T) time.Duration {
	return testwait.Bounded(t, 24*time.Hour, waitMargin)
}

// waitsGaveUp reports whether the moment every wait for a fact in these tests gives up has passed
// (untilGiveUp). Under -timeout 0 it never has. The prompt job helper has the deadline
// helperTimeout gives it, so its waits give up before the test's. When it has passed, it notes so
// on stderr (noteGaveUp) before the caller fails the test with its own text.
func waitsGaveUp(t *testing.T) bool {
	if untilGiveUp(t) > 0 {
		return false
	}
	noteGaveUp(t)
	return true
}

// noteGaveUp writes to stderr, at once, that a wait of t gave up at the binary's deadline less
// waitMargin, as testwait's own waits do (testwait.gaveUp): without -v a failed subtest's text is
// printed only when its parent test ends, and go test's alarm, should it come first, prints no
// running test's log, so this line is what still names the test whose wait gave up. The caller
// then fails the test with the text that says what it waited for.
func noteGaveUp(t *testing.T) {
	_, _ = fmt.Fprintf(os.Stderr, "testwait: %s: gave up waiting %s before the test binary's deadline\n", t.Name(), waitMargin)
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
// (noteGaveUp), and the caller fails the test with its own text, after anything it must kill
// first. A value ch has by then is still taken.
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

// stallTick is how long each sleep of a stallMeter asks for.
const stallTick = 5 * time.Millisecond

// stallMeter adds up how long this process was held up while it runs: a goroutine sleeps
// stallTick at a time and counts how much longer each sleep took than that. A loaded machine, a
// CPU quota or a frozen cgroup holds the whole process up, the code a row times with it, and the
// meter counts that time; code that is late on its own does not move it. Code in another process
// the row started shares the machine, so a machine that holds every process up is counted; one
// that starves that process alone and not this one is not, and only lateSlack covers it. It keeps
// each stall too (heldAround), for a step of another process, which this process's hold-ups delay
// only where they fall on a moment this process marks.
type stallMeter struct {
	mu       sync.Mutex
	held     time.Duration
	lastWake time.Time
	stalls   []stall
}

// stall is one sleep of a stallMeter that ran past stallTick: the span past it, from start to end.
type stall struct{ start, end time.Time }

// startStallMeter starts a stallMeter, which runs until t ends.
func startStallMeter(t *testing.T) *stallMeter {
	t.Helper()
	m := &stallMeter{lastWake: time.Now()}
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
				m.stalls = append(m.stalls, stall{start: now.Add(-over), end: now})
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
func (m *stallMeter) heldUp() time.Duration {
	m.mu.Lock()
	defer m.mu.Unlock()
	held := m.held
	if over := time.Since(m.lastWake) - stallTick; over > 0 {
		held += over
	}
	return held
}

// markWindow is how near a mark a stall must reach to count as holding that mark up (heldAround):
// two of the meter's ticks, so the stall in progress when the mark is taken, and the one the meter
// ends just before the code that takes the mark runs, both count.
const markWindow = 2 * stallTick

// heldAround is how long this process was held up in the stalls that reach within markWindow of
// at, the sleep the meter is in now included: the hold that can have made late a moment this
// process marked at at (a fake's mark of a request it got to only once the process ran again).
// A stall elsewhere, while this process only waited, delayed no mark, and is not in it.
func (m *stallMeter) heldAround(at time.Time) time.Duration {
	m.mu.Lock()
	defer m.mu.Unlock()
	lo, hi := at.Add(-markWindow), at.Add(markWindow)
	var held time.Duration
	reaches := func(s stall) {
		if s.end.After(lo) && s.start.Before(hi) {
			held += s.end.Sub(s.start)
		}
	}
	for _, s := range m.stalls {
		reaches(s)
	}
	if now := time.Now(); now.Sub(m.lastWake) > stallTick {
		reaches(stall{start: m.lastWake.Add(stallTick), end: now})
	}
	return held
}

// lateSlack is how much later than its own deadline code in this process may end, beyond the time
// this process was held up meanwhile (stallMeter). What such code still does once its deadline has
// passed - a cleanup call to a fake in this process, the report of its failure - takes
// milliseconds at the machine's own speed, and the meter counts the time the machine took from it;
// the most lateness a row measured beyond that, frozen 16 s after every 0.25 s of running at a load
// of 800, is in the pull request's record. Code whose own deadline ends late, by seconds, is past it.
const lateSlack = time.Second

// endsByItsDeadline waits for ch, the end of code in this process started at start under a
// deadline of its own, deadline after start, and answers what ch received. The wait is bounded
// only by the moment every wait for a fact in these tests gives up (received), so a machine that
// held the code up does not fail the row; the row fails where the code was late on its own
// (requireOnTime). what names the code for the failure's text.
func endsByItsDeadline[T any](t *testing.T, meter *stallMeter, what string, start time.Time, deadline time.Duration, ch <-chan T) T {
	t.Helper()
	v, ok := received(t, ch)
	if !ok {
		t.Fatalf("%s had not ended %s after it started, past its %s deadline, %s before the test binary's deadline", what, time.Since(start).Round(time.Millisecond), deadline, waitMargin)
	}
	requireOnTime(t, meter, what, start, deadline)
	return v
}

// requireOnTime fails t where code in this process, started at start under a deadline of its own,
// deadline after start (0 for code that should not wait at all), and ended just now, ended later
// than that deadline by more than lateSlack plus the time meter, started before start, counted
// this process held up: that is code whose own deadline came late, or that waited for something it
// should not. It answers how long the code took, and logs how late it was. what names the code for
// the failure's text.
//
// A command in another process (an agent-secrets binary a row runs, a helper subprocess) is not
// timed this way: a machine can starve that process alone, which the meter, counting this
// process's hold-ups, does not see. Its rows read the cause of its end instead: the signal its
// wait status names, or the cause its own answer gives, with the mechanism the row rules out held
// past the moment its waits give up.
func requireOnTime(t *testing.T, meter *stallMeter, what string, start time.Time, deadline time.Duration) time.Duration {
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
	return took
}
