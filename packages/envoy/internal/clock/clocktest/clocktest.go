// Package clocktest is Envoy's one fake clock: a clock.Clock that moves only when the test
// moves it, and that tells the test what the code has armed on it.
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
// it fires the next timer, so what a timer's function did is done when Advance returns. The move
// waits through the wait the tests share (testwait.ReceiveInHelper): a function that never returns,
// one that waits for the test to act after the move among them, holds the move until that wait
// gives up, 30 s before the test binary's deadline, and the move then panics with the wait's text,
// which names the timer's deadline and the move's target. A move whose timer's function waits for
// the test to act is made off the test goroutine through StartMove, whose join, on the test
// goroutine, is that move's one bound and names the function it waits for. A channel timer (After,
// NewTimer, Sleep) is fired by a non-blocking send on its channel, whose buffer holds one.
//
// A timer armed for a moment the fake has already reached, a wait of zero or less, fires as it is
// armed, as the real clock's does, and is never pending: an AfterFunc's function starts on a
// goroutine of its own that the arming call does not wait for, a channel timer's send is made at
// once, a Sleep returns at once and a deadline context is ended before it is returned. Such a
// channel timer sends the moment it was armed, the fake's present, and not the moment it was
// armed for, as Go's does: Go arms a wait of zero or less for the moment it is armed (time.when).
// That holds for a timer a function armed while a move fires it too, so the move does not wait
// for such a timer's function.
//
// # What is armed
//
// Armed is the deadlines pending on the fake, in order: every timer armed and neither fired nor
// stopped, a Sleep in progress and a deadline context not yet ended among them. AwaitArmed waits for
// a count of them, AwaitArmedAt for one deadline among them and AwaitArmedCountAt for a count of
// timers at one deadline, through testwait, as clockwork's
// BlockUntilContext does, so a test moves the fake past a deadline only once the code has armed it,
// and a failure names what the test expected, the deadlines it saw and the fake's moment.
//
// SetWhen reads the fake's present and its pending deadlines, asks a question of them and makes
// the move the answer names, under one hold of the fake's lock. A test that reads Armed and then
// calls Set leaves a gap between the two: a timer the code arms in it is pending when the move
// begins, though the reading that decided the move never saw it. SetWhen leaves no such gap: a
// timer the code arms while it runs is armed after the question, at the moment the move reached.
//
// # Deadline contexts
//
// WithTimeout, WithTimeoutCause and WithDeadline give a context that ends once the fake reaches its
// deadline with context.DeadlineExceeded as its error, and as its cause or the one WithTimeoutCause
// names, as a context derived from it does too; a cancel that runs first ends it with
// context.Canceled as its error and cause, as the context package's own do. Its
// Deadline reports the parent's and never the fake's own: a fake deadline that reached pgx, nats.go
// or a net.Dialer would be compared there with the wall clock, and end the call at a moment the test
// never chose. Code that reads ctx.Deadline() for its own logic takes the deadline from its clock
// instead.
//
// # Another source's moments
//
// WithNow is a clock whose waits run on one clock and whose Now reads another source, for code
// whose stamps must stand on the moments a test sets while its own pauses run on a clock the test
// does not move.
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
	// held is whether Now reads the wall clock plus added (Held) rather than now (New), and wall
	// is that wall clock: time.Now, or the one a test of this package names (HeldOn).
	held  bool
	wall  func() time.Time
	now   time.Time
	added time.Duration
	// stepped is every step StepWall has made, which Now reads on top of the fake's moment and the
	// timers do not.
	stepped time.Duration
	// arms counts every arm, so the timers due together fire in the order they were armed.
	arms    uint64
	pending []*timer
}

var _ clock.Clock = (*Fake)(nil)

// New is a fake whose Now is start until the test moves it.
func New(start time.Time) *Fake { return &Fake{now: start} }

// Held is a fake whose Now is the wall clock plus what Advance and Set have added.
func Held() *Fake { return &Fake{held: true, wall: time.Now} }

// WithNow is c with the moment it reads taken from now: every timer, sleep and deadline context
// runs on c, and Now answers now's moment. It is for code whose waits must run on c while the
// moments it stamps or judges are another source's, such as a writer whose backoff after a lost
// compare-and-swap is a pause of its own, on a clock nothing has to move, while the moments it
// stamps stand on a fake the test moves: on that fake, the pause would end only when the test
// moved it.
func WithNow(c clock.Clock, now func() time.Time) clock.Clock {
	return nowFrom{Clock: c, now: now}
}

