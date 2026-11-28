// Package duty holds the shapes Dispatch's background loops run in, and the lease that keeps two
// processes from doing one loop's work twice. RunJittered is the timer loop, for work that has no
// change to wake on - the GitHub-App importers (architecture.Run, delivery.Reconcile.Run), since
// GitHub has no feed of what they would miss: a jittered start and a jittered wait after every pass,
// so a restart does not stampede GitHub and two processes' ticks do not fall into step, and an idle
// check so a caller with no App credentials configured skips the loop entirely rather than tick
// forever doing nothing. RunLeased is the loop of every other leased maintenance loop of a fixed
// cadence, and RunLeasedAfterEachPass the loop of those paced by their own passes. RunWoken is the
// change-driven loop, for work a Postgres notification can announce: one pass at start, then a pass
// only when woken or when the last pass asked for one, so an idle database costs it nothing.
// Exclusive is the lease and period gate a pass of any of them may run through.
package duty

import (
	"context"
	"log/slog"
	"math/rand/v2"
	"time"

	"github.com/sjawhar/envoy/internal/clock"
)

// jitterN draws a duration in [0, n), and zero for an n of zero or less. Every jittered wait in this
// package draws from it, and it is a variable only so a test can fix the draw.
var jitterN = func(n time.Duration) time.Duration {
	if n <= 0 {
		return 0
	}
	// #nosec G404 — jitter, not a secret.
	return rand.N(n)
}

// Jittered is d plus up to a tenth of d again, drawn afresh each call, so the waits of processes
// that booted together drift apart. The jitter only ever adds, so a jittered wait is never shorter
// than the d it was asked for. RunWoken relies on that: a leased loop whose attempt did not run
// waits Jittered(MinGap) before it tries again, so no retry comes sooner than its least gap. A loop
// that must pass again within its interval waits JitteredWithin instead.
func Jittered(d time.Duration) time.Duration {
	return d + jitterN(d/10)
}

// JitteredWithin is d less up to a tenth of d, drawn afresh each call: a wait in (d - d/10, d], so
// a loop that waits it after each pass passes again within d of the pass before, and the waits of
// processes that booted together drift apart. For a d under 10ns a tenth of it is zero, and the
// wait is d.
func JitteredWithin(d time.Duration) time.Duration {
	return d - jitterN(d/10)
}

// LeasePeriod is the period a leased loop paced by JitteredWithin(d) hands Exclusive: d less a
// tenth of it. Every wait JitteredWithin(d) leaves is longer than it (for a d under 10ns both are
// d), and two due times at least one period apart always fall in two of Exclusive's epoch-aligned
// buckets, so no tick of a lone loop is refused by the period its own previous pass served.
func LeasePeriod(d time.Duration) time.Duration {
	return d - d/10
}

// RunJittered calls pass about every interval until ctx is cancelled, starting after a jittered
// delay in [0, interval) so a restart does not stampede whatever pass calls. Each wait after a
// pass is the interval less up to a tenth of it (JitteredWithin), drawn afresh each time, so two
// processes that booted with nearby phases drift apart rather than ticking in step for the life of
// both, and no wait is longer than the interval: a gap between passes is more than 0.9 of an
// interval and at most one, plus the pass's own run time. idle is checked once, before the first
// tick: when it returns true the loop never starts (the caller is expected to have logged why
// inside idle itself).
//
// pass receives the moment its tick was due, the time its timer delivered, which is what a leased
// pass hands Exclusive. Each timer is armed once the pass before it has returned and waits longer
// than LeasePeriod(interval), so a caller that leases its pass hands Exclusive that period, and not
// the interval: two due times are then more than one period apart, and a lone loop's tick is never
// refused by the period its own previous pass recorded, however late either one's lease came. With
// the interval as the period, a tick that came less than an interval after the one before would be
// refused whenever both fell in one bucket, and the next pass would come almost two intervals after
// the last.
func RunJittered(ctx context.Context, interval time.Duration, idle func() bool, pass func(ctx context.Context, due time.Time)) {
	if idle() {
		return
	}
	wait := jitterN(interval)
	for {
		timer := time.NewTimer(wait)
		var due time.Time
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case due = <-timer.C:
		}
		pass(ctx, due)
		wait = JitteredWithin(interval)
	}
}

// FirstTick is when RunLeased makes a loop's first tick.
type FirstTick int

const (
	// TickAtStart makes the first tick the moment the loop starts.
	TickAtStart FirstTick = iota
	// TickAfterInterval makes the first tick one interval after the loop starts.
	TickAfterInterval
)

