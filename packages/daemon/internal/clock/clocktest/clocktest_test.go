package clocktest_test

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/clock/clocktest"
	"github.com/sjawhar/legion/daemon/internal/testwait"
)

var epoch = time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)

// Advance fires every timer due by the moment it moves to, earliest first and the timers due
// together in the order they were armed, each reading its own deadline as the fake's time; a
// timer one of them arms inside the span fires in the same move, and none due after it fires.
func TestAdvanceFiresTimersInDeadlineOrder(t *testing.T) {
	c := clocktest.New(epoch)
	var mu sync.Mutex
	var fired []string
	record := func(name string, at time.Time) {
		mu.Lock()
		defer mu.Unlock()
		fired = append(fired, name+"@"+at.Sub(epoch).String())
	}
	channel := c.NewTimer(2 * time.Second)
	c.AfterFunc(3*time.Second, func() { record("third", c.Now()) })
	c.AfterFunc(time.Second, func() {
		record("first", c.Now())
		c.AfterFunc(time.Second, func() { record("armed by the first", c.Now()) })
	})
	c.AfterFunc(2*time.Second, func() {
		record("second", c.Now())
		if len(channel.C()) != 1 {
			record("the channel timer due with it had not fired", c.Now())
		}
	})
	c.AfterFunc(6*time.Second, func() { record("past the move", c.Now()) })

	c.Advance(5 * time.Second)

	want := []string{"first@1s", "second@2s", "armed by the first@2s", "third@3s"}
	if mu.Lock(); !slices.Equal(fired, want) {
		t.Errorf("fired %v, want %v", fired, want)
	}
	mu.Unlock()
	if got := <-channel.C(); !got.Equal(epoch.Add(2 * time.Second)) {
		t.Errorf("the channel timer sent %s, want its deadline %s", got, epoch.Add(2*time.Second))
	}
	if now := c.Now(); !now.Equal(epoch.Add(5 * time.Second)) {
		t.Errorf("after the move the fake reads %s, want %s", now, epoch.Add(5*time.Second))
	}
	if armed := c.Armed(); !slices.Equal(armed, []time.Time{epoch.Add(6 * time.Second)}) {
		t.Errorf("armed after the move: %v, want only the timer due past it", armed)
	}
}

// A timer armed for a moment the fake has reached fires as it is armed, as the real clock's does,
// and is never pending: an AfterFunc's function starts on a goroutine the arming call does not wait
// for, a channel timer's value is there at once, a Sleep returns at once and a deadline context is
// ended before it is returned. The fake does not move for any of them.
func TestATimerDueAtItsArmFiresAsItIsArmed(t *testing.T) {
	c := clocktest.New(epoch)
	release, ran := make(chan struct{}), make(chan struct{})
	// The function holds until the test lets it go, so an arm that waited for it would never return.
	c.AfterFunc(-time.Minute, func() {
		<-release
		close(ran)
	})
	close(release)
	testwait.Receive(t, "the overdue AfterFunc's function", ran)

	channel := c.NewTimer(0)
	if got := len(channel.C()); got != 1 {
		t.Errorf("a channel timer of no wait holds %d values as it is armed, want its firing", got)
	}
	if channel.Stop() {
		t.Error("Stop of a timer that fired as it was armed reported it armed")
	}
	if channel.Reset(-time.Second) {
		t.Error("Reset of a fired timer reported it armed")
	}
	if got := <-channel.C(); !got.Equal(epoch.Add(-time.Second)) {
		t.Errorf("the timer reset to an overdue moment sent %s, want %s", got, epoch.Add(-time.Second))
	}
	if err := c.Sleep(context.Background(), 0); err != nil {
		t.Errorf("a Sleep of no wait returned %v, want nil at once", err)
	}
	ctx, cancel := c.WithTimeout(context.Background(), 0)
	defer cancel()
	if err := ctx.Err(); err != context.DeadlineExceeded {
		t.Errorf("a deadline context of no wait reports %v as it is returned, want context.DeadlineExceeded", err)
	}
	if armed := c.Armed(); len(armed) != 0 {
		t.Errorf("armed %v, want nothing pending: every timer here fired as it was armed", armed)
	}
	if now := c.Now(); !now.Equal(epoch) {
		t.Errorf("the fake reads %s after the overdue timers fired, want it still at %s", now, epoch)
	}
}

