package main

import (
	"context"
	"sync"
	"testing"
	"time"
)

// Each shared listener's sweep passed once as it started and every interval after, so two listeners
// started together - a rollout of both, a restart of both - passed together from then on, and their
// takeover passes reached the stream's leader as one burst. A sweep passes now at a phase of its own
// in each interval of the clock, read from the listener's pod name (sweepPhaseSeed) and never from
// when it started: two listeners started at the same instant pass apart, by their phases' distance.
//
// The control is two sweeps of one name, started at the same instant: they pass together, so the
// reading tells passes in phase from passes apart.
func TestTwoListenersStartedTogetherSweepOutOfPhase(t *testing.T) {
	// Not parallel: on the wall clock it requires two sweeps of one phase to pass within about
	// 0.67 s of each other, and two whose phases lie 1.34 s apart to pass at least that far apart
	// (half the phases' distance in a 4 s interval).
	const interval = 4 * time.Second
	pods := [2]string{"listener-6c9d8f7b5-q2x7k", "listener-6c9d8f7b5-w7r3t"}
	phases := [2]time.Duration{}
	for i, pod := range pods {
		phases[i] = sweepPhase(pod+" kept role sweep", interval)
	}
	apart := circularDistance(phases[0], phases[1], interval)
	if apart < interval/4 {
		t.Fatalf("the two pods' phases are %s and %s, %s apart in %s: the row needs two names whose phases lie at least a quarter of the interval apart", phases[0], phases[1], apart, interval)
	}
	passTimes := func(phases [2]time.Duration) [2]time.Time {
		t.Helper()
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		var mu sync.Mutex
		var first [2]time.Time
		passed := make(chan struct{}, 2)
		var loops []<-chan struct{}
		for i, phase := range phases {
			loops = append(loops, startSweep(ctx, interval, phase, func(context.Context) {
				mu.Lock()
				defer mu.Unlock()
				if first[i].IsZero() {
					first[i] = time.Now()
					passed <- struct{}{}
				}
			}))
		}
		for range phases {
			select {
			case <-passed:
			case <-time.After(3 * interval):
				t.Fatalf("a sweep made no pass within %s of its start", 3*interval)
			}
		}
		cancel()
		for _, done := range loops {
			<-done
		}
		return first
	}
	gap := func(times [2]time.Time) time.Duration {
		return circularDistance(times[0].Sub(times[1]), 0, interval)
	}

	if got := gap(passTimes(phases)); got < apart/2 {
		t.Fatalf("two sweeps started together passed %s apart, want about %s, the distance of their phases", got, apart)
	}
	if got := gap(passTimes([2]time.Duration{phases[0], phases[0]})); got >= apart/2 {
		t.Fatalf("control: two sweeps of one phase started together passed %s apart, want together", got)
	}
}

// A sweep's passes fall where the clock, less the phase, is a whole number of intervals, read from
// the epoch and never from the listener's start: a listener that restarted passes where it passed
// before, and its first pass comes within one interval of its start. Each row is a clock reading
// and the wait to the next pass; a sweep that counted from its start would wait a whole interval at
// every reading.
func TestASweepsPassesFallAtItsPhaseOfTheClock(t *testing.T) {
	t.Parallel()
	const interval, phase = 5 * time.Minute, 90 * time.Second
	boundary := time.Unix(1_800_000_000-1_800_000_000%300, 0)
	for _, row := range []struct {
		name string
		now  time.Time
		want time.Duration
	}{
		{"at an interval's start", boundary, phase},
		{"just before the phase", boundary.Add(phase - time.Second), time.Second},
		{"at the phase itself, the next interval's", boundary.Add(phase), interval},
		{"just past the phase", boundary.Add(phase + time.Second), interval - time.Second},
		{"at the interval's end", boundary.Add(interval - time.Nanosecond), phase + time.Nanosecond},
	} {
		t.Run(row.name, func(t *testing.T) {
			if got := untilPhase(row.now, interval, phase); got != row.want {
				t.Errorf("untilPhase(%s, %s, %s) = %s, want %s", row.now.UTC().Format(time.RFC3339Nano), interval, phase, got, row.want)
			}
		})
	}
}

// circularDistance is how far apart a and b lie on a circle of length interval.
func circularDistance(a, b, interval time.Duration) time.Duration {
	d := (a - b) % interval
	if d < 0 {
		d += interval
	}
	return min(d, interval-d)
}
