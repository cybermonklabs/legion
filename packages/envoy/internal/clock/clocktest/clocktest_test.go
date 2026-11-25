package clocktest_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/clock"
	"github.com/sjawhar/envoy/internal/clock/clocktest"
	"github.com/sjawhar/envoy/internal/testwait"
)

var epoch = time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)

// childEnv tells the re-executed test binary to run the child its parent names
// (TestAMoveWhoseFunctionNeverReturnsChild, TestAStartedMoveThatGivesUpChild), each of which skips
// in an ordinary run.
const childEnv = "CLOCKTEST_CHILD"

// childTimeout is the -timeout a parent gives its child: the child's move, or its join, gives up
// 5 s into the run, 30 s before its deadline.
const childTimeout = "35s"

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
	if got := testwait.Receive(t, "the channel timer's send in the move past its deadline", channel.C()); !got.Equal(epoch.Add(2 * time.Second)) {
		t.Errorf("the channel timer sent %s, want its deadline %s", got, epoch.Add(2*time.Second))
	}
	if now := c.Now(); !now.Equal(epoch.Add(5 * time.Second)) {
		t.Errorf("after the move the fake reads %s, want %s", now, epoch.Add(5*time.Second))
	}
	if armed := c.Armed(); !slices.Equal(armed, []time.Time{epoch.Add(6 * time.Second)}) {
		t.Errorf("armed after the move: %v, want only the timer due past it", armed)
	}
}

// A move waits for each AfterFunc's function to return before it returns: the function here holds
// until the test lets it go, and the move is still in progress while it holds, however long the
// machine takes, and returns once it is let go.
func TestAMoveWaitsForTheFunctionItFired(t *testing.T) {
	c := clocktest.New(epoch)
	started, release := make(chan struct{}), make(chan struct{})
	c.AfterFunc(time.Second, func() {
		close(started)
		<-release
	})
	moved := make(chan struct{})
	go func() {
		defer close(moved)
		c.Advance(5 * time.Second)
	}()
	testwait.Receive(t, "the fired function to start", started)
	select {
	case <-moved:
		t.Fatal("the move returned while the function it fired still held")
	default:
	}
	close(release)
	testwait.Receive(t, "the move to return once its function returned", moved)
	if now := c.Now(); !now.Equal(epoch.Add(5 * time.Second)) {
		t.Errorf("after the move the fake reads %s, want %s", now, epoch.Add(5*time.Second))
	}
}

// A move waits for each AfterFunc's function through the wait the tests share: under a short
// -timeout a function that never returns ends the move 30 s before the deadline and before Go's
// alarm, with a panic whose text, written to stderr at once as well, names the timer's deadline
// and the move's target (TestAMoveWhoseFunctionNeverReturnsChild).
func TestAMoveGivesUpNamingAFunctionThatNeverReturns(t *testing.T) {
	// The child is waited for through Returns, and runs on testwait.Context, which ends it where
	// that wait gives up, so this row cannot outlast its own binary's deadline.
	child := exec.CommandContext(testwait.Context(t), os.Args[0], "-test.run=^TestAMoveWhoseFunctionNeverReturnsChild$", "-test.timeout="+childTimeout, "-test.v")
	child.Env = append(os.Environ(), childEnv+"=1")
	var stdout, stderr bytes.Buffer
	child.Stdout, child.Stderr = &stdout, &stderr
	var ran error
	testwait.Returns(t, "the child under -timeout "+childTimeout+" to exit", func() { ran = child.Run() })
	if ran == nil {
		t.Fatalf("the child was meant to fail; it passed:\n%s\nstderr:\n%s", &stdout, &stderr)
	}
	if strings.Contains(stdout.String(), "panic: test timed out") || strings.Contains(stderr.String(), "panic: test timed out") {
		t.Fatalf("the child's move ran into the binary's deadline rather than giving up before it:\n%s\nstderr:\n%s", &stdout, &stderr)
	}
	line := "testwait: clocktest: gave up waiting for the function of the AfterFunc due at 2026-09-22T12:00:01Z, which a move to 2026-09-22T12:00:05Z fired, to return 30s before the test binary's deadline"
	if !slices.Contains(strings.Split(stderr.String(), "\n"), line) {
		t.Errorf("the child wrote no stderr line %q; its stderr:\n%s", line, &stderr)
	}
	if !strings.Contains(stderr.String(), "panic: "+line) {
		t.Errorf("the child's move did not panic with %q; its output:\n%s\nstderr:\n%s", line, &stdout, &stderr)
	}
}

