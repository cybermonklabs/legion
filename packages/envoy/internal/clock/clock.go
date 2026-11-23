// Package clock is the time a package's code reads and waits on where a test must move that time
// itself: Real in production, a clocktest.Fake in a test, through a field that defaults to Real.
// The cipher helper's own timers run on it (helper.Broker.Clock, helper.Server.Clock): its expiry
// watch, refresh loop and retry ladder, a register's wait, the first confirming renew's bound, the
// renew cadence and the dormant sweep. A package that needs only some of Clock's methods can
// declare an interface of its own with those methods' signatures, which Real and the fake both fit.
//
// # The fake tells the test when a timer is armed
//
// A clocktest.Fake moves only when the test moves it, and it reports the deadlines the code has
// armed on it (Armed, AwaitArmed). A test therefore moves time past a deadline only once the code
// has armed it, and a test that waits for an arm that never comes fails naming the deadline it
// expected and the ones it saw, never a moment of the machine's own clock.
//
// # testing/synctest or the fake
//
// Code with no real network I/O is tested under testing/synctest, whose bubble fakes the time
// package itself and whose synctest.Wait says when every goroutine in it is blocked; such code may
// call the time package directly. Code over a real PostgreSQL or NATS cannot run in a bubble: a
// goroutine blocked on a socket is not durably blocked, so the bubble's clock would never move. Such
// code takes a Clock, and its test drives a clocktest.Fake.
package clock

import (
	"context"
	"time"
)

// Clock is what time it is, and every way to wait for a later one.
type Clock interface {
	Now() time.Time
	// After is a channel that receives the time once d has passed.
	After(d time.Duration) <-chan time.Time
	// NewTimer is a Timer whose channel receives the time once d has passed.
	NewTimer(d time.Duration) Timer
	// AfterFunc calls f on a goroutine of its own once d has passed. Its Timer's C is nil.
	AfterFunc(d time.Duration, f func()) Timer
	// Sleep waits d, or returns ctx's error once ctx ends first.
	Sleep(ctx context.Context, d time.Duration) error
	// WithTimeout is a context that ends with context.DeadlineExceeded once d has passed.
	WithTimeout(ctx context.Context, d time.Duration) (context.Context, context.CancelFunc)
	// WithDeadline is a context that ends with context.DeadlineExceeded at at.
	WithDeadline(ctx context.Context, at time.Time) (context.Context, context.CancelFunc)
}

// Timer is one armed wait, as time.Timer is.
type Timer interface {
	// C is the channel a timer from NewTimer sends on; nil for one from AfterFunc.
	C() <-chan time.Time
	// Stop disarms the timer, and reports whether this call stopped it before it fired.
	Stop() bool
	// Reset arms the timer again for d from now, and reports whether it was armed.
	Reset(d time.Duration) bool
}

// Real is the wall clock: the time package's own timers and contexts.
func Real() Clock { return realClock{} }

type realClock struct{}

func (realClock) Now() time.Time { return time.Now() }

func (realClock) After(d time.Duration) <-chan time.Time { return time.After(d) }

func (realClock) NewTimer(d time.Duration) Timer { return realTimer{time.NewTimer(d)} }

func (realClock) AfterFunc(d time.Duration, f func()) Timer { return realTimer{time.AfterFunc(d, f)} }

func (realClock) Sleep(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (realClock) WithTimeout(ctx context.Context, d time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, d)
}

func (realClock) WithDeadline(ctx context.Context, at time.Time) (context.Context, context.CancelFunc) {
	return context.WithDeadline(ctx, at)
}

// realTimer is a time.Timer as a Timer.
type realTimer struct{ timer *time.Timer }

func (t realTimer) C() <-chan time.Time { return t.timer.C }

func (t realTimer) Stop() bool { return t.timer.Stop() }

func (t realTimer) Reset(d time.Duration) bool { return t.timer.Reset(d) }
