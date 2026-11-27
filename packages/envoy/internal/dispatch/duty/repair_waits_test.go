package duty_test

import (
	"context"
	"testing"
	"testing/synctest"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/architecture"
	"github.com/sjawhar/envoy/internal/dispatch/delivery"
	"github.com/sjawhar/envoy/internal/dispatch/duty"
)

// Where an event never arrives, Dispatch's own timers bring the record up to date with reads
// after five minutes. Two of its five-minute GitHub readers run in duty.RunJittered:
// architecture.Run on architecture.SyncInterval, and delivery.Reconcile.Run on
// delivery.ReconcileInterval. This drives duty.RunJittered with each of those constants on a
// synctest fake clock for twenty intervals and measures the wait between passes: no wait may be
// longer than the interval, whatever the jitter draws. The control fixes the draw to zero, which
// makes every wait the interval itself; the loop's own draw is the random one a deployment makes;
// and the largest draw fixes it at the top of its range, which gives the shortest wait the loop
// leaves, and gave the longest, just under 1.1 intervals, where the draw was added to the
// interval.
//
// It names nothing but fixJitter, passClock, duty.RunJittered and the two constants, so the same
// file runs against a build whose waits overran the interval and shows the overrun there.
func TestTheFiveMinuteReadsWaitAtMostFiveMinutesBetweenPasses(t *testing.T) {
	// Not parallel: its rows fix duty.JitterN (fixJitter), the package variable every jittered
	// wait reads.
	for _, loop := range []struct {
		name     string
		interval time.Duration
	}{
		{"architecture_import", architecture.SyncInterval},
		{"delivery_reconcile", delivery.ReconcileInterval},
	} {
		t.Run(loop.name, func(t *testing.T) {
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
						fixJitter(t, tc.draw)
					}
					synctest.Test(t, func(t *testing.T) {
						var passes passClock
						ctx, cancel := context.WithCancel(context.Background())
						done := make(chan struct{})
						go func() {
							defer close(done)
							duty.RunJittered(ctx, loop.interval, func() bool { return false }, func(context.Context, time.Time) { passes.record() })
						}()
						time.Sleep(20 * loop.interval)
						cancel()
						<-done

						at := passes.times()
						if len(at) < 10 {
							t.Fatalf("passes in twenty intervals = %d, want at least 10", len(at))
						}
						var longest time.Duration
						late := 0
						for i := 1; i < len(at); i++ {
							gap := at[i].Sub(at[i-1])
							longest = max(longest, gap)
							if gap > loop.interval {
								late++
							}
						}
						t.Logf("%s: stated wait %v; passes %d; waits longer than stated %d of %d; longest wait %v",
							loop.name, loop.interval, len(at), late, len(at)-1, longest)
						if late > 0 {
							t.Errorf("%d of %d waits between %s passes were longer than the stated %v, the longest %v",
								late, len(at)-1, loop.name, loop.interval, longest)
						}
					})
				})
			}
		})
	}
}
