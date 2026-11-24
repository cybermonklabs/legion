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

// circularDistance is how far apart a and b lie on a circle of length interval.
func circularDistance(a, b, interval time.Duration) time.Duration {
	d := (a - b) % interval
	if d < 0 {
		d += interval
	}
	return min(d, interval-d)
}
