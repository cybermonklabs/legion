package duty_test

import (
	"context"
	"log/slog"
	"slices"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/duty"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// Most rows below run inside a synctest bubble, so the loop's timers run on a fake clock: an hour
// of idleness costs nothing, and a pass's start time is exact. The lease in those rows is a fake
// of Exclusive's shape (fakeLease); the last row takes a real one from Postgres.

// signal wakes the loop the way a pglisten.Hub does: a non-blocking send into a buffer of one.
func signal(wake chan struct{}) {
	select {
	case wake <- struct{}{}:
	default:
	}
}

// runLoop runs loop with exclusive in place of Exclusive until the returned cancel is called, and
// done closes once it has returned.
func runLoop(loop duty.WokenLoop, exclusive duty.ExclusiveFunc) (cancel context.CancelFunc, done <-chan struct{}) {
	ctx, cancel := context.WithCancel(context.Background())
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		loop.Run(ctx, exclusive)
	}()
	return cancel, finished
}

// stop cancels the loop and waits for it to return.
func stop(cancel context.CancelFunc, done <-chan struct{}) {
	cancel()
	<-done
}

// wantPassesAt fails unless the passes started exactly offsets after start, and no other pass did.
func wantPassesAt(t *testing.T, passes *passClock, start time.Time, offsets ...time.Duration) {
	t.Helper()
	at := passes.times()
	got := make([]time.Duration, len(at))
	for i, when := range at {
		got[i] = when.Sub(start)
	}
	if len(got) != len(offsets) {
		t.Fatalf("passes started at %v after boot, want %v", got, offsets)
	}
	for i := range got {
		if got[i] != offsets[i] {
			t.Fatalf("passes started at %v after boot, want %v", got, offsets)
		}
	}
}

// noLease is an exclusive the rows without a lease never call.
func noLease(t *testing.T) duty.ExclusiveFunc {
	return func(context.Context, *slog.Logger, duty.Leases, string, time.Time, time.Duration, func(context.Context)) bool {
		t.Error("a loop with no lease asked for one")
		return false
	}
}

// After its boot pass a loop with no wake and no due time does nothing at all - no pass in an hour,
// so no timer is armed either, since an armed timer would have fired one.
func TestAWokenLoopRunsOnceAtStartAndThenNothingWhileIdle(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var passes passClock
		booted := time.Now()
		cancel, done := runLoop(duty.WokenLoop{
			Wake:   make(chan struct{}, 1),
			MinGap: 5 * time.Second,
			Pass:   func(context.Context) time.Time { passes.record(); return time.Time{} },
		}, noLease(t))
		time.Sleep(time.Hour)
		stop(cancel, done)
		wantPassesAt(t, &passes, booted, 0)
	})
}

// Idle is called each time the loop is about to wait with nothing owed and nothing due, and not
// while a pass's due time is armed: here the boot pass asks to run again 10 s on, so the loop is
// first idle after that pass, and again after the pass a later wake earns.
func TestAWokenLoopReportsIdleOnlyWithNothingOwedOrDue(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		var passes, idles passClock
		wake := make(chan struct{}, 1)
		booted := time.Now()
		first := true
		cancel, done := runLoop(duty.WokenLoop{
			Wake:   wake,
			MinGap: 5 * time.Second,
			Pass: func(context.Context) time.Time {
				passes.record()
				if first {
					first = false
					return booted.Add(10 * time.Second)
				}
				return time.Time{}
			},
			Idle: idles.record,
		}, noLease(t))
		time.Sleep(time.Minute)
		signal(wake)
		time.Sleep(time.Minute)
		stop(cancel, done)
		wantPassesAt(t, &passes, booted, 0, 10*time.Second, time.Minute)
		var reported []time.Duration
		for _, at := range idles.times() {
			reported = append(reported, at.Sub(booted))
		}
		if want := []time.Duration{10 * time.Second, time.Minute}; !slices.Equal(reported, want) {
			t.Fatalf("idle reported at %v after boot, want %v", reported, want)
		}
	})
}

// Wakes that arrive inside MinGap of a pass's start collapse into one pass at the end of the gap,
// so a loop under load runs no more often than the timer it replaced; a wake after the gap runs at
// once.
func TestWakesInsideTheMinimumGapCollapseIntoOnePass(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var passes passClock
		wake := make(chan struct{}, 1)
		booted := time.Now()
		cancel, done := runLoop(duty.WokenLoop{
			Wake:   wake,
			MinGap: 5 * time.Second,
			Pass:   func(context.Context) time.Time { passes.record(); return time.Time{} },
		}, noLease(t))
		for range 3 {
			time.Sleep(time.Second)
			signal(wake)
		}
		time.Sleep(17 * time.Second)
		wantPassesAt(t, &passes, booted, 0, 5*time.Second)

		signal(wake)
		synctest.Wait()
		stop(cancel, done)
		wantPassesAt(t, &passes, booted, 0, 5*time.Second, 20*time.Second)
	})
}