// nowFrom is WithNow's clock: c's waits, with Now read from now.
type nowFrom struct {
	clock.Clock
	now func() time.Time
}

func (c nowFrom) Now() time.Time { return c.now() }

// Now is the fake's moment plus every step StepWall has made.
func (c *Fake) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.nowLocked().Add(c.stepped)
}

// StepWall steps the wall clock that Now reads by d, forward or back, as an operator or NTP steps
// a machine's clock, and fires no timer. The timers, Armed, AwaitArmed, AwaitArmedAt, Advance, Set
// and SetWhen stay on the fake's own moment, as Go's timers run on the monotonic clock whatever the
// wall clock does. What a channel timer sends carries the steps, as what Go's sends does: Go sends
// the moment a timer fell due read on the wall clock as it sends (time.sendTime, Now less the
// lateness), so a channel timer here sends its deadline plus every step made; one armed for a
// moment the fake has already reached sends the moment it was armed plus every step. A fake that
// never steps is a fake whose Now is its moment, and whose channel timers send their deadlines, or
// their arm moments.
//
// What the fake cannot model: a Go time.Time that time.Now returns carries a monotonic reading
// beside its wall one, and Go compares and subtracts two such times by the monotonic one, so code
// that compares or subtracts its own readings of the clock never sees a step of the wall. A time
// from the fake has one reading, so after a step such code sees it. A loop whose grid compares its
// own readings of Now (clock.Ticker) shifts that grid by a step on the fake, where production's
// grid does not move.
func (c *Fake) StepWall(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.stepped += d
}

func (c *Fake) nowLocked() time.Time {
	if c.held {
		return c.wall().Add(c.added)
	}
	return c.now
}

func (c *Fake) setLocked(at time.Time) {
	if c.held {
		c.added = at.Sub(c.wall())
		return
	}
	c.now = at
}

// Advance moves the fake d forward, firing every timer due by then. A fake moves back only by Set:
// the move reads the fake's present and begins under one hold of its lock, and another move that
// took the fake past the target while a timer fired leaves it there.
func (c *Fake) Advance(d time.Duration) {
	if d < 0 {
		panic(fmt.Sprintf("clocktest: Advance(%v): a fake moves back only by Set", d))
	}
	c.mu.Lock()
	c.moveLocked(c.nowLocked().Add(d), false, sharedWait)
}

// Set moves the fake to at, earlier or later, firing every timer due by then. A timer keeps the
// deadline it was armed for, so a move back fires none that a later move had not reached.
func (c *Fake) Set(at time.Time) { c.moveTo(at) }

// SetWhen moves the fake as Set does to the moment to names, when to reports true. to is asked
// of the fake's present and of every pending deadline, earliest first (Armed), under the hold of
// the fake's lock that the move begins with, so no timer is armed, fired or stopped between the
// question and the move's start. to runs under that lock, so it must not call the fake, nor wait
// for anything that does. SetWhen returns the deadlines to was asked of and whether it moved.
func (c *Fake) SetWhen(to func(now time.Time, armed []time.Time) (time.Time, bool)) ([]time.Time, bool) {
	c.mu.Lock()
	armed := c.armedLocked()
	target, move := to(c.nowLocked(), armed)
	if !move {
		c.mu.Unlock()
		return armed, false
	}
	c.moveLocked(target, true, sharedWait)
	return armed, true
}

func (c *Fake) moveTo(target time.Time) {
	c.mu.Lock()
	c.moveLocked(target, true, sharedWait)
}