// AwaitArmed returns once the code has armed what the test waits for, with every pending
// deadline in order: a Sleep in progress and a deadline context among them.
func TestAwaitArmedReturnsThePendingDeadlines(t *testing.T) {
	c := clocktest.New(epoch)
	slept := make(chan error, 1)
	go func() {
		c.AfterFunc(2*time.Minute, func() {})
		slept <- c.Sleep(context.Background(), time.Minute)
	}()
	_, cancel := c.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	armed := c.AwaitArmed(t, "the sleep, the timer and the deadline", 3)
	want := []time.Time{epoch.Add(time.Minute), epoch.Add(2 * time.Minute), epoch.Add(3 * time.Minute)}
	if !slices.Equal(armed, want) {
		t.Fatalf("AwaitArmed = %v, want %v", armed, want)
	}
	c.Advance(time.Minute)
	if err := testwait.Receive(t, "the sleep's end", slept); err != nil {
		t.Fatalf("the sleep ended with %v, want nil once the fake passed its deadline", err)
	}
	cancel()
	if armed := c.Armed(); !slices.Equal(armed, want[1:2]) {
		t.Errorf("armed after the sleep and the cancel: %v, want %v", armed, want[1:2])
	}
}

// A Sleep the context ends first returns the context's error and leaves nothing armed.
func TestASleepEndsWithItsContext(t *testing.T) {
	c := clocktest.New(epoch)
	ctx, cancel := context.WithCancel(context.Background())
	slept := make(chan error, 1)
	go func() { slept <- c.Sleep(ctx, time.Hour) }()
	c.AwaitArmed(t, "the sleep", 1)
	cancel()
	if err := testwait.Receive(t, "the sleep's end", slept); !errors.Is(err, context.Canceled) {
		t.Fatalf("the sleep ended with %v, want its context's cancellation", err)
	}
	if armed := c.Armed(); len(armed) != 0 {
		t.Errorf("armed after the sleep ended: %v, want nothing", armed)
	}
}

// A deadline context on the fake ends when the fake reaches its deadline, with
// context.DeadlineExceeded as its error and its cause, and so does a context derived from it; its
// Deadline reports none of the fake's, and a parent's real deadline is the one it reports.
func TestADeadlineContextEndsAtTheFakesDeadline(t *testing.T) {
	c := clocktest.New(epoch)
	ctx, cancel := c.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	derived, cancelDerived := context.WithCancel(ctx)
	defer cancelDerived()
	if at, ok := ctx.Deadline(); ok {
		t.Fatalf("the fake's deadline context reports a deadline %s; a fake deadline must not reach a caller that reads the wall clock", at)
	}
	c.Advance(time.Minute - time.Nanosecond)
	if err := ctx.Err(); err != nil {
		t.Fatalf("the context ended with %v before the fake reached its deadline", err)
	}
	c.Advance(time.Nanosecond)
	select {
	case <-ctx.Done():
	default:
		t.Fatal("the context was not done once the fake reached its deadline")
	}
	if err, cause := ctx.Err(), context.Cause(ctx); err != context.DeadlineExceeded || cause != context.DeadlineExceeded {
		t.Fatalf("the context ended with %v (cause %v), want context.DeadlineExceeded for both", err, cause)
	}
	testwait.Receive(t, "the derived context's end", derived.Done())
	if err, cause := derived.Err(), context.Cause(derived); err != context.DeadlineExceeded || cause != context.DeadlineExceeded {
		t.Errorf("the derived context ended with %v (cause %v), want context.DeadlineExceeded for both", err, cause)
	}

	wall, cancelWall := context.WithDeadline(context.Background(), time.Now().Add(time.Hour))
	defer cancelWall()
	under, cancelUnder := c.WithDeadline(wall, c.Now().Add(time.Minute))
	defer cancelUnder()
	parent, _ := wall.Deadline()
	if at, ok := under.Deadline(); !ok || !at.Equal(parent) {
		t.Errorf("the context under a real deadline reports %s, %v; want its parent's %s", at, ok, parent)
	}
	cancelUnder()
	if err := under.Err(); err != context.Canceled {
		t.Errorf("a cancelled deadline context reports %v, want context.Canceled", err)
	}
	if armed := c.Armed(); len(armed) != 0 {
		t.Errorf("armed after every deadline context ended: %v, want nothing", armed)
	}
}

