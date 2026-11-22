package testwait

import (
	"sync"
	"testing"
	"time"
)

// stallTick is how long each sleep of a StallMeter asks for.
const stallTick = 5 * time.Millisecond

// LateSlack is how much later than its own bound code may end beyond the time measured held up
// meanwhile: this process's (StallMeter) and the processes' it started (a caller measures theirs,
// for instance from their run_delay in /proc/<pid>/schedstat). What such code still does past its
// bound, a reap, a directory removed, a few goroutine handoffs, takes milliseconds at the machine's
// own speed; code whose own bound ends late, by seconds, is past it. It is not measured: it is the
// residue neither meter counts.
const LateSlack = time.Second

// StallMeter adds up how long this process was held up while it runs: a goroutine sleeps
// stallTick at a time and counts how much longer each sleep took than that. A loaded machine, a
// CPU quota or a frozen cgroup holds the whole process up, and the meter counts that time; code
// that is late on its own does not move it. A process the row started shares the machine, so a
// machine that holds every process up is counted; one that starves that process alone and not
// this one is not, which is why a caller measures and passes that process's own wait (OnTime).
type StallMeter struct {
	mu       sync.Mutex
	held     time.Duration
	lastWake time.Time
}

// StartStallMeter starts a StallMeter, which runs until t ends.
func StartStallMeter(t testing.TB) *StallMeter {
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

// HeldUp is how long the meter has counted this process held up so far, the sleep it is in now
// included: when a frozen process wakes, the code it timed can end before the meter's goroutine
// has run again.
func (m *StallMeter) HeldUp() time.Duration {
	m.mu.Lock()
	defer m.mu.Unlock()
	held := m.held
	if over := time.Since(m.lastWake) - stallTick; over > 0 {
		held += over
	}
	return held
}

// OnTime fails t where code started at start under a bound it states of its own, bound after start,
// and ended just now, ended later than that bound by more than LateSlack plus the time meter,
// started before start, counted this process held up and othersHeld, the time the processes that
// code depends on were measured waiting to run: that is code whose own bound came late, or that
// waited for something it should not. It logs how late the code was either way. what names the
// code for the failure's text.
func OnTime(t testing.TB, meter *StallMeter, what string, start time.Time, bound, othersHeld time.Duration) {
	t.Helper()
	took := time.Since(start)
	held := meter.HeldUp()
	late := took - bound - held - othersHeld
	if late > LateSlack {
		t.Fatalf("%s ended %s after it started, past its own bound of %s; this process was held up %s of that and the processes it waits on %s, so it was %s late on its own, more than %s",
			what, took.Round(time.Millisecond), bound, held.Round(time.Millisecond), othersHeld.Round(time.Millisecond), late.Round(time.Millisecond), LateSlack)
	}
	t.Logf("on time: %s took %s against its bound of %s, %s held up here, %s by the processes it waits on, %s late on its own (slack %s)",
		what, took.Round(time.Millisecond), bound, held.Round(time.Millisecond), othersHeld.Round(time.Millisecond), late.Round(time.Millisecond), LateSlack)
}