// A change that lands while a pass runs may have been read too early by it, so its wake earns one
// more pass after the gap.
func TestAWakeDuringAPassEarnsAPassAfterIt(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var passes passClock
		wake := make(chan struct{}, 1)
		inside, release := make(chan struct{}), make(chan struct{})
		first := true
		booted := time.Now()
		cancel, done := runLoop(duty.WokenLoop{
			Wake:   wake,
			MinGap: 5 * time.Second,
			Pass: func(context.Context) time.Time {
				passes.record()
				if first {
					first = false
					close(inside)
					<-release
				}
				return time.Time{}
			},
		}, noLease(t))
		<-inside
		signal(wake)
		close(release)
		time.Sleep(time.Minute)
		stop(cancel, done)
		wantPassesAt(t, &passes, booted, 0, 5*time.Second)
	})
}

// A due time a pass returns fires one pass, and only one: the next pass returns none.
func TestADueTimeAPassReturnsFiresOnce(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var passes passClock
		calls := 0
		booted := time.Now()
		cancel, done := runLoop(duty.WokenLoop{
			MinGap: 5 * time.Second,
			Pass: func(context.Context) time.Time {
				passes.record()
				calls++
				if calls == 1 {
					return time.Now().Add(30 * time.Second)
				}
				return time.Time{}
			},
		}, noLease(t))
		time.Sleep(time.Hour)
		stop(cancel, done)
		wantPassesAt(t, &passes, booted, 0, 30*time.Second)
	})
}

// Each pass's due time replaces the one before: a pass woken before the first due time that
// returns a later one moves the timer there, and nothing fires at the first.
func TestALaterPassesDueTimeReplacesTheEarlierOne(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var passes passClock
		wake := make(chan struct{}, 1)
		calls := 0
		booted := time.Now()
		cancel, done := runLoop(duty.WokenLoop{
			Wake:   wake,
			MinGap: time.Second,
			Pass: func(context.Context) time.Time {
				passes.record()
				calls++
				switch calls {
				case 1:
					return time.Now().Add(30 * time.Second)
				case 2:
					return time.Now().Add(time.Minute)
				}
				return time.Time{}
			},
		}, noLease(t))
		time.Sleep(10 * time.Second)
		signal(wake)
		time.Sleep(2 * time.Hour)
		stop(cancel, done)
		wantPassesAt(t, &passes, booted, 0, 10*time.Second, 70*time.Second)
	})
}

// A due time sooner than MinGap after the pass that returned it waits out the gap: a pass asking
// to run again at once cannot make the loop poll faster than its gap.
func TestADueTimeWaitsOutTheMinimumGap(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var passes passClock
		calls := 0
		booted := time.Now()
		cancel, done := runLoop(duty.WokenLoop{
			MinGap: 5 * time.Second,
			Pass: func(context.Context) time.Time {
				passes.record()
				calls++
				if calls < 3 {
					return time.Now().Add(time.Second)
				}
				return time.Time{}
			},
		}, noLease(t))
		time.Sleep(time.Hour)
		stop(cancel, done)
		wantPassesAt(t, &passes, booted, 0, 5*time.Second, 10*time.Second)
	})
}

// With no MinGap a leased loop runs at most one pass at a time and owes at most one: five wakes
// during a pass of one second earn one pass, which starts as that pass ends, and a wake after both
// passes runs a pass at once. A lease no other process holds makes no retry, so Retry, ten seconds
// here, holds nothing, and a leased loop with no MinGap runs.
func TestWakesDuringAPassMakeOneOwedPassAtItsEnd(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var passes passClock
		wake := make(chan struct{}, 1)
		lease := &fakeLease{}
		booted := time.Now()
		cancel, done := runLoop(duty.WokenLoop{
			Lease: "eval-days",
			Wake:  wake,
			Retry: 10 * time.Second,
			Pass: func(context.Context) time.Time {
				passes.record()
				time.Sleep(time.Second)
				return time.Time{}
			},
		}, lease.exclusive)
		for range 5 {
			time.Sleep(150 * time.Millisecond)
			signal(wake)
		}
		time.Sleep(2500*time.Millisecond - time.Since(booted))
		signal(wake)
		time.Sleep(time.Hour)
		stop(cancel, done)
		wantPassesAt(t, &passes, booted, 0, time.Second, 2500*time.Millisecond)
	})
}

