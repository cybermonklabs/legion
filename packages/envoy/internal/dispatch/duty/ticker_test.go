package duty_test

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/sjawhar/envoy/internal/clock/clocktest"
	"github.com/sjawhar/envoy/internal/dispatch/duty"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/testwait"
)

// fixJitter makes every jittered wait draw what draw answers for its bound, until the test ends.
func fixJitter(t *testing.T, draw func(n time.Duration) time.Duration) {
	t.Helper()
	saved := *duty.JitterN
	*duty.JitterN = draw
	t.Cleanup(func() { *duty.JitterN = saved })
}

// passClock records the fake-clock time each pass started at.
type passClock struct {
	mu sync.Mutex
	at []time.Time
}

func (c *passClock) record() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.at = append(c.at, time.Now())
}

func (c *passClock) times() []time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]time.Time(nil), c.at...)
}

// Every wait after a pass is the interval less up to a tenth of it, never more: at the smallest
// draw the gap is exactly the interval, at the largest it is just over 0.9 of one. So a pass comes
// at most one interval after the pass before it, two processes' ticks do not stay in step, and
// every gap is longer than the lease period a leased pass hands Exclusive (duty.LeasePeriod), so no
// tick lands inside a period its own previous pass served. Each pass is handed the moment its timer
// fired as its due time, the moment a leased pass buckets its period by (Exclusive).
func TestRunJitteredWaitsMoreThanNineTenthsAndAtMostOneIntervalAfterEveryPass(t *testing.T) {
	const interval = 5 * time.Minute
	period := duty.LeasePeriod(interval)
	for _, tc := range []struct {
		name string
		draw func(n time.Duration) time.Duration
		gap  time.Duration
	}{
		{name: "smallest draw", draw: func(time.Duration) time.Duration { return 0 }, gap: interval},
		{name: "largest draw", draw: func(n time.Duration) time.Duration { return n - 1 }, gap: interval - interval/10 + 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fixJitter(t, tc.draw)
			synctest.Test(t, func(t *testing.T) {
				var passes passClock
				ctx, cancel := context.WithCancel(context.Background())
				done := make(chan struct{})
				booted := time.Now()
				go func() {
					defer close(done)
					duty.RunJittered(ctx, interval, func() bool { return false }, func(_ context.Context, due time.Time) {
						if now := time.Now(); !due.Equal(now) {
							t.Errorf("a pass at %v was handed due %v, want the moment its timer fired", now.Sub(booted), due.Sub(booted))
						}
						passes.record()
					})
				}()
				time.Sleep(6 * interval)
				cancel()
				// done is read once, after every goroutine of the bubble is durably blocked
				// (synctest.Wait), and is not waited for: a loop that ignored its cancel keeps its
				// next tick's timer armed, so a receive that waited for done would run the
				// bubble's clock through that loop's ticks until go test's alarm. The read fails
				// such a loop by name.
				synctest.Wait()
				select {
				case <-done:
				default:
					t.Fatal("RunJittered had not returned once its context ended")
				}

				at := passes.times()
				if len(at) < 6 {
					t.Fatalf("passes in six intervals = %d, want at least 6", len(at))
				}
				if start := at[0].Sub(booted); start < 0 || start >= interval {
					t.Errorf("first pass %v after boot, want within [0, %v)", start, interval)
				}
				for i := 1; i < len(at); i++ {
					gap := at[i].Sub(at[i-1])
					if gap <= period || gap > interval {
						t.Errorf("gap before pass %d = %v, want within (%v, %v]", i+1, gap, period, interval)
					}
					if gap != tc.gap {
						t.Errorf("gap before pass %d = %v, want %v for this draw", i+1, gap, tc.gap)
					}
				}
			})
		})
	}
}

// A caller with nothing to do never starts the loop.
func TestRunJitteredDoesNotStartWhenIdle(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ran := false
		duty.RunJittered(context.Background(), time.Minute, func() bool { return true }, func(context.Context, time.Time) { ran = true })
		if ran {
			t.Error("an idle loop ran a pass")
		}
	})
}

