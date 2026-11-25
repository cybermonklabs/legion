package clock

import "time"

// Ticker is a time.Ticker on a Clock, for a loop a test must be able to read: its next tick is
// armed only once the loop's work for the tick before it is done, so a test that sees that tick
// armed on a clocktest.Fake knows the work before it has returned. It is the one ticker of the
// module's loops that keep a fixed cadence on a Clock.
//
// Its ticks fall on a grid one interval apart from the moment NewTicker read the clock, and each
// hands the loop the moment it fired, as a time.Ticker hands the value it sent: Go sends the moment
// a tick fell due, read on the wall clock as it sends (time.sendTime), however late its receiver
// takes it, and a clocktest.Fake sends the timer's deadline plus every step of its wall
// (StepWall). So every process on one grid puts a tick in the period it fired in, however late it
// took it. On Real the ticks come as a time.Ticker's do; on a clocktest.Fake each comes when the
// test moves the fake to its grid moment.
//
// A loop takes each tick from C, calls Took as it takes it, does the tick's work, and calls Next
// once that work is done:
//
//	for {
//		select {
//		case <-ctx.Done():
//			return
//		case fired := <-ticks.C():
//			ticks.Took()
//			work(fired)
//			ticks.Next()
//		}
//	}
//
// Took drops the grid moments the tick waited past for its receiver, as a time.Ticker's channel,
// full while its receiver is away, drops them: the next tick is the first grid moment after the
// moment the loop took this one. Next arms that tick, or, when it fell due while the work ran,
// hands it at once with the moment it fell due, so it serves the period it fell in: the first tick
// a slow loop overran comes at once and only the ones after it are dropped, as Go's runtime gives a
// ticker's slow receiver (runtime/time.go). That tick comes through no timer, since a timer armed
// for a moment already past sends the moment it was armed (time.when), which is the moment the
// loop took it again. It comes on a channel of its own, so a loop reads C afresh for each tick, as
// the select above does.
//
// The grid compares the clock's own readings. A time Real reads carries a monotonic reading, which
// Go compares and subtracts by, so a step of the wall clock does not move the grid; a time from a
// clocktest.Fake carries none, so a step there shifts it (clocktest.Fake.StepWall).
type Ticker struct {
	clock    Clock
	interval time.Duration
	// due is the grid moment of the tick in flight: armed, sent, or handed late. Took moves it to
	// the tick after the one taken.
	due time.Time
	// timer brings each tick armed for a moment ahead; nil until Next first arms one, for a ticker
	// whose first tick came at its start.
	timer Timer
	// late holds a tick that fell due while the loop's work ran, and c is the channel the next
	// tick comes on: timer's, or late.
	late chan time.Time
	c    <-chan time.Time
}

// NewTicker starts a Ticker on c whose grid begins at the moment it reads c and whose ticks are
// interval apart: its first tick is one interval in, or, when atStart, at once, with that moment,
// and the one after it one interval in. interval must be positive, as time.NewTicker's must.
func NewTicker(c Clock, interval time.Duration, atStart bool) *Ticker {
	if interval <= 0 {
		panic("clock: NewTicker of a non-positive interval")
	}
	t := &Ticker{clock: c, interval: interval, late: make(chan time.Time, 1)}
	start := c.Now()
	if atStart {
		t.due = start
		t.late <- start
		t.c = t.late
		return t
	}
	t.due = start.Add(interval)
	t.timer = c.NewTimer(interval)
	t.c = t.timer.C()
	return t
}

// C is the channel the next tick comes on, which sends the moment the tick fired. Read it afresh
// for each tick: a tick that fell due while the loop worked comes on a channel of its own (Next).
func (t *Ticker) C() <-chan time.Time { return t.c }

// Took marks the tick the loop has just received from C as taken, and returns the moment it was
// taken, read on the ticker's clock: the next tick is the first grid moment after it.
func (t *Ticker) Took() time.Time {
	taken := t.clock.Now()
	t.due = t.due.Add(t.interval)
	for !t.due.After(taken) {
		t.due = t.due.Add(t.interval)
	}
	return taken
}

// Next arms the tick after the one the loop took, once the loop's work for that one is done. A
// tick that fell due while the work ran is not armed: it is on C at once, with the moment it fell
// due, the clock read now less the time since (now plus a wait of zero or less, which Real
// measures on the monotonic reading, as Go's runtime measures the lateness it subtracts).
func (t *Ticker) Next() {
	now := t.clock.Now()
	wait := t.due.Sub(now)
	if wait <= 0 {
		t.late <- now.Add(wait)
		t.c = t.late
		return
	}
	if t.timer == nil {
		t.timer = t.clock.NewTimer(wait)
	} else {
		t.timer.Reset(wait)
	}
	t.c = t.timer.C()
}

// Stop stops the ticker: no receive from C after it returns reads a tick.
func (t *Ticker) Stop() {
	if t.timer != nil {
		t.timer.Stop()
	}
	select {
	case <-t.late:
	default:
	}
}