// A pass sees a wake that arrives while it runs (duty.WakeOwed) without taking it: the first pass
// reads no wake owed at its start and one owed after the wake, and the loop still runs the pass that
// wake earns, which reads none owed. A context no loop gave its wake reads none.
func TestAPassSeesAWakeOwedWithoutTakingIt(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var passes passClock
		wake := make(chan struct{}, 1)
		var before, after []bool
		booted := time.Now()
		cancel, done := runLoop(duty.WokenLoop{
			Wake: wake,
			Pass: func(ctx context.Context) time.Time {
				passes.record()
				before = append(before, duty.WakeOwed(ctx))
				if len(before) == 1 {
					time.Sleep(time.Second)
				}
				after = append(after, duty.WakeOwed(ctx))
				return time.Time{}
			},
		}, noLease(t))
		time.Sleep(500 * time.Millisecond)
		signal(wake)
		time.Sleep(time.Hour)
		stop(cancel, done)
		wantPassesAt(t, &passes, booted, 0, time.Second)
		if want := []bool{false, false}; !slices.Equal(before, want) {
			t.Errorf("the passes read a wake owed at their start %v, want %v", before, want)
		}
		if want := []bool{true, false}; !slices.Equal(after, want) {
			t.Errorf("the passes read a wake owed at their end %v, want %v: the first after the wake, the second after none", after, want)
		}
		if duty.WakeOwed(context.Background()) {
			t.Error("a context no loop gave its wake reads a wake owed")
		}
	})
}

// A Backoff's waits begin at First and double up to Max, and Reset begins them again at First:
// 250 ms doubling to the 10 s the refresher and the Legion facts consumer retry a held-back event
// by at most.
func TestABackoffDoublesFromItsFirstWaitToItsMaxUntilReset(t *testing.T) {
	b := duty.Backoff{First: 250 * time.Millisecond, Max: 10 * time.Second}
	want := []time.Duration{250 * time.Millisecond, 500 * time.Millisecond, time.Second, 2 * time.Second,
		4 * time.Second, 8 * time.Second, 10 * time.Second, 10 * time.Second}
	got := make([]time.Duration, len(want))
	for i := range got {
		got[i] = b.Next()
	}
	if !slices.Equal(got, want) {
		t.Errorf("the waits are %v, want %v", got, want)
	}
	b.Reset()
	if first := b.Next(); first != want[0] {
		t.Errorf("after Reset the wait is %v, want %v", first, want[0])
	}
}

// fakeLease is Exclusive's shape over a lease the test controls: it refuses the first refusals
// attempts, as a lease held by another process (or one that could not be taken) does, and runs the
// pass on every attempt after. It records each attempt's time, name, due time and period.
type fakeLease struct {
	mu       sync.Mutex
	refusals int
	attempts []time.Time
	names    []string
	dues     []time.Time
	periods  []time.Duration
}

func (f *fakeLease) exclusive(ctx context.Context, _ *slog.Logger, _ duty.Leases, name string, due time.Time, period time.Duration, pass func(context.Context)) bool {
	f.mu.Lock()
	f.attempts = append(f.attempts, time.Now())
	f.names = append(f.names, name)
	f.dues = append(f.dues, due)
	f.periods = append(f.periods, period)
	refused := len(f.attempts) <= f.refusals
	f.mu.Unlock()
	if refused {
		return false
	}
	pass(ctx)
	return true
}

// A pass refused its lease is retried every Retry, plus up to a tenth again, until a pass of its
// own runs, with no wake: the process holding the lease may have read before the change that woke
// this loop. Once it has run, the loop is idle again. Every attempt goes through the lease named
// for the loop, with no period and no due time.
func TestARefusedLeaseIsRetriedUntilAPassOfItsOwnRuns(t *testing.T) {
	const retry = 2 * time.Second
	fixJitter(t, func(n time.Duration) time.Duration { return n - 1 })
	synctest.Test(t, func(t *testing.T) {
		var passes passClock
		lease := &fakeLease{refusals: 3}
		booted := time.Now()
		cancel, done := runLoop(duty.WokenLoop{
			Lease: "suggestion-outcomes",
			Wake:  make(chan struct{}, 1),
			Retry: retry,
			Pass:  func(context.Context) time.Time { passes.record(); return time.Time{} },
		}, lease.exclusive)
		time.Sleep(time.Hour)
		stop(cancel, done)

		lease.mu.Lock()
		defer lease.mu.Unlock()
		if len(lease.attempts) != 4 {
			t.Fatalf("attempts = %d, want 3 refused and the one that ran", len(lease.attempts))
		}
		for i := 1; i < len(lease.attempts); i++ {
			if gap := lease.attempts[i].Sub(lease.attempts[i-1]); gap < retry || gap >= retry+retry/10 {
				t.Errorf("retry %d came %v after the refusal, want within [%v, %v)", i, gap, retry, retry+retry/10)
			}
		}
		for i := range lease.attempts {
			if lease.names[i] != "suggestion-outcomes" || lease.periods[i] != 0 || !lease.dues[i].IsZero() {
				t.Errorf("attempt %d took lease %q with period %v due %v, want suggestion-outcomes with neither", i+1, lease.names[i], lease.periods[i], lease.dues[i])
			}
		}
		wantPassesAt(t, &passes, booted, lease.attempts[3].Sub(booted))
	})
}

