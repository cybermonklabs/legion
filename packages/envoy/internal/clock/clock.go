// Package clock is the time Envoy's code reads and waits on.
//
// # One clock per module
//
// Code whose timers, waits or deadlines a test asserts takes a Clock, through a field that
// defaults to Real, and a test hands it a clocktest.Fake. There is one Clock and one fake for the
// whole module: a package that needs only some of Clock's methods declares a narrowing of it, an
// interface of its own whose methods have Clock's signatures, so Real and the fake fit every one
// and no package keeps a clock or a fake of its own. The rule this serves is R4 of "The design of
// this repository's tests" in docs/testing.md: no test passes or fails because it read a clock.
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
	"math"
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

// unreported is how far off WithShownDeadline shows a deadline that ctx does not report: the
// farthest a time.Duration reaches, which no call meets.
const unreported = time.Duration(math.MaxInt64)

// WithShownDeadline is ctx as a call that refuses a context with no deadline is to see it: ctx
// itself when ctx reports a deadline, and otherwise ctx under a deadline unreported off on the
// wall clock, with the function that releases that deadline. nats.go refuses a flush whose
// context reports no deadline (nats.ErrNoDeadlineContext), and bounds a JetStream call on such a
// context by a wait of its own on the wall clock. Two contexts report none though they end: one
// that a clocktest.Fake's WithTimeout or WithDeadline made, which keeps its deadline to itself, and
// a test's testwait.Context under -timeout 0, which has no deadline. Each is shown one that bounds
// nothing, and its own end is what ends the call. The real clock's contexts report their deadline,
// so production never takes the second branch.
func WithShownDeadline(ctx context.Context) (context.Context, context.CancelFunc) {
	if _, reported := ctx.Deadline(); reported {
		return ctx, func() {}
	}
	return context.WithDeadline(ctx, time.Now().Add(unreported))
}

// Timeouts is the part of a Clock that WithShownTimeout reads. Real, a clocktest.Fake and every
// package's narrowing of Clock that keeps WithTimeout fit it.
type Timeouts interface {
	WithTimeout(ctx context.Context, d time.Duration) (context.Context, context.CancelFunc)
}

// WithShownTimeout is c's WithTimeout of ctx and d as WithShownDeadline shows it: a context that
// ends once d has passed on c and reports a deadline a call such as nats.go's can read, with one
// function that releases both. On the real clock it is c's own context, which reports its
// deadline; on a clocktest.Fake it ends only where the test moves the fake past d.
func WithShownTimeout(c Timeouts, ctx context.Context, d time.Duration) (context.Context, context.CancelFunc) {
	clocked, cancel := c.WithTimeout(ctx, d)
	shown, cancelShown := WithShownDeadline(clocked)
	return shown, func() {
		cancelShown()
		cancel()
	}
}
