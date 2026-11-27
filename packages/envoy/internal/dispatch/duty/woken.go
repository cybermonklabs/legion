package duty

import (
	"context"
	"time"
)

// WokenLoop is one change-driven background loop: the work a timer loop would redo every period
// whether or not anything changed, run instead when something says it changed. RunWoken runs it.
type WokenLoop struct {
	// Lease, when set, names the lease each pass runs under (Exclusive, with no period), so two
	// processes woken by one change do not both do its work at once. Empty runs every pass
	// without one.
	Lease string
	// Leases is the pool a leased pass takes its lease connection from (Exclusive); unused when
	// Lease is empty.
	Leases Leases
	// Wake is signalled when the work may have changed: a pglisten.Hub wake, whose buffer of one
	// and non-blocking send coalesce any number of changes into one signal. A signal that
	// arrives while a pass runs stays in that buffer and earns one pass after it, so a change the
	// running pass read too early is still served, and any number of changes during a pass earn
	// that one pass; the pass can see the signal waiting (WakeOwed). Nil is a loop that runs only
	// at start and when its passes ask to.
	Wake <-chan struct{}
	// MinGap is the least time from the start of one pass to the start of the next, whatever woke
	// it: wakes that arrive sooner collapse into the one pass at the end of the gap, so a loop under
	// load runs no more often than once per MinGap - the period of the timer it replaces - and an
	// idle one not at all. Zero holds no pass back: at most one pass runs and at most one is owed,
	// and the owed pass starts as soon as the running one ends.
	MinGap time.Duration
	// Retry is how long a leased loop waits, plus up to a tenth again, before trying once more
	// after an attempt whose pass did not run: the lease was held by another process, or could not
	// be taken. It must be positive when Lease is set, and a loop with no lease never reads it. The
	// retry is what keeps a wake from being lost to the other process's pass, which may have read
	// before the change that woke this one; the loop keeps trying until a pass of its own runs.
	Retry time.Duration
	// Pass does the work, on the loop's goroutine, and returns when the loop next needs to run
	// with no wake - an attempt's retry time, a claim's expiry - or the zero time for never.
	// Each pass's answer replaces the last one's, and a due time is held to MinGap like a wake.
	Pass func(context.Context) (next time.Time)
	// Idle, when set, is called on the loop's own goroutine each time it is about to wait with
	// nothing owed and nothing due: it holds no timer and will make no statement until a wake
	// arrives or ctx ends. That is the state this loop shape exists for, and a test that must
	// know the loop has reached it waits on this rather than on a window of its own. Production
	// leaves it nil.
	Idle func()
}

// RunWoken runs loop until ctx is cancelled: one pass at once, then a pass on each wake and at each
// due time a pass returned, never sooner than loop.MinGap after the start of the pass before it.
// Between those it holds no timer and makes no statement, so a loop with nothing to do costs
// nothing - the point of the shape, against RunJittered's pass every period whatever changed.
//
// A leased loop runs each pass under Exclusive with no period and no due time (the zero time),
// which is mutual exclusion and nothing else: no period is read or recorded, so no tick can be
// judged into a period another pass served. The lease name passes through Exclusive like every
// other loop's. An attempt Exclusive reports did not run - the lease held elsewhere, or a failure
// to take it - is retried every loop.Retry until one runs: the other process's pass may have read
// before the change, and a failure must neither spin nor drop the wake. A pass shutdown cut short
// also reports it did not run, and the loop returns then rather than retrying.
func RunWoken(ctx context.Context, loop WokenLoop) {
	loop.run(ctx, Exclusive)
}

// wakeKey is the key a pass's context carries its loop's wake under (WithWake).
type wakeKey struct{}

// WithWake is ctx carrying wake as the wake of the loop whose pass runs under it, which WakeOwed
// reads: what a WokenLoop gives each of its passes, and what a test of a pass gives it.
func WithWake(ctx context.Context, wake <-chan struct{}) context.Context {
	return context.WithValue(ctx, wakeKey{}, wake)
}

// WakeOwed reports, without blocking and without taking it, whether a signal waits in the wake of
// the loop whose pass runs under ctx: a change arrived that the loop runs another pass for as soon
// as this one returns. A pass may stop work it can leave to a later pass, so that the owed pass,
// and the change behind the wake, start sooner. It answers false under a context no loop gave its
// wake, and for a loop with none.
func WakeOwed(ctx context.Context) bool {
	wake, _ := ctx.Value(wakeKey{}).(<-chan struct{})
	return len(wake) > 0
}