// Cancelling the context returns the loop at once from every wait: idle, a retry's wait, a gap's
// wait. A pass shutdown cut short reports it did not run, and the loop returns rather than
// retrying it.
func TestAWokenLoopReturnsAtOnceWhenItsContextEnds(t *testing.T) {
	returnsAtOnce := func(t *testing.T, cancel context.CancelFunc, done <-chan struct{}) {
		t.Helper()
		synctest.Wait()
		cancel()
		synctest.Wait()
		select {
		case <-done:
		default:
			t.Fatal("the loop had not returned once its context ended")
		}
	}
	idlePass := func(context.Context) time.Time { return time.Time{} }

	t.Run("idle", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			cancel, done := runLoop(duty.WokenLoop{Wake: make(chan struct{}, 1), Pass: idlePass}, noLease(t))
			returnsAtOnce(t, cancel, done)
		})
	})
	t.Run("waiting to retry a refused lease", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			lease := &fakeLease{refusals: 1 << 30}
			cancel, done := runLoop(duty.WokenLoop{Lease: "l", Retry: time.Hour, Pass: idlePass}, lease.exclusive)
			returnsAtOnce(t, cancel, done)
		})
	})
	t.Run("waiting out the gap", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			wake := make(chan struct{}, 1)
			cancel, done := runLoop(duty.WokenLoop{Wake: wake, MinGap: time.Hour, Pass: idlePass}, noLease(t))
			synctest.Wait()
			signal(wake)
			returnsAtOnce(t, cancel, done)
		})
	})
	t.Run("a pass cut short", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			attempts := 0
			cutShort := func(context.Context, *slog.Logger, duty.Leases, string, time.Time, time.Duration, func(context.Context)) bool {
				attempts++
				cancel()
				return false
			}
			loop := duty.WokenLoop{Lease: "l", Retry: time.Second, Pass: idlePass}
			loop.Run(ctx, cutShort)
			if attempts != 1 {
				t.Errorf("attempts = %d, want the one shutdown cut short", attempts)
			}
		})
	})
}

// Against Postgres: a lease another session holds keeps the pass from running, and the retry runs
// it once that session lets go, with no wake in between.
func TestRunWokenRetriesALeaseHeldElsewhereUntilThePassRuns(t *testing.T) {
	database := storetest.Open(t)
	leases := leasesOf(t, database)
	ctx := context.Background()
	const name = "reference-excerpts"

	holder, err := database.Pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire the other process's connection: %v", err)
	}
	defer holder.Release()
	var locked bool
	if err := holder.QueryRow(ctx, duty.TakeLease, duty.LeaseNamespace, name).Scan(&locked); err != nil || !locked {
		t.Fatalf("hold the lease: %v (%t)", err, locked)
	}

	ran := make(chan struct{}, 8)
	loopCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		duty.RunWoken(loopCtx, duty.WokenLoop{
			Lease:  name,
			Leases: leases,
			Wake:   make(chan struct{}, 1),
			Retry:  50 * time.Millisecond,
			Pass:   func(context.Context) time.Time { ran <- struct{}{}; return time.Time{} },
		})
	}()
	defer func() {
		cancel()
		<-done
	}()

	select {
	case <-ran:
		t.Fatal("the pass ran while another session held its lease")
	case <-time.After(500 * time.Millisecond):
	}
	var freed bool
	if err := holder.QueryRow(ctx, duty.FreeLease, duty.LeaseNamespace, name).Scan(&freed); err != nil || !freed {
		t.Fatalf("let the lease go: %v (%t)", err, freed)
	}
	select {
	case <-ran:
	case <-time.After(5 * time.Second):
		t.Fatal("the pass did not run within 5s of the lease being let go")
	}
}