// A deadline context whose parent has already ended is ended at once, with the parent's error.
func TestADeadlineContextUnderAnEndedParentIsEnded(t *testing.T) {
	c := clocktest.New(epoch)
	parent, cancelParent := context.WithCancel(context.Background())
	cancelParent()
	ctx, cancel := c.WithDeadline(parent, epoch.Add(time.Hour))
	defer cancel()
	if err := ctx.Err(); err != context.Canceled {
		t.Fatalf("the context under an ended parent reports %v, want the parent's context.Canceled", err)
	}
}

// A parent's cancel ends the deadline context by the time the cancel returns, as it ends a
// context.WithDeadline's: Err and Done read it ended without waiting for any goroutine.
func TestADeadlineContextEndsWithItsParentsCancel(t *testing.T) {
	c := clocktest.New(epoch)
	for range 1000 {
		parent, cancelParent := context.WithCancel(context.Background())
		ctx, cancel := c.WithDeadline(parent, epoch.Add(time.Hour))
		cancelParent()
		if err := ctx.Err(); err != context.Canceled {
			cancel()
			t.Fatalf("right after its parent's cancel the context reports %v, want context.Canceled", err)
		}
		select {
		case <-ctx.Done():
		default:
			cancel()
			t.Fatal("right after its parent's cancel the context's Done is open, want it closed")
		}
		cancel()
	}
	if armed := c.Armed(); len(armed) != 0 {
		t.Errorf("armed after every deadline context ended: %v, want nothing", armed)
	}
}

// Held reads the wall clock plus what Advance and Set added, and its timers fire on those moves.
func TestHeldFollowsTheWallClockPlusWhatWasAdded(t *testing.T) {
	c := clocktest.Held()
	within := func(what string, got, from, to time.Time) {
		t.Helper()
		if got.Before(from) || got.After(to) {
			t.Errorf("%s: the held fake reads %s, want between %s and %s", what, got, from, to)
		}
	}
	before := time.Now()
	now := c.Now()
	within("unmoved", now, before, time.Now())

	fired := false
	c.AfterFunc(time.Hour, func() { fired = true })
	before = time.Now()
	c.Advance(time.Hour)
	now = c.Now()
	within("an hour on", now, before.Add(time.Hour), time.Now().Add(time.Hour))
	if !fired {
		t.Error("Advance by an hour did not fire the timer armed for an hour")
	}

	at := time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)
	before = time.Now()
	c.Set(at)
	now = c.Now()
	within("set back to noon", now, at, at.Add(time.Since(before)))
}

// Stop disarms a timer and says whether it did; Reset arms it again and says whether it was armed,
// and no receive after either reads a value the timer sent before it.
func TestStopAndReset(t *testing.T) {
	c := clocktest.New(epoch)
	timer := c.NewTimer(time.Minute)
	if !timer.Stop() {
		t.Error("Stop of an armed timer reported it was not armed")
	}
	if timer.Stop() {
		t.Error("a second Stop reported the timer armed")
	}
	if armed := c.Armed(); len(armed) != 0 {
		t.Errorf("armed after Stop: %v, want nothing", armed)
	}
	if timer.Reset(2 * time.Minute) {
		t.Error("Reset of a stopped timer reported it armed")
	}
	if !timer.Reset(30 * time.Second) {
		t.Error("Reset of an armed timer reported it was not armed")
	}
	if armed := c.Armed(); !slices.Equal(armed, []time.Time{epoch.Add(30 * time.Second)}) {
		t.Errorf("armed after the Resets: %v, want the last one's deadline only", armed)
	}
	c.Advance(30 * time.Second)
	if timer.Stop() {
		t.Error("Stop of a fired timer reported it armed")
	}
	select {
	case got := <-timer.C():
		t.Errorf("a receive after Stop read %s, a value sent before it", got)
	default:
	}

	timer.Reset(time.Minute)
	c.Advance(time.Minute)
	timer.Reset(time.Minute)
	select {
	case got := <-timer.C():
		t.Errorf("a receive after Reset read %s, a value sent before it", got)
	default:
	}

	ran := false
	stopped := c.AfterFunc(time.Minute, func() { ran = true })
	if !stopped.Stop() {
		t.Error("Stop of an armed AfterFunc reported it was not armed")
	}
	c.Advance(time.Hour)
	if ran {
		t.Error("a stopped AfterFunc ran")
	}
}
