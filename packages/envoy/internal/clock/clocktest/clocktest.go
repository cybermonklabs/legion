// Package clocktest is Envoy's one fake clock: a clock.Clock that moves only when the test
// moves it, and that tells the test what the code has armed on it.
// packages/daemon/internal/clock/clocktest and packages/envoy/internal/clock/clocktest hold the same
// code, because Go's internal rule keeps one module from importing the other's (packages/envoy/infra
// imports Envoy's through its replace), so a change to one is made to the other.
//
// # Two fakes
//
// New is a fake whose Now stands at the moment it was given until the test moves it. Held is a
// fake whose Now reads the wall clock plus what the test has added, for code that stamps times a
// database compares with its own now(), or reads back. Either moves only by Advance or Set, and no
// timer of either fires by itself: a fake that is never moved is a clock whose timers never fire.
//
// # Moving it
//
// Advance and Set fire every timer due by the moment they move the fake to, in deadline order, the
// timers due together in the order they were armed. The fake reads each timer's deadline as that
// timer fires, so a timer which the firing of another arms inside the span fires in the same move.
// An AfterFunc's function runs on a goroutine of its own, and the move waits for it to return before
// it fires the next timer, so what a timer's function did is done when Advance returns; a function
// that waits for the test to act after the move holds the move for ever. A channel timer (After,
// NewTimer, Sleep) is fired by a non-blocking send on its channel, whose buffer holds one.
//
// A timer armed for a moment the fake has already reached, a wait of zero or less, fires as it is
// armed, as the real clock's does, and is never pending: an AfterFunc's function starts on a
// goroutine of its own that the arming call does not wait for, a channel timer's send is made at
// once, a Sleep returns at once and a deadline context is ended before it is returned. That holds
// for a timer a function armed while a move fires it too, so the move does not wait for such a
// timer's function.
//
// # What is armed
//
// Armed is the deadlines pending on the fake, in order: every timer armed and neither fired nor
// stopped, a Sleep in progress and a deadline context not yet ended among them. AwaitArmed waits for
// them through testwait, as clockwork's BlockUntilContext does, so a test moves the fake past a
// deadline only once the code has armed it, and a failure names what the test expected and the
// deadlines it saw.
//
// # Deadline contexts
//
// WithTimeout and WithDeadline give a context that ends once the fake reaches its deadline, with
// context.DeadlineExceeded as its error and its cause, as a context derived from it does too. Its
// Deadline reports the parent's and never the fake's own: a fake deadline that reached pgx, nats.go
// or a net.Dialer would be compared there with the wall clock, and end the call at a moment the test
// never chose. Code that reads ctx.Deadline() for its own logic takes the deadline from its clock
// instead.
package clocktest

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/clock"
	"github.com/sjawhar/envoy/internal/testwait"
)

// Fake is a clock.Clock that moves only by Advance and Set. Its zero value is not ready; New and
// Held make one.
type Fake struct {
	mu sync.Mutex
	// held is whether Now reads the wall clock plus added (Held) rather than now (New).
	held  bool
	now   time.Time
	added time.Duration
	// arms counts every arm, so the timers due together fire in the order they were armed.
	arms    uint64
	pending []*timer
}

var _ clock.Clock = (*Fake)(nil)

// New is a fake whose Now is start until the test moves it.
func New(start time.Time) *Fake { return &Fake{now: start} }

// Held is a fake whose Now is the wall clock plus what Advance and Set have added.
func Held() *Fake { return &Fake{held: true} }

func (c *Fake) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.nowLocked()
}

func (c *Fake) nowLocked() time.Time {
	if c.held {
		return time.Now().Add(c.added)
	}
	return c.now
}

func (c *Fake) setLocked(at time.Time) {
	if c.held {
		c.added = at.Sub(time.Now())
		return
	}
	c.now = at
}

// Advance moves the fake d forward, firing every timer due by then. A fake moves back only by Set.
func (c *Fake) Advance(d time.Duration) {
	if d < 0 {
		panic(fmt.Sprintf("clocktest: Advance(%v): a fake moves back only by Set", d))
	}
	c.mu.Lock()
	target := c.nowLocked().Add(d)
	c.mu.Unlock()
	c.moveTo(target)
}

// Set moves the fake to at, earlier or later, firing every timer due by then. A timer keeps the
// deadline it was armed for, so a move back fires none that a later move had not reached.
func (c *Fake) Set(at time.Time) { c.moveTo(at) }

