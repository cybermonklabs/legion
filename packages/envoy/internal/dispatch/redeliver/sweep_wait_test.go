package redeliver_test

import (
	"context"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/duty"
	"github.com/sjawhar/envoy/internal/dispatch/redeliver"
)

// Where an event never arrives, Dispatch's own timers bring the record up to date, the webhook
// redelivery with a sweep after two minutes: cmd/dispatch runs the sweep as
// sweeper.Run(ctx, redeliver.Interval, leases). This drives the same loop through the package's
// test door (RunUnderLease, export_test.go) on a synctest fake clock for twenty intervals, with a
// lease gate of its own that records when each sweep was due and runs no sweep, and measures the
// wait between sweeps: no wait may be longer than redeliver.Interval, whatever the jitter draws.
// The control fixes the draw to zero, which makes every wait the interval itself; the loop's own
// draw is the random one a deployment makes; and the largest draw fixes it at the top of its range,
// which gives the shortest wait the loop leaves, and gave the longest, just under 1.1 intervals,
// where the draw was added to the interval.
//
// It names nothing but redeliver.FixJitter, Sweeper.RunUnderLease, redeliver.Interval and
// duty.Leases, so the same file runs against a build whose waits overran the interval and shows
// the overrun there.
func TestTheSweepWaitsAtMostTwoMinutesBetweenSweeps(t *testing.T) {
	// Not parallel: its rows write the package variable jittered (redeliver.FixJitter), which
	// every sweep loop draws its waits from.
	for _, tc := range []struct {
		name string
		// draw fixes the jitter's draw for its bound; nil keeps the loop's own.
		draw func(n time.Duration) time.Duration
	}{
		{"control_no_jitter", func(time.Duration) time.Duration { return 0 }},
		{"the_loops_own_draw", nil},
		{"largest_draw", func(n time.Duration) time.Duration { return n - 1 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.draw != nil {
				redeliver.FixJitter(t, tc.draw)
			}
			synctest.Test(t, func(t *testing.T) {
				var mu sync.Mutex
				var dues []time.Time
				gate := func(_ context.Context, _ duty.Leases, _ string, due time.Time, _ time.Duration, _ func(context.Context)) bool {
					mu.Lock()
					defer mu.Unlock()
					dues = append(dues, due)
					return true
				}
				ctx, cancel := context.WithCancel(context.Background())
				done := make(chan struct{})
				go func() {
					defer close(done)
					(&redeliver.Sweeper{}).RunUnderLease(ctx, redeliver.Interval, gate)
				}()
				time.Sleep(20 * redeliver.Interval)
				cancel()
				<-done

				mu.Lock()
				defer mu.Unlock()
				if len(dues) < 10 {
					t.Fatalf("sweeps due in twenty intervals = %d, want at least 10", len(dues))
				}
				var longest time.Duration
				late := 0
				for i := 1; i < len(dues); i++ {
					gap := dues[i].Sub(dues[i-1])
					longest = max(longest, gap)
					if gap > redeliver.Interval {
						late++
					}
				}
				t.Logf("stated wait %v; sweeps due %d; waits longer than stated %d of %d; longest wait %v",
					redeliver.Interval, len(dues), late, len(dues)-1, longest)
				if late > 0 {
					t.Errorf("%d of %d waits between sweeps were longer than the stated %v, the longest %v",
						late, len(dues)-1, redeliver.Interval, longest)
				}
			})
		})
	}
}