// A move StartMove started runs off the test goroutine, so a timer's function that waits for the
// test to act lets the test act while the move waits: StartMove returns while the function waits,
// and join returns once the function has returned and the move with it, with what the function did
// done and the fake at the move's target. A second join returns at once, as a cleanup's join after
// the row's own does. The negative controls: a StartMove that made the move on the caller's
// goroutine does not return while the function waits, and the wait for it fails; a join that
// returned before the move had finds the function's work undone; a join that took a value the move
// sent once waits for a second that never comes, and its second call fails.
func TestAStartedMoveIsJoinedOnceItsTimersFunctionReturns(t *testing.T) {
	c := clocktest.New(epoch)
	entered, release := make(chan struct{}), make(chan struct{})
	ran := false
	c.AfterFunc(time.Second, func() {
		close(entered)
		<-release
		ran = true
	})
	var join func(testing.TB)
	testwait.Returns(t, "StartMove to return while the move it started waits for the timer's function", func() {
		join = c.StartMove("the move past the timer due at 1s", epoch.Add(5*time.Second))
	})
	testwait.Receive(t, "the timer's function, which the started move fired, to start", entered)
	close(release)
	join(t)
	if !ran {
		t.Error("join returned before the timer's function had")
	}
	if now := c.Now(); !now.Equal(epoch.Add(5 * time.Second)) {
		t.Errorf("once join returned the fake reads %s, want the move's target %s", now, epoch.Add(5*time.Second))
	}
	join(t)
}

// A started move whose timer's function never returns fails the test that joins it, by name, on
// the test goroutine, and the test binary goes on: join's wait is the move's one bound, and its
// give-up names the move and the function the move waits for, by its timer's deadline and the
// move's target (TestAStartedMoveThatGivesUpChild, under -timeout 35s, whose join gives up 5 s in).
// The negative controls: a started move that waits for its function through the shared wait, as a
// move on the test goroutine does, panics at that wait's give-up and ends the child; a join whose
// reading does not name the function the move waits for fails the line the row reads.
func TestAStartedMoveThatGivesUpFailsTheTestByName(t *testing.T) {
	// As in TestAMoveGivesUpNamingAFunctionThatNeverReturns, the child is waited for through
	// Returns and runs on testwait.Context, so this row cannot outlast its own binary's deadline.
	child := exec.CommandContext(testwait.Context(t), os.Args[0], "-test.run=^TestAStartedMoveThatGivesUpChild$", "-test.timeout="+childTimeout, "-test.v")
	child.Env = append(os.Environ(), childEnv+"=1")
	var stdout, stderr bytes.Buffer
	child.Stdout, child.Stderr = &stdout, &stderr
	var ran error
	testwait.Returns(t, "the child under -timeout "+childTimeout+" to exit", func() { ran = child.Run() })
	if ran == nil {
		t.Fatalf("the child was meant to fail; it passed:\n%s\nstderr:\n%s", &stdout, &stderr)
	}
	if strings.Contains(stdout.String(), "panic:") || strings.Contains(stderr.String(), "panic:") {
		t.Fatalf("the child panicked rather than failing its test by name:\n%s\nstderr:\n%s", &stdout, &stderr)
	}
	for _, want := range []string{
		"testwait: TestAStartedMoveThatGivesUpChild: gave up waiting for the move past the timer due at 1s to return 30s before the test binary's deadline; last read: the move waits for the function of the AfterFunc due at 2026-09-22T12:00:01Z, which a move to 2026-09-22T12:00:05Z fired, to return",
		"--- FAIL: TestAStartedMoveThatGivesUpChild (",
	} {
		if !strings.Contains(stdout.String(), want) {
			t.Errorf("the child's output does not say %q:\n%s\nstderr:\n%s", want, &stdout, &stderr)
		}
	}
}