func (c *Fake) moveTo(target time.Time) {
	for {
		c.mu.Lock()
		next := c.dueLocked(target)
		if next == nil {
			c.setLocked(target)
			c.mu.Unlock()
			return
		}
		c.disarmLocked(next)
		if next.at.After(c.nowLocked()) {
			c.setLocked(next.at)
		}
		if next.c != nil {
			// Under the lock, so a Stop or Reset that returned before it never leaves this value
			// behind for a later receive.
			select {
			case next.c <- next.at:
			default:
			}
			c.mu.Unlock()
			continue
		}
		f, own := next.f, next.own
		c.mu.Unlock()
		if !own {
			f()
			continue
		}
		done := make(chan struct{})
		go func() {
			defer close(done)
			f()
		}()
		<-done
	}
}

// dueLocked is the pending timer due earliest by target, the earliest armed among those due
// together, or nil.
func (c *Fake) dueLocked(target time.Time) *timer {
	var next *timer
	for _, t := range c.pending {
		if t.at.After(target) {
			continue
		}
		if next == nil || t.at.Before(next.at) || (t.at.Equal(next.at) && t.seq < next.seq) {
			next = t
		}
	}
	return next
}

// Armed is the deadline of every pending timer, earliest first.
func (c *Fake) Armed() []time.Time {
	c.mu.Lock()
	pending := make([]*timer, len(c.pending))
	for i, t := range c.pending {
		// A copy, so the sort below reads no field a Reset writes.
		pending[i] = &timer{at: t.at, seq: t.seq}
	}
	c.mu.Unlock()
	slices.SortFunc(pending, func(a, b *timer) int {
		if order := a.at.Compare(b.at); order != 0 {
			return order
		}
		return cmp.Compare(a.seq, b.seq)
	})
	deadlines := make([]time.Time, len(pending))
	for i, t := range pending {
		deadlines[i] = t.at
	}
	return deadlines
}

// AwaitArmed waits until at least n timers are pending on the fake and returns their deadlines,
// earliest first. what names what the test waits to see armed; a failure prints it with the
// deadlines last seen pending.
func (c *Fake) AwaitArmed(t testing.TB, what string, n int) []time.Time {
	t.Helper()
	return testwait.Until(t, fmt.Sprintf("%s (at least %d timers armed on the fake clock)", what, n), func() ([]time.Time, bool) {
		armed := c.Armed()
		return armed, len(armed) >= n
	})
}

func (c *Fake) After(d time.Duration) <-chan time.Time { return c.NewTimer(d).C() }

func (c *Fake) NewTimer(d time.Duration) clock.Timer {
	t := &timer{fake: c, c: make(chan time.Time, 1)}
	c.arm(t, func(now time.Time) time.Time { return now.Add(d) })
	return t
}

func (c *Fake) AfterFunc(d time.Duration, f func()) clock.Timer {
	t := &timer{fake: c, f: f, own: true}
	c.arm(t, func(now time.Time) time.Time { return now.Add(d) })
	return t
}