// MergeWakes is the one wake a WokenLoop reads, signalled by any of wakes: a loop woken by more
// than one channel (legionfacts.Run). It keeps the shape a pglisten.Hub
// wake has: a buffer of one and a send that never blocks, so any number of changes collapse into
// one signal and a change that arrives while a pass runs earns a pass after it. A nil wake is left out,
// and only nil wakes answer nil, which is a loop that runs at start and at each due time alone.
// The goroutines that forward the wakes end when ctx is done.
func MergeWakes(ctx context.Context, wakes ...<-chan struct{}) <-chan struct{} {
	live := make([]<-chan struct{}, 0, len(wakes))
	for _, wake := range wakes {
		if wake != nil {
			live = append(live, wake)
		}
	}
	switch len(live) {
	case 0:
		return nil
	case 1:
		return live[0]
	}
	merged := make(chan struct{}, 1)
	for _, wake := range live {
		go func() {
			for {
				select {
				case <-ctx.Done():
					return
				case <-wake:
					select {
					case merged <- struct{}{}:
					default:
					}
				}
			}
		}()
	}
	return merged
}

func (l WokenLoop) run(ctx context.Context, exclusive ExclusiveFunc) {
	if l.Lease != "" && l.Retry <= 0 {
		panic("duty: a leased WokenLoop needs a positive Retry")
	}
	var (
		// owed is whether a pass is owed regardless of any due time: the boot pass, a wake, or an
		// attempt whose pass did not run.
		owed = true
		// due is when the last pass asked to run again; zero for never.
		due time.Time
		// notBefore is the earliest the next attempt may start: MinGap after the start of the
		// last pass, pushed later by a held attempt's retry wait.
		notBefore time.Time
		// The one timer the loop owns, re-armed rather than replaced, and the time it is armed
		// for (zero when it is not armed).
		timer *time.Timer
		armed time.Time
	)
	defer func() {
		if timer != nil {
			timer.Stop()
		}
	}()
	for {
		var fire <-chan time.Time
		if at, ok := nextAttempt(owed, due, notBefore); ok {
			switch {
			case timer == nil:
				timer = time.NewTimer(time.Until(at))
			case !at.Equal(armed):
				timer.Reset(time.Until(at))
			}
			armed, fire = at, timer.C
		} else {
			if !armed.IsZero() {
				timer.Stop()
				armed = time.Time{}
			}
			if l.Idle != nil {
				l.Idle()
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-l.Wake:
			owed = true
			continue
		case <-fire:
			armed = time.Time{}
		}
		if ctx.Err() != nil {
			return
		}
		started := time.Now()
		next, ran := l.attempt(ctx, exclusive)
		if ran {
			owed, due = false, next
			notBefore = started.Add(l.MinGap)
			continue
		}
		if ctx.Err() != nil {
			return
		}
		owed = true
		notBefore = later(notBefore, time.Now().Add(Jittered(l.Retry)))
	}
}

// attempt runs one pass, under the lease when the loop has one, with the loop's wake on its
// context (WakeOwed), and reports whether it ran and the due time it returned.
func (l WokenLoop) attempt(ctx context.Context, exclusive ExclusiveFunc) (next time.Time, ran bool) {
	ctx = WithWake(ctx, l.Wake)
	if l.Lease == "" {
		return l.Pass(ctx), true
	}
	ran = exclusive(ctx, nil, l.Leases, l.Lease, time.Time{}, 0, func(ctx context.Context) {
		next = l.Pass(ctx)
	})
	return next, ran
}

// nextAttempt is when the loop's next attempt falls, and false when nothing is owed or due: an
// owed pass at the earliest the gap allows, a due one at its time or the gap's end if that is
// later.
func nextAttempt(owed bool, due, notBefore time.Time) (time.Time, bool) {
	switch {
	case owed:
		return notBefore, true
	case !due.IsZero():
		return later(due, notBefore), true
	}
	return time.Time{}, false
}

func later(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}

// Backoff is a retry's wait that begins at First and doubles each time it is taken, up to Max,
// until it is Reset: the retry of a pass that met something whose end no wake announces
// (legionfacts.Run's pass whose safe horizon held an event back),
// so a brief one is retried soon after it ends and a long one costs one retry per Max once the
// waits have grown to it. A new Backoff's first Next is First.
type Backoff struct {
	First, Max time.Duration
	wait       time.Duration
}

// Next is the wait before the next retry: First after a Reset, then twice the wait before it,
// never more than Max.
func (b *Backoff) Next() time.Duration {
	b.wait = min(max(2*b.wait, b.First), b.Max)
	return b.wait
}

// Reset begins the waits again at First.
func (b *Backoff) Reset() {
	b.wait = 0
}