// A timer armed for a moment the fake has reached fires as it is armed, as the real clock's does,
// and is never pending: an AfterFunc's function starts on a goroutine the arming call does not wait
// for, a channel timer's value is there at once, a Sleep returns at once and a deadline context is
// ended before it is returned. The fake does not move for any of them. A channel timer armed for a
// moment already past sends the moment it was armed, as Go's does, which arms a wait of zero or
// less for the moment it is armed (time.when), and not the past moment it was asked for.
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
	if got := testwait.Receive(t, "the send of the timer reset to an overdue moment", channel.C()); !got.Equal(epoch) {
		t.Errorf("the timer reset to a second ago sent %s, want the moment it was armed, %s, as Go's sends", got, epoch)
	}
	if err := c.Sleep(testwait.Context(t), 0); err != nil {
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

// AwaitArmedAt returns once a timer due at the one deadline it names is pending, whatever else is
// pending beside it: here a timer due earlier, and the one it waits for, armed on a goroutine of
// its own. It returns the deadlines pending at that reading, both of them, earliest first.
func TestAwaitArmedAtWaitsForItsOneDeadline(t *testing.T) {
	c := clocktest.New(epoch)
	c.AfterFunc(time.Minute, func() {})
	go c.AfterFunc(2*time.Minute, func() {})

	held := c.AwaitArmedAt(t, "the timer armed on a goroutine of its own", epoch.Add(2*time.Minute))
	want := []time.Time{epoch.Add(time.Minute), epoch.Add(2 * time.Minute)}
	if !slices.Equal(held, want) {
		t.Fatalf("AwaitArmedAt returned %v, want %v", held, want)
	}
	if armed := c.Armed(); !slices.Equal(armed, want) {
		t.Fatalf("armed once AwaitArmedAt returned: %v, want %v", armed, want)
	}
}

// AwaitArmedCountAt returns only once as many timers as it names are pending at its one deadline,
// whatever else is pending beside them: here a timer due elsewhere, and two due at the deadline, the
// second armed on a goroutine of its own. It returns the deadlines pending at that reading, all
// three, earliest first; a wait that returned at the first timer due there, or once two timers of
// any deadline were pending, would return the deadline once.
func TestAwaitArmedCountAtWaitsForEveryTimerAtItsDeadline(t *testing.T) {
	c := clocktest.New(epoch)
	at := epoch.Add(2 * time.Minute)
	c.AfterFunc(time.Minute, func() {})
	c.AfterFunc(2*time.Minute, func() {})
	go c.AfterFunc(2*time.Minute, func() {})

	held := c.AwaitArmedCountAt(t, "the two timers at the deadline", at, 2)
	want := []time.Time{epoch.Add(time.Minute), at, at}
	if !slices.Equal(held, want) {
		t.Fatalf("AwaitArmedCountAt returned %v, want %v", held, want)
	}
}

// SetWhen asks its question of the fake's present and every pending deadline, in order, and moves
// only when the answer says to, then as Set does. A timer the code arms while the question is
// asked is armed after it, from the moment the move reached (here, a move that fires nothing, the
// moment it ends at): one armed between a reading of Armed and a Set is armed before the move,
// and here would have fired in it.
func TestSetWhenMovesOnItsAnswerWithNoTimerArmedBetweenTheQuestionAndTheMove(t *testing.T) {
	c := clocktest.New(epoch)
	due := c.After(2 * time.Second)
	c.AfterFunc(5*time.Second, func() {})
	pending := []time.Time{epoch.Add(2 * time.Second), epoch.Add(5 * time.Second)}

	var asked time.Time
	var read []time.Time
	armed, moved := c.SetWhen(func(now time.Time, armed []time.Time) (time.Time, bool) {
		asked, read = now, armed
		return now.Add(3 * time.Second), false
	})
	if moved || !asked.Equal(epoch) || !slices.Equal(read, pending) || !slices.Equal(armed, pending) {
		t.Fatalf("SetWhen = %v, %t after asking at %s of %v; want no move, asked at %s of %v", armed, moved, asked, read, epoch, pending)
	}
	if now := c.Now(); !now.Equal(epoch) || len(due) != 0 {
		t.Fatalf("after an answer of no the fake reads %s with %d values sent, want %s and none", now, len(due), epoch)
	}

	arming := make(chan (<-chan time.Time), 1)
	if _, moved := c.SetWhen(func(now time.Time, _ []time.Time) (time.Time, bool) {
		go func() { arming <- c.After(500 * time.Millisecond) }()
		return now.Add(time.Second), true
	}); !moved {
		t.Fatal("SetWhen did not move on an answer of yes")
	}
	armedBetween := testwait.Receive(t, "the timer armed while the question was asked", arming)
	want := append([]time.Time{epoch.Add(1500 * time.Millisecond)}, pending...)
	if now, armed := c.Now(), c.Armed(); !now.Equal(epoch.Add(time.Second)) || !slices.Equal(armed, want) || len(armedBetween) != 0 {
		t.Fatalf("after the move the fake reads %s with %v armed, the timer armed during the question holding %d values; want %s, %v and none",
			now, armed, len(armedBetween), epoch.Add(time.Second), want)
	}

	if _, moved := c.SetWhen(func(now time.Time, _ []time.Time) (time.Time, bool) { return now.Add(2 * time.Second), true }); !moved {
		t.Fatal("SetWhen did not move on an answer of yes")
	}
	if len(due) != 1 || len(armedBetween) != 1 {
		t.Fatalf("a move to %s sent %d values on the timer due at %s and %d on the one due at %s, want one each", epoch.Add(3*time.Second), len(due), pending[0], len(armedBetween), want[0])
	}
	if armed := c.Armed(); !slices.Equal(armed, pending[1:]) {
		t.Errorf("armed after the move: %v, want only the timer due past it, %v", armed, pending[1:])
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
// The wall clock here is one the test moves, so each reading is exact.
func TestHeldFollowsTheWallClockPlusWhatWasAdded(t *testing.T) {
	wall := epoch
	c := clocktest.HeldOn(func() time.Time { return wall })
	if now := c.Now(); !now.Equal(wall) {
		t.Errorf("unmoved: the held fake reads %s, want the wall clock's %s", now, wall)
	}
	wall = wall.Add(time.Minute)
	if now := c.Now(); !now.Equal(wall) {
		t.Errorf("unmoved, a minute on the wall clock later: the held fake reads %s, want %s", now, wall)
	}

	fired := false
	c.AfterFunc(time.Hour, func() { fired = true })
	c.Advance(time.Hour)
	if now := c.Now(); !now.Equal(wall.Add(time.Hour)) {
		t.Errorf("an hour on: the held fake reads %s, want %s", now, wall.Add(time.Hour))
	}
	if !fired {
		t.Error("Advance by an hour did not fire the timer armed for an hour")
	}
	wall = wall.Add(time.Minute)
	if now := c.Now(); !now.Equal(wall.Add(time.Hour)) {
		t.Errorf("an hour on, a minute on the wall clock later: the held fake reads %s, want %s", now, wall.Add(time.Hour))
	}

	at := epoch.Add(-time.Hour)
	c.Set(at)
	if now := c.Now(); !now.Equal(at) {
		t.Errorf("set back an hour before the start: the held fake reads %s, want %s", now, at)
	}
	wall = wall.Add(time.Minute)
	if now := c.Now(); !now.Equal(at.Add(time.Minute)) {
		t.Errorf("set back, a minute on the wall clock later: the held fake reads %s, want %s", now, at.Add(time.Minute))
	}
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

// StepWall moves what Now reads, forward and back, and fires no timer: the timers stay on the
// fake's own moment, as Go's run on the monotonic clock whatever the wall clock does, and fire
// when a move of that moment reaches them. What a channel timer sends carries the steps, as Go's
// sends the moment it fell due read on the wall clock as it sends (time.sendTime): both the timer
// a move fires and one armed for a moment the fake has already reached, which fires as it is armed.
func TestStepWallMovesNowAndFiresNoTimer(t *testing.T) {
	c := clocktest.New(epoch)
	channel := c.NewTimer(time.Minute)
	ran := false
	c.AfterFunc(time.Minute, func() { ran = true })
	pending := []time.Time{epoch.Add(time.Minute), epoch.Add(time.Minute)}

	for _, step := range []struct {
		by   time.Duration
		want time.Time
	}{{time.Hour, epoch.Add(time.Hour)}, {-2 * time.Hour, epoch.Add(-time.Hour)}} {
		c.StepWall(step.by)
		if now := c.Now(); !now.Equal(step.want) {
			t.Errorf("after a step of %v the fake reads %s, want %s", step.by, now, step.want)
		}
		if armed := c.Armed(); !slices.Equal(armed, pending) {
			t.Errorf("after a step of %v armed: %v, want the timers where they were, %v", step.by, armed, pending)
		}
		if got := len(channel.C()); got != 0 || ran {
			t.Errorf("a step of %v fired a timer: the channel holds %d values, the AfterFunc ran %t", step.by, got, ran)
		}
	}

	c.Advance(time.Minute)
	stepped := epoch.Add(-time.Hour + time.Minute)
	if got := testwait.Receive(t, "the channel timer's send once the fake's own moment reached it", channel.C()); !got.Equal(stepped) {
		t.Errorf("the channel timer sent %s, want its deadline plus the steps, %s", got, stepped)
	}
	if got := testwait.Receive(t, "the send of a channel timer armed for the moment the fake has reached", c.NewTimer(0).C()); !got.Equal(stepped) {
		t.Errorf("a channel timer armed for the fake's present sent %s, want that moment plus the steps, %s", got, stepped)
	}
	if !ran {
		t.Error("the AfterFunc did not run once the fake's own moment reached it")
	}
	if now := c.Now(); !now.Equal(stepped) {
		t.Errorf("after the move the fake reads %s, want its moment plus the steps, %s", now, stepped)
	}
}

// contextEnding is how a context ended: its Err and its Cause, printed by name.
type contextEnding struct{ err, cause error }

func (e contextEnding) String() string { return fmt.Sprintf("Err %v, Cause %v", e.err, e.cause) }

// WithTimeoutCause ends as context.WithTimeoutCause does, on the fake as on the real clock: at its
// deadline with context.DeadlineExceeded as its error and the cause it was given, or
// context.DeadlineExceeded as its cause too when that cause is nil; with context.Canceled as both
// once its cancel runs first; and before it is returned when its wait is zero or less. The real
// clock's deadlines here are a millisecond, each waited for on its own Done.
func TestWithTimeoutCauseEndsAsTheContextPackagesDoes(t *testing.T) {
	cause := errors.New("the allowance ended")
	type ending = contextEnding
	ended := func(ctx context.Context) ending { return ending{ctx.Err(), context.Cause(ctx)} }
	fake := clocktest.New(epoch)

	realAtDeadline, cancel := clock.Real().WithTimeoutCause(context.Background(), time.Millisecond, cause)
	defer cancel()
	testwait.Receive(t, "the real clock's context at its deadline", realAtDeadline.Done())
	fakeAtDeadline, cancel := fake.WithTimeoutCause(context.Background(), time.Minute, cause)
	defer cancel()
	fake.Advance(time.Minute)
	testwait.Receive(t, "the fake's context at its deadline", fakeAtDeadline.Done())
	want := ending{context.DeadlineExceeded, cause}
	if got := ended(realAtDeadline); got != want {
		t.Errorf("the real clock's context at its deadline ended %+v, want %+v", got, want)
	}
	if got := ended(fakeAtDeadline); got != want {
		t.Errorf("the fake's context at its deadline ended %+v, want %+v, as the real clock's does", got, want)
	}

	realCancelled, cancelReal := clock.Real().WithTimeoutCause(context.Background(), time.Hour, cause)
	fakeCancelled, cancelFake := fake.WithTimeoutCause(context.Background(), time.Hour, cause)
	cancelReal()
	cancelFake()
	want = ending{context.Canceled, context.Canceled}
	if got := ended(realCancelled); got != want {
		t.Errorf("the real clock's context cancelled first ended %+v, want %+v", got, want)
	}
	if got := ended(fakeCancelled); got != want {
		t.Errorf("the fake's context cancelled first ended %+v, want %+v, as the real clock's does", got, want)
	}

	realSpent, cancel := clock.Real().WithTimeoutCause(context.Background(), 0, cause)
	defer cancel()
	fakeSpent, cancel := fake.WithTimeoutCause(context.Background(), 0, cause)
	defer cancel()
	want = ending{context.DeadlineExceeded, cause}
	if got := ended(realSpent); got != want {
		t.Errorf("the real clock's context of no wait read %+v as it was returned, want %+v", got, want)
	}
	if got := ended(fakeSpent); got != want {
		t.Errorf("the fake's context of no wait read %+v as it was returned, want %+v, as the real clock's does", got, want)
	}

	realNoCause, cancel := clock.Real().WithTimeoutCause(context.Background(), time.Millisecond, nil)
	defer cancel()
	testwait.Receive(t, "the real clock's context of no cause at its deadline", realNoCause.Done())
	fakeNoCause, cancel := fake.WithTimeoutCause(context.Background(), time.Minute, nil)
	defer cancel()
	fake.Advance(time.Minute)
	testwait.Receive(t, "the fake's context of no cause at its deadline", fakeNoCause.Done())
	want = ending{context.DeadlineExceeded, context.DeadlineExceeded}
	if got := ended(realNoCause); got != want {
		t.Errorf("the real clock's context of no cause at its deadline ended %+v, want %+v", got, want)
	}
	if got := ended(fakeNoCause); got != want {
		t.Errorf("the fake's context of no cause at its deadline ended %+v, want %+v, as the real clock's does", got, want)
	}
}

// TestAMoveWhoseFunctionNeverReturnsChild is the child of
// TestAMoveGivesUpNamingAFunctionThatNeverReturns, run under -timeout 35s: its move fires a
// function that never returns, and gives up 5 s in. It runs only when the parent re-executes this
// binary.
func TestAMoveWhoseFunctionNeverReturnsChild(t *testing.T) {
	if os.Getenv(childEnv) == "" {
		t.Skip(childEnv + " is not set: this test is the child of TestAMoveGivesUpNamingAFunctionThatNeverReturns")
	}
	c := clocktest.New(epoch)
	never := make(chan struct{})
	c.AfterFunc(time.Second, func() { <-never })
	c.Advance(5 * time.Second)
	t.Fatal("the move returned, though the function it fired never does")
}

// TestAStartedMoveThatGivesUpChild is the child of TestAStartedMoveThatGivesUpFailsTheTestByName,
// run under -timeout 35s: it starts a move (StartMove) that fires a function that never returns,
// and joins it; the join gives up 5 s in. Its verdict is the join's alone. It runs only when the
// parent re-executes this binary.
func TestAStartedMoveThatGivesUpChild(t *testing.T) {
	if os.Getenv(childEnv) == "" {
		t.Skip(childEnv + " is not set: this test is the child of TestAStartedMoveThatGivesUpFailsTheTestByName")
	}
	c := clocktest.New(epoch)
	never := make(chan struct{})
	c.AfterFunc(time.Second, func() { <-never })
	join := c.StartMove("the move past the timer due at 1s", epoch.Add(5*time.Second))
	join(t)
}
