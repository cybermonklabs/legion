package testwait

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"reflect"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"
)

// childEnv tells the re-executed test binary to run the helpers below, which skip in an ordinary
// run. Some give up on purpose and one waits under -timeout 0, and the parent names each flag, so
// each runs in a binary of its own.
const childEnv = "TESTWAIT_CHILD"

// stepClock is the clock a child of these tests gives the waits here (clock). Its time moves only
// when the child sets now, or when a timer a wait armed fires, which moves it to the timer's moment.
// A timer fires only once a Receive is blocked waiting on it (fireOnceWaited), so a Receive that
// gives up without waiting never gets its tick. A wait's margin thus passes at a step the child
// takes or at the wait's own step, never at a moment a loaded machine moves, and the child reads
// which timers its waits armed and whether each wait took its timer's tick. Only one wait of the
// child's runs at a time.
type stepClock struct {
	now    time.Time
	timers []*stepTimer
}

// stepTimer is a timer a wait armed on a stepClock: the moment it was armed for, its channel, and
// whether it fired. quit, closed by the timer's stop, ends the goroutine that would fire it.
type stepTimer struct {
	at    time.Time
	fired chan time.Time
	sent  bool
	quit  chan struct{}
}

func (c *stepClock) Now() time.Time { return c.now }

func (c *stepClock) Timer(at time.Time) (<-chan time.Time, func() bool) {
	timer := &stepTimer{at: at, fired: make(chan time.Time, 1), quit: make(chan struct{})}
	c.timers = append(c.timers, timer)
	go c.fireOnceWaited(timer)
	return timer.fired, func() bool { close(timer.quit); return false }
}

// fireOnceWaited fires timer, moving c's time to its moment, once a goroutine of this binary is
// blocked in Receive's own select, which is the wait that armed it: the child runs one wait at a
// time. It asks every Poll until then, and ends without firing once the timer is stopped: a Receive
// that returned without waiting has stopped it.
func (c *stepClock) fireOnceWaited(timer *stepTimer) {
	for {
		select {
		case <-timer.quit:
			return
		default:
		}
		if blockedInReceive() {
			c.now = timer.at
			timer.sent = true
			timer.fired <- timer.at
			return
		}
		time.Sleep(Poll)
	}
}

// receiveFrame is how a goroutine's stack names Receive: this package's path, then Receive and its
// type parameters.
var receiveFrame = strings.TrimSuffix(runtime.FuncForPC(reflect.ValueOf(childGiveUpAt).Pointer()).Name(), "childGiveUpAt") + "Receive["

// blockedInReceive reports whether a goroutine of this binary is blocked inside a Receive: its state
// is a select, and Receive is a frame of its stack, the first or any below it, so a Receive whose
// select sits in a helper it calls still counts. runtime.Stack forces its own traceback level, so
// GOTRACEBACK cannot hide the frames; a Go release that renamed Receive's frame or the select's
// state would, and then a child's timer would never fire: TestUnderNoTimeoutAWaitHasNoEnd and row 89
// each fail by name at their own deadline, and kill their child (runChild).
func blockedInReceive() bool {
	buffer := make([]byte, 1<<20)
	for _, stack := range strings.Split(string(buffer[:runtime.Stack(buffer, true)]), "\n\n") {
		header, frames, _ := strings.Cut(stack, "\n")
		if strings.Contains(header, " [select") && strings.Contains(frames, receiveFrame) {
			return true
		}
	}
	return false
}

// armed is the moment of each timer the waits armed on c, in the order they armed them.
func (c *stepClock) armed() []time.Time {
	moments := make([]time.Time, 0, len(c.timers))
	for _, timer := range c.timers {
		moments = append(moments, timer.at)
	}
	return moments
}

// useStepClock gives the waits of this test binary a stepClock whose time is now, until t ends.
func useStepClock(t *testing.T, now time.Time) *stepClock {
	stepped := &stepClock{now: now}
	clock = stepped
	t.Cleanup(func() { clock = wallClock{} })
	return stepped
}

// childGiveUpAt is the moment the waits of a child give up (giveUpAt), failing t when the child has
// no deadline: a child's -timeout is the one its parent gives it.
func childGiveUpAt(t *testing.T) time.Time {
	t.Helper()
	end, bounded := giveUpAt(t)
	if !bounded {
		t.Fatal("the child has no deadline: it needs the -timeout its parent gives it")
	}
	return end
}

// childTimeout is the -timeout a parent gives its child: what the parent's own deadline leaves less
// the margin, so the parent has the margin to fail in when the child runs into its alarm, which only
// a wait that never gives up does.
func childTimeout(t *testing.T) time.Duration {
	t.Helper()
	return Remaining(t, margin)
}

