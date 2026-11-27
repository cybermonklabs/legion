// Package clock is the time Envoy's code reads and waits on.
//
// # One clock for the module's new seams
//
// Code whose timers, waits or deadlines a test asserts takes a Clock, through a field that
// defaults to Real, and a test hands it a clocktest.Fake. A package that needs only some of
// Clock's methods declares a narrowing of it, an interface of its own whose methods have Clock's
// signatures, so Real and the fake fit every one. Some packages still keep a clock of their own
// from before this one (the delivery hub's hubClock, the delivery jobs' jobClock); a new seam
// takes this one, so no test passes or fails because it read a clock.
//
// # The fake tells the test when a timer is armed
//
// A clocktest.Fake moves only when the test moves it, and it reports the deadlines the code has
// armed on it (Armed). A test therefore moves time past a deadline only once the code has armed
// it, and a test that waits for an arm that never comes fails naming the deadline it expected and
// the ones it saw, never a moment of the machine's own clock.
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
	// WithTimeoutCause is WithTimeout whose end at d carries cause (context.Cause), as
	// context.WithTimeoutCause's does: its Err is context.DeadlineExceeded at d, with cause as its
	// cause, or context.DeadlineExceeded when cause is nil, and context.Canceled, as error and
	// cause both, once its cancel runs first. A d of zero or less ends it before it is returned.
	WithTimeoutCause(ctx context.Context, d time.Duration, cause error) (context.Context, context.CancelFunc)
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

func (realClock) WithTimeoutCause(ctx context.Context, d time.Duration, cause error) (context.Context, context.CancelFunc) {
	return context.WithTimeoutCause(ctx, d, cause)
}

func (realClock) WithDeadline(ctx context.Context, at time.Time) (context.Context, context.CancelFunc) {
	return context.WithDeadline(ctx, at)
}

// realTimer is a time.Timer as a Timer.
type realTimer struct{ timer *time.Timer }

func (t realTimer) C() <-chan time.Time { return t.timer.C }

func (t realTimer) Stop() bool { return t.timer.Stop() }

func (t realTimer) Reset(d time.Duration) bool { return t.timer.Reset(d) }