// Sleep waits until the fake reaches d from now, or returns ctx's error once ctx ends first. A wait
// of zero or less returns at once.
func (c *Fake) Sleep(ctx context.Context, d time.Duration) error {
	t := c.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C():
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (c *Fake) WithTimeout(ctx context.Context, d time.Duration) (context.Context, context.CancelFunc) {
	return c.withDeadline(ctx, func(now time.Time) time.Time { return now.Add(d) })
}

func (c *Fake) WithDeadline(ctx context.Context, at time.Time) (context.Context, context.CancelFunc) {
	return c.withDeadline(ctx, func(time.Time) time.Time { return at })
}

// arm places t at the deadline due gives for the fake's present, read under the same hold of the
// lock, so no move lands between the two.
func (c *Fake) arm(t *timer, due func(now time.Time) time.Time) {
	c.mu.Lock()
	fire := c.placeLocked(t, due(c.nowLocked()))
	c.mu.Unlock()
	fire()
}

// placeLocked arms t for at, or, when the fake has already reached at, fires it at once as the real
// clock fires a wait of zero or less: a channel timer's send is made here, under the lock, and the
// function it answers, which the caller runs once it has let the lock go, starts an AfterFunc's
// function on a goroutine of its own, not waited for, or ends a deadline context.
func (c *Fake) placeLocked(t *timer, at time.Time) func() {
	c.arms++
	t.at, t.seq = at, c.arms
	if at.After(c.nowLocked()) {
		t.armed = true
		c.pending = append(c.pending, t)
		return func() {}
	}
	t.armed = false
	switch {
	case t.c != nil:
		select {
		case t.c <- at:
		default:
		}
		return func() {}
	case t.own:
		return func() { go t.f() }
	default:
		return t.f
	}
}

func (c *Fake) disarmLocked(t *timer) {
	t.armed = false
	if i := slices.Index(c.pending, t); i >= 0 {
		c.pending = slices.Delete(c.pending, i, i+1)
	}
}

// timer is one arm on a Fake: a channel timer when c is set, otherwise f, run on a goroutine of
// its own when own is set (AfterFunc) and inline when not (a deadline context's end).
type timer struct {
	fake  *Fake
	at    time.Time
	seq   uint64
	armed bool
	c     chan time.Time
	f     func()
	own   bool
}

func (t *timer) C() <-chan time.Time { return t.c }

func (t *timer) Stop() bool {
	c := t.fake
	c.mu.Lock()
	defer c.mu.Unlock()
	was := t.armed
	if was {
		c.disarmLocked(t)
	}
	t.drainLocked()
	return was
}

func (t *timer) Reset(d time.Duration) bool {
	c := t.fake
	c.mu.Lock()
	was := t.armed
	if was {
		c.disarmLocked(t)
	}
	t.drainLocked()
	at := c.nowLocked().Add(d)
	fire := c.placeLocked(t, at)
	c.mu.Unlock()
	fire()
	return was
}

// drainLocked takes a value the timer sent and nobody received, so no receive after a Stop or a
// Reset reads a firing from before it, as time.Timer promises since Go 1.23.
func (t *timer) drainLocked() {
	if t.c == nil {
		return
	}
	select {
	case <-t.c:
	default:
	}
}

func (c *Fake) withDeadline(parent context.Context, due func(now time.Time) time.Time) (context.Context, context.CancelFunc) {
	inner, cancelInner := context.WithCancelCause(parent)
	ctx := &deadlineContext{Context: inner, cancelInner: cancelInner, done: make(chan struct{})}
	ctx.timer = &timer{fake: c, f: func() { ctx.end(context.DeadlineExceeded, context.DeadlineExceeded) }}
	cancel := func() { ctx.end(context.Canceled, context.Canceled) }
	// A parent already ended ends the context with its own error, before a deadline already
	// reached could, as context.WithDeadline orders the two.
	if err := inner.Err(); err != nil {
		ctx.end(err, context.Cause(inner))
		return ctx, cancel
	}
	c.arm(ctx.timer, due)
	context.AfterFunc(inner, func() { ctx.end(inner.Err(), context.Cause(inner)) })
	return ctx, cancel
}

// deadlineContext is a context that ends at a deadline on a Fake. inner, the embedded context,
// answers Deadline (its parent's), Value, and so the cause context.Cause reads; Done and Err are
// its own, so a context derived from it, which cannot take it for one of the context package's
// own, is ended by the context package's watch of Done with the error Err gives.
//
// A parent's end cancels inner at once, and Done and Err read inner before they answer, so a
// caller that looks after the parent's cancel returned sees this context ended too, as it would
// a context.WithDeadline's: the end context.AfterFunc runs comes on a goroutine of its own, and
// a publish that read Err before that goroutine ran would send what its cancel meant to stop.
type deadlineContext struct {
	context.Context
	cancelInner context.CancelCauseFunc
	timer       *timer
	done        chan struct{}

	mu     sync.Mutex
	ending bool
	err    error
}

func (ctx *deadlineContext) Done() <-chan struct{} {
	ctx.endWithParent()
	return ctx.done
}

func (ctx *deadlineContext) Err() error {
	ctx.endWithParent()
	ctx.mu.Lock()
	defer ctx.mu.Unlock()
	return ctx.err
}

// endWithParent ends the context with its parent's error once the parent has ended.
func (ctx *deadlineContext) endWithParent() {
	if err := ctx.Context.Err(); err != nil {
		ctx.end(err, context.Cause(ctx.Context))
	}
}

// end ends the context once, with err as its error and cause as its cause: the cause is set on
// inner before Done closes, so whoever sees Done closed reads both. A second end waits for the
// first to close Done, so an Err that ended the context itself never answers before it is ended.
func (ctx *deadlineContext) end(err, cause error) {
	ctx.mu.Lock()
	if ctx.ending {
		ctx.mu.Unlock()
		<-ctx.done
		return
	}
	ctx.ending = true
	ctx.mu.Unlock()
	ctx.timer.Stop()
	ctx.cancelInner(cause)
	ctx.mu.Lock()
	ctx.err = err
	close(ctx.done)
	ctx.mu.Unlock()
}
