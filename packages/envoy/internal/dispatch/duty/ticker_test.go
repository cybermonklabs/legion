package duty_test

import (
	"context"
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
	// Not parallel: its rows fix duty.JitterN (fixJitter), the package variable every jittered
	// wait reads.
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
				// A blocking receive would let a loop that ignored cancellation keep ticking.
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
	t.Parallel()
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
// keeps two of a lone loop's ticks out of one period.
func TestRunLeasedAfterEachPassTicksAtStartAndThenAtTheWaitEachPassAsksFor(t *testing.T) {
	t.Parallel()
	leases := leasesOf(t, storetest.Open(t))
	const interval, step = time.Hour, 100 * time.Millisecond
	const passes = 4

	type passTimes struct {
		began, ended time.Time
		period       time.Duration
	}
	ran := make(chan passTimes, 1)
	tickSet := make(chan struct{}, 1)
	tickSet <- struct{}{}
	ctx, stop := context.WithCancel(context.Background())
	done := make(chan struct{})
	fake := clocktest.New(tickDue)
	go func() {
		defer close(done)
		duty.RunLeasedAfterEachPassOn(ctx, fake, nil, leases, "block-index", interval, func(ctx context.Context, period time.Duration) time.Duration {
			// The test's Set must finish before the pass moves the clock again.
			select {
			case <-tickSet:
			case <-ctx.Done():
				return step
			}
			began := fake.Now()
			// A pass takes time so a timer measured from its tick's due arms too early.
			fake.Advance(step / 2)
			select {
			case ran <- passTimes{began: began, ended: fake.Now(), period: period}:
			case <-ctx.Done():
			}
			return step
		})
	}()
	t.Cleanup(func() {
		stop()
		testwait.Receive(t, "the pass-paced loop to return after cancellation", done)
	})

	wantStart, wantPeriod := tickDue, interval
	for i := 1; i <= passes; i++ {
		p := testwait.Receive(t, "the next block-index pass", ran)
		if !p.began.Equal(wantStart) || p.period != wantPeriod {
			t.Fatalf("pass %d started at %s with period %v; want %s with period %v",
				i, p.began.Format(time.RFC3339Nano), p.period, wantStart.Format(time.RFC3339Nano), wantPeriod)
		}
		wantStart, wantPeriod = p.ended.Add(step), step
		armed := fake.AwaitArmed(t, "the wait after the block-index pass", 1)
		if len(armed) != 1 || !armed[0].Equal(wantStart) {
			t.Fatalf("after pass %d ended at %s, timers armed at %v; want just %s (%v later)",
				i, p.ended.Format(time.RFC3339Nano), armed, wantStart.Format(time.RFC3339Nano), step)
		}
		if i < passes {
			fake.Set(armed[0])
			tickSet <- struct{}{}
		}
	}
}

// A loop whose first tick is one interval in - the issue-ask repair (the boot makes its own pass
// first) - must not pass at start: the boot pass would run twice on each deploy. Cancellation
// during the first pass waits for that pass to return and stops the ticker before the loop returns.
func TestRunLeasedWaitsAnIntervalBeforeTheFirstTickWhenAsked(t *testing.T) {
	t.Parallel()
	const interval = 300 * time.Millisecond
	const name = "issue-ask-repair"
	first := tickDue.Add(interval)
	t.Run("first tick", func(t *testing.T) {
		t.Parallel()
		leases := leasesOf(t, storetest.Open(t))
		fake := clocktest.New(tickDue)
		ran := make(chan time.Time, 1)
		ctx, stop := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() {
			defer close(done)
			duty.RunLeasedOn(ctx, fake, nil, leases, name, interval, duty.TickAfterInterval, func(context.Context) {
				select {
				case ran <- fake.Now():
				default:
				}
			})
		}()
		t.Cleanup(func() {
			stop()
			testwait.Receive(t, "the fixed-cadence loop to return after cancellation", done)
		})

		fake.AwaitArmedAt(t, "the first issue-ask-repair tick", first)
		select {
		case at := <-ran:
			t.Fatalf("the first pass ran at %s before the first tick at %s", at, first)
		default:
		}
		fake.Set(first)
		if at := testwait.Receive(t, "the first issue-ask-repair pass", ran); !at.Equal(first) {
			t.Fatalf("the first pass ran at %s, want %s", at, first)
		}
		fake.AwaitArmedAt(t, "the second tick, after the first pass returns", first.Add(interval))
	})

	t.Run("cancel during the first pass", func(t *testing.T) {
		t.Parallel()
		leases := leasesOf(t, storetest.Open(t))
		fake := clocktest.New(tickDue)
		entered, release, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
		letPassReturn := sync.OnceFunc(func() { close(release) })
		ctx, stop := context.WithCancel(context.Background())
		waitCtx := testwait.Context(t)
		go func() {
			defer close(done)
			duty.RunLeasedOn(ctx, fake, nil, leases, name, interval, duty.TickAfterInterval, func(context.Context) {
				close(entered)
				select {
				case <-release:
				case <-waitCtx.Done():
				}
			})
		}()
		t.Cleanup(func() {
			stop()
			letPassReturn()
			testwait.Receive(t, "the loop to finish after releasing its first pass", done)
		})

		testwait.Until(t, "the first tick of the loop to cancel to arm before its pass starts", func() ([]time.Time, bool) {
			select {
			case <-entered:
				t.Fatal("the loop entered its first pass before the test fired its first tick")
			default:
			}
			armed := fake.Armed()
			return armed, len(armed) == 1 && armed[0].Equal(first)
		})
		fake.Set(first)
		testwait.Receive(t, "the first pass to enter", entered)
		stop()
		select {
		case <-done:
			t.Fatal("the loop returned while its first pass was still blocked")
		default:
		}
		letPassReturn()
		testwait.Receive(t, "the cancelled loop to return after its first pass", done)
		if armed := fake.Armed(); len(armed) != 0 {
			t.Fatalf("the loop returned with ticks still armed at %v", armed)
		}
	})
}