// A loop that speeds up while work remains - the block-index reconciliation asks for a two-second
// step after a full batch and the full interval otherwise - must make its first pass at start and
// then every pass at the step it asked for, with the step as each tick's period. A loop that held
// its ticks to the interval, or judged a step's tick by the interval's period, would serve the
// step's ticks an interval apart: one batch per interval while thousands of blocks wait to be
// indexed. And no tick may come sooner than its wait after the pass before it ended, which is what
// keeps two of a lone loop's ticks out of one period. The loop runs on a fake clock
// (RunLeasedAfterEachPassOn), each pass moves it by the pass's own time, and the row reads where
// the loop arms each next tick, its own record of when it will act, before it moves there.
func TestRunLeasedAfterEachPassTicksAtStartAndThenAtTheWaitEachPassAsksFor(t *testing.T) {
	leases := leasesOf(t, storetest.Open(t))
	const interval, step = time.Hour, 100 * time.Millisecond
	const passes = 4

	ticks := clocktest.New(time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC))
	started := ticks.Now()
	type passTimes struct{ began, ended time.Time }
	ran := make(chan passTimes)
	ctx, stop := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		duty.RunLeasedAfterEachPassOn(ctx, ticks, leases, "block-index", interval, func(ctx context.Context, _ time.Duration) time.Duration {
			began := ticks.Now()
			// A pass's own time, so a loop that ticks on a fixed cadence instead of after the
			// pass arms its next tick less than the step after the pass ended.
			ticks.Advance(step / 2)
			select {
			case ran <- passTimes{began: began, ended: ticks.Now()}:
			case <-ctx.Done():
			}
			return step
		})
	}()
	defer func() {
		stop()
		testwait.Receive(t, "the block-index loop (RunLeasedAfterEachPassOn) to return once its context ended", done)
	}()

	var got []passTimes
	for len(got) < passes {
		pass := testwait.Receive(t, "the loop's next pass, at the step the pass before it asked for", ran)
		got = append(got, pass)
		if len(got) == passes {
			break
		}
		armed := ticks.AwaitArmed(t, fmt.Sprintf("the tick after pass %d", len(got)), 1)
		if want := pass.ended.Add(step); len(armed) != 1 || !armed[0].Equal(want) {
			t.Fatalf("after pass %d ended the loop armed %v; want one tick, the %v it asked for after the pass ended (%v)",
				len(got), armed, step, want)
		}
		ticks.Set(armed[0])
	}
	if !got[0].began.Equal(started) {
		t.Errorf("the first pass came %v after start; want it at start", got[0].began.Sub(started))
	}
	for i := 1; i < len(got); i++ {
		if gap := got[i].began.Sub(got[i-1].ended); gap != step {
			t.Errorf("pass %d began %v after pass %d ended; want the %v it asked for", i+1, gap, i, step)
		}
	}
}

// A loop whose first tick is one interval in - the issue-ask repair (the boot makes its own pass
// first) - must not pass at start: the issue-ask repair's boot pass would run twice on each
// deploy. The loop runs on a fake clock (RunLeasedOn), which the row moves to the tick it arms.
func TestRunLeasedWaitsAnIntervalBeforeTheFirstTickWhenAsked(t *testing.T) {
	leases := leasesOf(t, storetest.Open(t))
	const interval = 300 * time.Millisecond

	ticks := clocktest.New(time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC))
	started := ticks.Now()
	ran := make(chan time.Time, 1)
	ctx, stop := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		duty.RunLeasedOn(ctx, ticks, leases, "issue-ask-repair", interval, duty.TickAfterInterval, func(context.Context) {
			select {
			case ran <- ticks.Now():
			default:
			}
		})
	}()
	defer func() {
		stop()
		testwait.Receive(t, "the issue-ask-repair loop (RunLeasedOn) to return once its context ended", done)
	}()

	if armed := ticks.AwaitArmed(t, "the loop's first tick", 1); !armed[0].Equal(started.Add(interval)) {
		t.Errorf("the loop armed its first tick for %v after start; want one interval, %v, in", armed[0].Sub(started), interval)
	}
	ticks.Set(started.Add(interval))
	began := testwait.Receive(t, "the loop's first pass", ran)
	if first := began.Sub(started); first < interval {
		t.Errorf("the first pass came %v after start; want one interval, %v, in", first, interval)
	}
}