// A wait whose fact is already there returns from its first question: Until asks once and never
// sleeps at the pace first, and Receive takes a value already sent.
func TestAWaitThatHoldsReturnsAtOnce(t *testing.T) {
	asks := 0
	if got := Until(t, "a reading that holds", func() (string, bool) { asks++; return "the reading", true }); got != "the reading" {
		t.Fatalf("Until returned %q, want the reading that held", got)
	}
	if asks != 1 {
		t.Fatalf("Until asked a reading that holds %d times, want 1", asks)
	}
	sent := make(chan int, 1)
	sent <- 7
	if got := Receive(t, "a value already sent", sent); got != 7 {
		t.Fatalf("Receive returned %d, want the 7 sent", got)
	}
}

// Until returns the reading that held, not one it read before it.
func TestUntilReturnsTheReadingThatHeld(t *testing.T) {
	asks := 0
	got := Until(t, "the third reading", func() (int, bool) { asks++; return asks, asks == 3 })
	if got != 3 {
		t.Fatalf("Until returned %d, want the third reading, the one that held", got)
	}
}

// UntilPolling asks at the caller's pace: two false readings before the one that holds cost it at
// least two of its polls. Sleep never ends early, so the bound below is a floor no load can push
// the row under.
func TestUntilPollingAsksAtItsPace(t *testing.T) {
	const poll = 50 * time.Millisecond
	asks := 0
	started := time.Now()
	got := UntilPolling(t, poll, "the third reading", func() (int, bool) { asks++; return asks, asks == 3 })
	if elapsed := time.Since(started); got != 3 || elapsed < 2*poll {
		t.Fatalf("UntilPolling returned %d after %s, want the third reading after at least two polls of %s", got, elapsed, poll)
	}
}

// Receive returns a value already sent and one a sender sends later, and returns on a close with
// the zero value.
func TestReceiveReturnsASentValueAndAClose(t *testing.T) {
	ready := make(chan int, 1)
	ready <- 7
	if got := Receive(t, "a value already sent", ready); got != 7 {
		t.Fatalf("Receive returned %d, want the 7 sent", got)
	}
	sent := make(chan string)
	go func() { sent <- "sent later" }()
	if got := Receive(t, "a value a sender sends later", sent); got != "sent later" {
		t.Fatalf("Receive returned %q, want the value sent", got)
	}
	closed := make(chan string)
	close(closed)
	if got := Receive(t, "the close of a channel", closed); got != "" {
		t.Fatalf("Receive on a closed channel returned %q, want the zero value", got)
	}
}

// Context's deadline is the test binary's deadline less the margin, where Receive gives up, and
// the context ends with the test that took it.
func TestContextEndsWhereReceiveGivesUpAndWithTheTest(t *testing.T) {
	var taken context.Context
	t.Run("taker", func(t *testing.T) {
		taken = Context(t)
		binary, bounded := t.Deadline()
		deadline, has := taken.Deadline()
		if has != bounded || has && !deadline.Equal(binary.Add(-margin)) {
			t.Fatalf("Context's deadline is %v (%t), want %s before the binary's %v (%t)", deadline, has, margin, binary, bounded)
		}
		if err := taken.Err(); err != nil {
			t.Fatalf("Context ended before its test did: %v", err)
		}
	})
	if err := taken.Err(); !errors.Is(err, context.Canceled) {
		t.Fatalf("Context outlived the test that took it: its error is %v, want context.Canceled", err)
	}
}

// Until and Receive give up the margin before the test binary's deadline, which the test names.
func TestAWaitGivesUpTheMarginBeforeTheDeadline(t *testing.T) {
	deadline, bounded := t.Deadline()
	end, ok := giveUpAt(t)
	if ok != bounded || ok && !end.Equal(deadline.Add(-margin)) {
		t.Fatalf("a wait in a test whose deadline is %v (%t) gives up at %v (%t), want %s before it", deadline, bounded, end, ok, margin)
	}
}

// lateBound is how long after a wall timer was armed the row below reads it as late. It is a
// third of the margin every wait here leaves, so a timer late by more than it is later than any
// scheduling a test of this package stands: the timer the row arms is due in 50 ms.
const lateBound = margin / 3