// moveLocked is moveTo begun under the hold of the lock its caller took: each later pass takes
// the lock again (the loop's post statement), and the last lets it go. back is whether the move
// may end before the fake's present, as a Set may; a move that may not ends at target only when
// target is ahead, since another move can take the fake past it while a timer of this one fires.
// wait waits for done, which closes once the AfterFunc's function what names has returned: a move
// on the test goroutine passes sharedWait, and the move StartMove started passes its waitFor.
func (c *Fake) moveLocked(target time.Time, back bool, wait func(what string, done <-chan struct{})) {
	for ; ; c.mu.Lock() {
		next := c.dueLocked(target)
		if next == nil {
			if back || target.After(c.nowLocked()) {
				c.setLocked(target)
			}
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
			case next.c <- next.at.Add(c.stepped):
			default:
			}
			c.mu.Unlock()
			continue
		}
		// The deadline is read under the lock: the function may Reset its own timer once it runs.
		f, own, due := next.f, next.own, next.at
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
		what := fmt.Sprintf("the function of the AfterFunc due at %s, which a move to %s fired, to return",
			due.Format(time.RFC3339Nano), target.Format(time.RFC3339Nano))
		wait(what, done)
	}
}

// sharedWait waits for done, which closes once the function what names has returned, through the
// wait the tests share (testwait.ReceiveInHelper), and panics with that wait's text at its give-up.
func sharedWait(what string, done <-chan struct{}) {
	if _, err := testwait.ReceiveInHelper("clocktest", what, done); err != nil {
		panic(err)
	}
}

// StartMove moves the fake to target on a goroutine of its own, firing every timer due by then as
// Advance does, and never back: another move that took the fake past target while a timer of this
// one fired leaves it there. It returns join, which waits on the test goroutine for that move to
// return. It is for a move that fires a timer whose function waits on what the test holds, or on
// what the test does after the move began, for which a move on the test goroutine would wait for
// ever.
//
// The started move waits for each timer's function with no bound of its own, and join's wait
// (testwait.Until) is its one bound: a function that never returns fails t there, on the test
// goroutine, with a text that names what, the move, and the function the move waits for, by its
// timer's deadline and the move's target. A move made on the test goroutine waits for each
// function through testwait.ReceiveInHelper instead and panics at its give-up, whose moment, read
// from the process's start, is microseconds before the test's own waits give up, too close to
// order a give-up on another goroutine against them. A started move nobody joins waits for its
// function for ever, on its own goroutine.
//
// Once the move has returned, each call of join returns at once, so a row may join in its body and
// again in a cleanup that runs when the body ended before its join. Call join on the test
// goroutine only.
func (c *Fake) StartMove(what string, target time.Time) (join func(t testing.TB)) {
	started := &startedMove{returned: make(chan struct{})}
	go func() {
		defer close(started.returned)
		c.mu.Lock()
		c.moveLocked(target, false, started.waitFor)
	}()
	return func(t testing.TB) {
		t.Helper()
		testwait.Until(t, what+" to return", func() (string, bool) {
			select {
			case <-started.returned:
				return "", true
			default:
				return started.reading(), false
			}
		})
	}
}

// startedMove is a move StartMove started, as its join reads it: returned closes once the move has
// returned, and waiting is the wait for a timer's function the move is in, empty between two.
type startedMove struct {
	returned chan struct{}

	mu      sync.Mutex
	waiting string
}

// waitFor waits for done, which closes once the function what names has returned, with no bound of
// its own: the join's wait on the test goroutine is the one bound, and reads what while the move
// waits here.
func (m *startedMove) waitFor(what string, done <-chan struct{}) {
	m.mu.Lock()
	m.waiting = what
	m.mu.Unlock()
	<-done
	m.mu.Lock()
	m.waiting = ""
	m.mu.Unlock()
}

// reading is the join's last reading of the move: the wait for a timer's function it is in.
func (m *startedMove) reading() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.waiting == "" {
		return "the move waits for no timer's function"
	}
	return "the move waits for " + m.waiting
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
	defer c.mu.Unlock()
	return c.armedLocked()
}

