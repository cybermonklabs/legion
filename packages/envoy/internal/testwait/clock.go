package testwait

import (
	"fmt"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// clock is the time the waits here read and arm their timer by: the wall clock, until a child test
// binary gives them a clock it moves itself (stepClock). testwait's own tests give theirs one from
// the start (useStepClock); a child another package's row runs takes one once its setup has
// reported done (SetUpDone). A wait's margin then passes at a step the child takes, never at a
// moment a loaded machine moves, and the child's verdict follows from what its waits did, not from
// how long the machine took to do it.
var clock interface {
	// Now is the time now.
	Now() time.Time
	// Timer returns a channel that receives once at has come, and the function that releases it.
	Timer(at time.Time) (<-chan time.Time, func() bool)
	// wall is the wall clock's moment at which at comes.
	wall(at time.Time) time.Time
} = wallClock{}

// wallClock is the time as the machine keeps it.
type wallClock struct{}

func (wallClock) Now() time.Time { return time.Now() }

func (wallClock) Timer(at time.Time) (<-chan time.Time, func() bool) {
	timer := time.NewTimer(time.Until(at))
	return timer.C, timer.Stop
}

func (wallClock) wall(at time.Time) time.Time { return at }

// Now is the time on the clock the waits of this binary read: the wall clock's, or, in a child that
// took SetUpDone, its step clock's, which stands at the waits' give-up and moves only at the
// child's own steps. A fixture that keeps a span or a deadline of its own reads its start here
// (Within, WallTime).
func Now() time.Time { return clock.Now() }

// WallTime is the wall clock's moment at which at, a moment on the clock the waits read (Now),
// comes: at itself on the wall clock, and on a child's step clock, which does not follow the wall
// clock, the wall clock's now plus what is left until at on the step clock, or now once at has come
// there. It is for a deadline only the kernel keeps, such as a socket's, which a fixture takes from
// the waits' clock.
func WallTime(at time.Time) time.Time { return clock.wall(at) }

// Stepped reports whether the waits of this binary read a step clock (SetUpDone), on which nothing
// the machine does moves the time. A fixture whose span another library keeps for it on the wall
// clock, such as nats.go's bound on a request, keeps that span on the waits' clock instead
// (Within) while they read one.
func Stepped() bool {
	_, stepped := clock.(*stepClock)
	return stepped
}

// Within waits for one receive from ch for d on the clock the waits read, and returns what it
// received and whether a value came. It is no wait of a test's own, which takes no span (Receive):
// it is for a span a fixture keeps of its own, a hang guard on a call or a stand-in's latency, so
// that on a child's step clock (SetUpDone) the span passes on that clock, in its moment's order
// among the timers armed there, as on the wall clock it passes on the wall clock's. A span that
// would end past the test binary's deadline on a step clock ends the binary there, as go test's
// alarm would.
func Within[T any](ch <-chan T, d time.Duration) (T, bool) {
	expired, stop := clock.Timer(clock.Now().Add(d))
	defer stop()
	return received(ch, expired)
}

// Pause holds its caller for d on the clock the waits read (Within): the latency of a stand-in that
// answers late on purpose.
func Pause(d time.Duration) { Within[struct{}](nil, d) }

// stepClock is the clock a child test binary gives the waits here (clock). Its time moves only when
// the child sets it (set), or when a timer armed on it fires, which moves it to the timer's moment
// when that is later. A timer fires only once a wait is blocked waiting on a timer and no timer
// still pending is due before it (fireOnceWaited): timers fire in the order of their moments, as
// the wall clock's do, so a stand-in that answers on this clock answers before a bound due after
// it ends; a wait that gives up without waiting never gets its tick, and a value that came before
// the wait blocked is taken first. A clock with a deadline, the test binary's own (SetUpDone), ends
// the binary as go test's alarm would once the next timer to fire is due past it. The child reads
// which timers its waits armed and whether each wait took its timer's tick (armed, armedTimers).
// Its lock lets a wait on a goroutine other than the test's, such as a hook's, read it beside the
// test goroutine.
type stepClock struct {
	mu  sync.Mutex
	now time.Time
	// deadline is the test binary's deadline on this clock, or zero for none.
	deadline time.Time
	timers   []*stepTimer
}

// stepTimer is a timer armed on a stepClock: the moment it was armed for, its channel, whether it
// fired, and whether it was stopped. quit, closed by the timer's stop, ends the goroutine that
// would fire it.
type stepTimer struct {
	at      time.Time
	fired   chan time.Time
	sent    bool
	stopped bool
	quit    chan struct{}
}

func (c *stepClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

// set moves c's time to now.
func (c *stepClock) set(now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = now
}

// setDeadline gives c the test binary's deadline on c, past which no timer of c fires (alarm).
func (c *stepClock) setDeadline(deadline time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.deadline = deadline
}

func (c *stepClock) Timer(at time.Time) (<-chan time.Time, func() bool) {
	timer := &stepTimer{at: at, fired: make(chan time.Time, 1), quit: make(chan struct{})}
	c.mu.Lock()
	c.timers = append(c.timers, timer)
	c.mu.Unlock()
	go c.fireOnceWaited(timer)
	return timer.fired, func() bool {
		c.mu.Lock()
		timer.stopped = true
		c.mu.Unlock()
		close(timer.quit)
		return false
	}
}

// wall is the wall clock's moment at which at, a moment on c, comes: the wall clock's now plus
// what is left until at on c, or now once at has come on c. c does not follow the wall clock, so
// this is the moment for a deadline only the kernel keeps, such as a socket's.
func (c *stepClock) wall(at time.Time) time.Time {
	return time.Now().Add(max(at.Sub(c.Now()), 0))
}

// fireOnceWaited fires timer, moving c's time to its moment when that is later, once a goroutine of
// this binary is blocked in a wait's own select on its timer and no other timer still pending on c
// is due before it. It asks at the package's pace until then, and ends without firing once the
// timer is stopped: a wait that returned without waiting has stopped it. It reads the stop again
// after it found a wait blocked, so a timer whose wait returned while it read the stacks does not
// fire for the next wait. A timer due past c's deadline that would fire next panics as go test's
// alarm does (alarm): the binary's deadline comes first.
func (c *stepClock) fireOnceWaited(timer *stepTimer) {
	for {
		select {
		case <-timer.quit:
			return
		default:
		}
		if blockedInAWait() {
			c.mu.Lock()
			switch {
			case timer.stopped:
				c.mu.Unlock()
				return
			case c.earlierThan(timer):
				c.mu.Unlock()
			case !c.deadline.IsZero() && timer.at.After(c.deadline):
				deadline := c.deadline
				c.mu.Unlock()
				alarm(deadline, timer.at)
			default:
				if timer.at.After(c.now) {
					c.now = timer.at
				}
				timer.sent = true
				c.mu.Unlock()
				timer.fired <- timer.at
				return
			}
		}
		time.Sleep(pace)
	}
}

// earlierThan reports whether a timer of c that is neither fired nor stopped is due before timer.
// The caller holds c's lock.
func (c *stepClock) earlierThan(timer *stepTimer) bool {
	for _, other := range c.timers {
		if other != timer && !other.sent && !other.stopped && other.at.Before(timer.at) {
			return true
		}
	}
	return false
}

// alarm ends the test binary as go test's alarm does, with a panic whose text begins as the alarm's
// ("test timed out"), for a step clock that came to the binary's deadline before a timer due at at.
func alarm(deadline, at time.Time) {
	panic(fmt.Sprintf("test timed out: the step clock came to the test binary's deadline, %s, before a timer due at %s (%s past it)",
		deadline.Format(time.RFC3339Nano), at.Format(time.RFC3339Nano), at.Sub(deadline)))
}

// armed is the moment of each timer the waits armed on c, in the order they armed them.
func (c *stepClock) armed() []time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	moments := make([]time.Time, 0, len(c.timers))
	for _, timer := range c.timers {
		moments = append(moments, timer.at)
	}
	return moments
}

// armedTimer is what a child reads of a timer a wait armed on a stepClock: its moment, whether it
// fired, and whether its tick is still in its channel, untaken by the wait.
type armedTimer struct {
	at      time.Time
	sent    bool
	untaken bool
}

// armedTimers is what c holds of each timer the waits armed on it, in the order they armed them.
func (c *stepClock) armedTimers() []armedTimer {
	c.mu.Lock()
	defer c.mu.Unlock()
	timers := make([]armedTimer, 0, len(c.timers))
	for _, timer := range c.timers {
		timers = append(timers, armedTimer{at: timer.at, sent: timer.sent, untaken: len(timer.fired) != 0})
	}
	return timers
}

// useStepClock gives the waits of this test binary a stepClock whose time is now, until t ends.
func useStepClock(t testing.TB, now time.Time) *stepClock {
	stepped := &stepClock{now: now}
	clock = stepped
	t.Cleanup(func() { clock = wallClock{} })
	return stepped
}

// waitFrames is how a goroutine's stack names the functions of this package that select on a
// wait's timer (expiry): received, which Receive, ReceiveInHelper and so Returns wait in, then
// Next, Span and Quiet, each after this package's path.
var waitFrames = func() []string {
	path := strings.TrimSuffix(runtime.FuncForPC(reflect.ValueOf(GiveUpAt).Pointer()).Name(), "GiveUpAt")
	return []string{path + "received[", path + "Next[", path + "Span(", path + "Quiet["}
}()

// blockedInAWait reports whether a goroutine of this binary is blocked in a wait's select on its
// timer: its state is a select, and one of waitFrames is a frame of its stack, the first or any
// below it. runtime.Stack forces its own traceback level, so GOTRACEBACK cannot hide the frames; a
// Go release that renamed a wait's frame or the select's state would, and then a child's timer
// would never fire: the child's wait then runs into the child's -timeout, which its parent gives as
// a hang guard (RunChild), and the parent fails naming it. The stacks are read whole, in a buffer
// grown until they fit, so a binary with many goroutines, a database pool's among them, still has
// the waiting one read.
func blockedInAWait() bool {
	buffer := make([]byte, 1<<20)
	for {
		if n := runtime.Stack(buffer, true); n < len(buffer) {
			buffer = buffer[:n]
			break
		}
		buffer = make([]byte, 2*len(buffer))
	}
	for _, stack := range strings.Split(string(buffer), "\n\n") {
		header, frames, _ := strings.Cut(stack, "\n")
		if !strings.Contains(header, " [select") {
			continue
		}
		for _, frame := range waitFrames {
			if strings.Contains(frames, frame) {
				return true
			}
		}
	}
	return false
}