// The wall clock's timer fires at the moment it was armed for: not before it, and not lateBound
// after it. The moment it sends is the moment it fired, not when this test reads it (time's
// sendTime takes back how late the send is), so a timer that fired early reads early however late
// the machine lets this test read it. Lateness is read as an order rather than a span: the row
// loses only when lateBound's own timer, armed beside it, has come and the wall timer's channel
// still holds nothing. A machine that stalls this test delays both timers alike and leaves the
// wall timer's tick waiting, which the second read takes, so load fails no run.
func TestTheWallClocksTimerFiresAtItsMoment(t *testing.T) {
	at := time.Now().Add(5 * Poll)
	fired, stop := wallClock{}.Timer(at)
	defer stop()
	late := time.NewTimer(lateBound)
	defer late.Stop()
	var moment time.Time
	select {
	case moment = <-fired:
	case <-late.C:
		select {
		case moment = <-fired:
		default:
			t.Fatalf("the wall clock's timer for %v had not fired %s after it was armed, which is %s past its moment", at, lateBound, lateBound-5*Poll)
		}
	}
	if moment.Before(at) {
		t.Fatalf("the wall clock's timer for %v fired at %v, before its moment", at, moment)
	}
}

// requireNoRace fails t when a child's output holds a report of the race detector: a child meant
// to fail fails whatever the detector finds, so its parent reads the report itself.
func requireNoRace(t *testing.T, output ...string) {
	t.Helper()
	for _, text := range output {
		if strings.Contains(text, "WARNING: DATA RACE") {
			t.Fatalf("the child reported a data race:\n%s", text)
		}
	}
}

// Until and Receive give up the margin before the test binary's deadline, before Go's alarm, and
// not before the margin: each writes its line to stderr and fails the test with the same text, which
// carries Until's last reading. The child's waits read a clock the child moves (stepClock): its
// Until's third reading reaches the margin, a nanosecond after its second, and its Receive begins
// past the margin, so each gives up at a step of the child, whenever the machine lets the child take
// it. An Until that gave up early names its second reading and reads twice, and fails this row by
// name. The child ran under -timeout 35s before, so a child stalled 5 s before its second reading, or
// 30 s after its first wait gave up, failed this row on a loaded machine (INFRA-3904).
func TestAWaitGivesUpWithItsLineOnStderrAndTheSameFailure(t *testing.T) {
	child := exec.Command(os.Args[0], "-test.run=^TestTheWaitsGiveUpChild$", "-test.timeout="+childTimeout(t).String(), "-test.v")
	child.Env = append(os.Environ(), childEnv+"=1")
	var stdout, stderr bytes.Buffer
	child.Stdout, child.Stderr = &stdout, &stderr
	if err := child.Run(); err == nil {
		t.Fatalf("the child was meant to fail; it passed:\n%s\nstderr:\n%s", &stdout, &stderr)
	}
	if strings.Contains(stdout.String(), "panic: test timed out") || strings.Contains(stderr.String(), "panic: test timed out") {
		t.Fatalf("the child's waits ran into the binary's deadline rather than giving up before it:\n%s\nstderr:\n%s", &stdout, &stderr)
	}
	requireNoRace(t, stdout.String(), stderr.String())
	name := "TestTheWaitsGiveUpChild/"
	ending := " 30s before the test binary's deadline"
	want := []string{
		"testwait: " + name + "until: gave up waiting for a reading that never holds" + ending + `; last read: a later\nreading`,
		"testwait: " + name + "receive: gave up waiting for a send no one makes" + ending,
	}
	written := strings.Split(stderr.String(), "\n")
	for _, line := range want {
		if !slices.Contains(written, line) {
			t.Errorf("the child wrote no stderr line %q; its stderr:\n%s", line, &stderr)
		}
		if !strings.Contains(stdout.String(), line+"\n") {
			t.Errorf("the child failed no test with %q; its output:\n%s", line, &stdout)
		}
	}
	for _, row := range []string{"until_read_three_times", "armed_no_timer"} {
		if !strings.Contains(stdout.String(), "--- PASS: "+name+row+" (") {
			t.Errorf("the child's row %s did not pass; its output:\n%s", row, &stdout)
		}
	}
}

// childDrain is how long the no-deadline row's parent waits for the child's output once it has
// killed the child (exec.Cmd.WaitDelay): a kill closes the child's end of the pipes at once, so this
// bounds only a pipe some other process still held, and decides no verdict.
const childDrain = time.Second