func (c *Fake) armedLocked() []time.Time {
	pending := make([]*timer, len(c.pending))
	for i, t := range c.pending {
		// A copy, so the sort below leaves the fake's own list as it stands.
		pending[i] = &timer{at: t.at, seq: t.seq}
	}
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
// earliest first. what names what the test waits to see armed; a failure prints it with n and what
// the last try read (armedReading).
func (c *Fake) AwaitArmed(t testing.TB, what string, n int) []time.Time {
	t.Helper()
	var armed []time.Time
	testwait.Until(t, fmt.Sprintf("%s (at least %d timers armed on the fake clock)", what, n), func() (string, bool) {
		pending, reading := c.armedReading()
		if len(pending) < n {
			return reading, false
		}
		armed = pending
		return reading, true
	})
	return armed
}

// AwaitArmedAt waits until a timer due at at is pending on the fake, and returns the deadline of
// every timer pending then, earliest first: the reading that held at. It looks for the one deadline
// rather than a count, since the code's other timers on the fake may be pending beside it. what
// names what the test waits to see armed; a failure prints it with at and what the last try read
// (armedReading).
func (c *Fake) AwaitArmedAt(t testing.TB, what string, at time.Time) []time.Time {
	t.Helper()
	var held []time.Time
	testwait.Until(t, what+" armed for "+at.Format(time.RFC3339Nano), func() (string, bool) {
		armed, reading := c.armedReading()
		if !slices.ContainsFunc(armed, at.Equal) {
			return reading, false
		}
		held = armed
		return reading, true
	})
	return held
}

// AwaitArmedCountAt waits until at least n timers due at at are pending on the fake, and returns
// the deadline of every timer pending then, earliest first: the reading that held them. It is for a
// row that moves the fake to a deadline several timers share, one for each socket the code opened,
// only once every one of them has armed it, so none is armed after the move and comes due at a
// moment the row does not drive. what names what the test waits to see armed; a failure prints it
// with at, n and what the last try read (armedReading).
func (c *Fake) AwaitArmedCountAt(t testing.TB, what string, at time.Time, n int) []time.Time {
	t.Helper()
	var held []time.Time
	testwait.Until(t, fmt.Sprintf("%s armed %d times for %s", what, n, at.Format(time.RFC3339Nano)), func() (string, bool) {
		armed, reading := c.armedReading()
		if count := len(slices.DeleteFunc(slices.Clone(armed), func(deadline time.Time) bool { return !deadline.Equal(at) })); count < n {
			return reading, false
		}
		held = armed
		return reading, true
	})
	return held
}

// armedReading is one reading of what is armed, for the waits on it: the deadline of every pending
// timer, earliest first, and what a wait that gives up prints of them, read under one hold of the
// lock: no timer armed, or how many are and their deadlines, and the fake's moment then.
func (c *Fake) armedReading() ([]time.Time, string) {
	c.mu.Lock()
	now, armed := c.nowLocked(), c.armedLocked()
	c.mu.Unlock()
	if len(armed) == 0 {
		return armed, fmt.Sprintf("no timer armed, the fake at %v", now)
	}
	return armed, fmt.Sprintf("%d timers armed, due at %v, the fake at %v", len(armed), armed, now)
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
	return c.withDeadline(ctx, func(now time.Time) time.Time { return now.Add(d) }, context.DeadlineExceeded)
}

// WithTimeoutCause is WithTimeout whose end at its deadline carries cause, or
// context.DeadlineExceeded when cause is nil, as context.WithTimeoutCause's does.
func (c *Fake) WithTimeoutCause(ctx context.Context, d time.Duration, cause error) (context.Context, context.CancelFunc) {
	return c.withDeadline(ctx, func(now time.Time) time.Time { return now.Add(d) }, cmp.Or(cause, context.DeadlineExceeded))
}

func (c *Fake) WithDeadline(ctx context.Context, at time.Time) (context.Context, context.CancelFunc) {
	return c.withDeadline(ctx, func(time.Time) time.Time { return at }, context.DeadlineExceeded)
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
// clock fires a wait of zero or less: a channel timer's send, of the fake's present plus the steps,
// the moment it was armed, is made here, under the lock, and the function it answers, which the
// caller runs once it has let the lock go, starts an AfterFunc's function on a goroutine of its
// own, not waited for, or ends a deadline context.
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
		case t.c <- c.nowLocked().Add(c.stepped):
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

// withDeadline is a deadline context on the fake at the moment due names, which ends there with
// context.DeadlineExceeded as its error and cause as its cause.
func (c *Fake) withDeadline(parent context.Context, due func(now time.Time) time.Time, cause error) (context.Context, context.CancelFunc) {
	inner, cancelInner := context.WithCancelCause(parent)
	ctx := &deadlineContext{Context: inner, cancelInner: cancelInner, done: make(chan struct{})}
	ctx.timer = &timer{fake: c, f: func() { ctx.end(context.DeadlineExceeded, cause) }}
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