// RunLeased runs the fixed-cadence loop of RunLeasedOn on clock.Real() with the default logger.
// Its clock.Ticker delivers ticks as a time.Ticker does, with a tick at start if first is
// TickAtStart.
func RunLeased(ctx context.Context, leases Leases, name string, interval time.Duration, first FirstTick, pass func(context.Context)) {
	RunLeasedOn(ctx, clock.Real(), nil, leases, name, interval, first, pass)
}

// RunLeasedOn runs pass under the lease named name, once per tick of a clock.Ticker on c until
// ctx ends. Each tick is one Exclusive call with logger (nil for the default logger), so pass
// runs only when this process takes the lease and no process has already served the tick's period,
// which is always interval. The lease calls themselves run on clock.Real().
//
// first selects a tick at start or one interval after start. Each tick hands Exclusive the
// moment the ticker delivered, not the moment the loop took the tick. On clock.Real(), ticks
// come as a time.Ticker's do: a step of the wall clock shifts the delivered due times without
// moving the ticker's monotonic grid.
//
// Without a wall-clock step, a lone loop that keeps up with the ticker gets due times one
// interval apart and serves consecutive periods (periodBoundary). Waiting a full interval
// after each pass instead would add each pass's run time to the cadence, eventually skipping
// a period. When a pass overruns, the first tick it overran comes at once with its original
// due time, and any later ticks already due are dropped instead of replayed.
//
// Cancellation ends a wait at once. If a tick is in progress, the loop returns once its
// Exclusive call, including the pass and lease cleanup, returns.
func RunLeasedOn(ctx context.Context, c clock.Clock, logger *slog.Logger, leases Leases, name string, interval time.Duration, first FirstTick, pass func(context.Context)) {
	runLeased(ctx, c, logger, leases, name, interval, first, Exclusive, pass)
}

// runLeased takes ticks from a clock.Ticker on c and arms the next tick only after the supplied
// gate returns. A schedule test can supply its own gate and run without Postgres, including
// inside a synctest bubble with clock.Real().
func runLeased(ctx context.Context, c clock.Clock, logger *slog.Logger, leases Leases, name string, interval time.Duration, first FirstTick, exclusive ExclusiveFunc, pass func(context.Context)) {
	ticks := clock.NewTicker(c, interval, first == TickAtStart)
	defer ticks.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case due := <-ticks.C():
			if ctx.Err() != nil {
				return
			}
			ticks.Took()
			exclusive(ctx, logger, leases, name, due, interval, pass)
			ticks.Next()
		}
	}
}

// RunLeasedAfterEachPass runs the pass-paced loop of RunLeasedAfterEachPassOn on clock.Real()
// with the default logger.
func RunLeasedAfterEachPass(ctx context.Context, leases Leases, name string, interval time.Duration, pass func(context.Context, time.Duration) time.Duration) {
	RunLeasedAfterEachPassOn(ctx, clock.Real(), nil, leases, name, interval, pass)
}

// RunLeasedAfterEachPassOn runs pass under the lease named name, starting at c.Now(). Each tick
// calls Exclusive with logger (nil for the default logger). The lease calls run on clock.Real().
// Once Exclusive returns, a timer on c waits the positive duration that pass returned, or
// interval if the pass did not run. For example, block-index reconciliation asks for a
// two-second wait after a full batch.
//
// Exclusive gets the wait before the tick as its period (interval for the first tick), and
// pass receives that same period. The first due is the start reading; later dues are the times
// the timers deliver. Without a wall-clock step, consecutive dues are at least the later tick's
// period apart, so a lone loop's previous pass does not consume the next tick's period
// (periodBoundary). A slow pass delays the next tick by its run time; it does not cut the wait
// short.
//
// Cancellation stops the timer if waiting, or ends the loop once the Exclusive call in
// progress returns. A pass cut short by cancellation still returns a wait, which goes unused.
func RunLeasedAfterEachPassOn(ctx context.Context, c clock.Clock, logger *slog.Logger, leases Leases, name string, interval time.Duration, pass func(context.Context, time.Duration) time.Duration) {
	period := interval
	due := c.Now()
	var timer clock.Timer
	for ctx.Err() == nil {
		wait := interval
		Exclusive(ctx, logger, leases, name, due, period, func(ctx context.Context) {
			wait = pass(ctx, period)
		})
		period = wait
		if ctx.Err() != nil {
			return
		}
		if timer == nil {
			timer = c.NewTimer(wait)
		} else {
			timer.Reset(wait)
		}
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case due = <-timer.C():
		}
	}
}