// Under -timeout 0 there is no deadline: Until asks until its reading holds, however many false
// readings come first, Receive waits until a value comes, and Context has no deadline. The child has
// no alarm of its own, so this test bounds it: it runs under this test's Context, which ends the
// margin before this binary's deadline, and a child still running then is killed and fails the row
// by name. Before, the read of the child's output had no bound: a child whose wait never returned
// held this test to the binary's alarm, which names neither, and was left running when the binary
// died.
func TestUnderNoTimeoutAWaitHasNoEnd(t *testing.T) {
	ctx := Context(t)
	child := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestNoDeadlineChild$", "-test.timeout=0", "-test.v")
	child.Env = append(os.Environ(), childEnv+"=1")
	child.WaitDelay = childDrain
	output, err := child.CombinedOutput()
	if ctx.Err() != nil {
		t.Fatalf("the child under -timeout 0 was still running %s before this test's deadline, so it was killed: a wait in it never returned (%v)\n%s", margin, err, output)
	}
	if err != nil || !strings.Contains(string(output), "--- PASS: TestNoDeadlineChild (") {
		t.Fatalf("under -timeout 0 the child did not pass: %v\n%s", err, output)
	}
}

// TestTheWaitsGiveUpChild is the child of TestAWaitGivesUpWithItsLineOnStderrAndTheSameFailure. Its
// waits read a stepClock an hour before the margin. Until reads three times: the first at that hour,
// the second moves the clock to a nanosecond before the margin, the third to the margin, so Until
// gives up on the third reading and names it ("a later\nreading"), and a row then reads that there
// were exactly three. An Until that gave up any time before the margin, a nanosecond early or an
// hour, gives up on the second reading instead: it names that reading, which the parent finds no line
// for, and the count row fails. Receive then begins past the margin and asks once, and the last row
// reads that neither wait armed a timer. It runs only when the parent re-executes this binary.
func TestTheWaitsGiveUpChild(t *testing.T) {
	if os.Getenv(childEnv) == "" {
		t.Skip(childEnv + " is not set: this test is the child of TestAWaitGivesUpWithItsLineOnStderrAndTheSameFailure")
	}
	end := childGiveUpAt(t)
	stepped := useStepClock(t, end.Add(-time.Hour))
	asks := 0
	t.Run("until", func(t *testing.T) {
		Until(t, "a reading that never holds", func() (string, bool) {
			asks++
			switch asks {
			case 1:
				return "the first reading", false
			case 2:
				stepped.now = end.Add(-time.Nanosecond)
				return "a reading a nanosecond before the margin", false
			}
			stepped.now = end
			return "a later\nreading", false
		})
	})
	t.Run("until read three times", func(t *testing.T) {
		if asks != 3 {
			t.Errorf("Until read %d times, want 3: it gives up at the margin, which the third reading reaches, and not before", asks)
		}
	})
	t.Run("receive", func(t *testing.T) {
		Receive(t, "a send no one makes", make(chan int))
	})
	t.Run("armed no timer", func(t *testing.T) {
		if armed := stepped.armed(); len(armed) != 0 {
			t.Errorf("the waits armed timers for %v, want none: Until reads the clock, and a Receive begun past the margin at %v asks once", armed, end)
		}
	})
}

// Receive gives up when the margin passes and not before, and a Receive that starts past the
// margin takes only a value already sent and otherwise fails at once. The child's Receives read a
// clock the child moves (stepClock), which fires a timer they arm only once the Receive waits on
// it, and the child reads which timers they armed: the Receive begun before the margin must arm
// one, for the margin, and take its tick, and the Receives begun past the margin must arm none. A
// Receive that armed its timer for a fixed span instead, of any length, arms it for another moment
// or arms one past the margin, and a Receive that gave up before the margin never waited for its
// tick: each fails a row by name, at whatever speed the machine runs the child. The child ran under
// -timeout 45s before, so a child stalled 30 s after its first Receive gave up ran into its alarm
// on a loaded machine (INFRA-3904).
func TestReceiveGivesUpAtTheMargin(t *testing.T) {
	child := exec.Command(os.Args[0], "-test.run=^TestReceiveGivesUpAtTheMarginChild$", "-test.timeout="+childTimeout(t).String(), "-test.v")
	child.Env = append(os.Environ(), childEnv+"=1")
	var stdout, stderr bytes.Buffer
	child.Stdout, child.Stderr = &stdout, &stderr
	if err := child.Run(); err == nil {
		t.Fatalf("the child was meant to fail; it passed:\n%s\nstderr:\n%s", &stdout, &stderr)
	}
	if strings.Contains(stdout.String(), "panic: test timed out") || strings.Contains(stderr.String(), "panic: test timed out") {
		t.Fatalf("the child's Receives ran into the binary's deadline rather than giving up at the margin:\n%s\nstderr:\n%s", &stdout, &stderr)
	}
	requireNoRace(t, stdout.String(), stderr.String())
	name := "TestReceiveGivesUpAtTheMarginChild/"
	ending := " 30s before the test binary's deadline"
	want := []string{
		"testwait: " + name + "before_the_margin: gave up waiting for a send no one makes" + ending,
		"testwait: " + name + "no_value_past_the_margin: gave up waiting for a later send no one makes" + ending,
	}
	written := strings.Split(stderr.String(), "\n")
	for _, line := range want {
		if !slices.Contains(written, line) {
			t.Errorf("the child wrote no stderr line %q; its stderr:\n%s", line, &stderr)
		}
		if !strings.Contains(stdout.String(), line+"\n") {
			t.Errorf("the child failed no test with %q; its output:\n%s", line, &stdout)
		}
	}
	for _, row := range []string{"the_margin_ended_the_wait", "a_value_past_the_margin", "armed_no_timer_past_the_margin"} {
		if !strings.Contains(stdout.String(), "--- PASS: "+name+row+" (") {
			t.Errorf("the child's row %s did not pass; its output:\n%s", row, &stdout)
		}
	}
}

// TestReceiveGivesUpAtTheMarginChild is the child of TestReceiveGivesUpAtTheMargin. Its Receives
// read a stepClock an hour before the margin: the first arms its timer, which fires once that
// Receive waits on it and moves the clock to the moment it was armed for, so the Receives after it
// begin past the margin when the first armed it for the margin. It runs only when the parent
// re-executes this binary.
func TestReceiveGivesUpAtTheMarginChild(t *testing.T) {
	if os.Getenv(childEnv) == "" {
		t.Skip(childEnv + " is not set: this test is the child of TestReceiveGivesUpAtTheMargin")
	}
	end := childGiveUpAt(t)
	stepped := useStepClock(t, end.Add(-time.Hour))
	t.Run("before the margin", func(t *testing.T) {
		Receive(t, "a send no one makes", make(chan int))
	})
	t.Run("the margin ended the wait", func(t *testing.T) {
		if armed := stepped.armed(); len(armed) != 1 || !armed[0].Equal(end) {
			t.Fatalf("the Receive begun before the margin armed timers for %v, want one, for the margin at %v", armed, end)
		}
		if timer := stepped.timers[0]; !timer.sent || len(timer.fired) != 0 {
			t.Errorf("the Receive begun before the margin gave up without waiting for its timer (fired %t, tick left untaken %t): it gave up before the margin at %v", timer.sent, len(timer.fired) != 0, end)
		}
	})
	before := len(stepped.timers)
	t.Run("a value past the margin", func(t *testing.T) {
		ready := make(chan int, 1)
		ready <- 7
		if got := Receive(t, "a value already sent", ready); got != 7 {
			t.Fatalf("Receive past the margin returned %d, want the 7 sent", got)
		}
	})
	t.Run("no value past the margin", func(t *testing.T) {
		Receive(t, "a later send no one makes", make(chan int))
	})
	t.Run("armed no timer past the margin", func(t *testing.T) {
		if armed := stepped.armed()[before:]; len(armed) != 0 {
			t.Errorf("the Receives begun past the margin at %v armed timers for %v, want none: past the margin a Receive asks once", end, armed)
		}
	})
}

// TestNoDeadlineChild is the child of TestUnderNoTimeoutAWaitHasNoEnd, run under -timeout 0: Until
// reads false twice and true the third time, Receive's value is sent only once Receive is blocked
// waiting for it (blockedInReceive), and Context has no deadline. A wait that gave up on a false
// reading or on an empty channel fails it: the channel is empty when the wait begins, whenever the
// machine runs the sender, so a Receive that gave up at once would find nothing. It runs only when
// the parent re-executes this binary.
func TestNoDeadlineChild(t *testing.T) {
	if os.Getenv(childEnv) == "" {
		t.Skip(childEnv + " is not set: this test is the child of TestUnderNoTimeoutAWaitHasNoEnd")
	}
	asks := 0
	if got := Until(t, "the third reading", func() (int, bool) { asks++; return asks, asks == 3 }); got != 3 || asks != 3 {
		t.Errorf("under -timeout 0 Until returned %d after %d readings, want the third reading after 3", got, asks)
	}
	sent := make(chan string)
	go func() {
		for !blockedInReceive() {
			time.Sleep(Poll)
		}
		sent <- "sent later"
	}()
	if got := Receive(t, "a value sent once the wait is blocked", sent); got != "sent later" {
		t.Errorf("under -timeout 0 Receive returned %q, want the value sent", got)
	}
	if deadline, ok := Context(t).Deadline(); ok {
		t.Errorf("under -timeout 0 Context has the deadline %v, want none", deadline)
	}
}
