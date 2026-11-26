package testwait

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"
)

// childEnv tells the re-executed test binary to run the helpers below, which skip in an ordinary
// run. Some give up on purpose and one reads the deadline and waits under -timeout 0, and the
// parent names each flag, so each runs in a binary of its own.
const childEnv = "TESTWAIT_CHILD"

// childGiveUpAt is the moment the waits of a child give up (GiveUpAt), failing t when the child has
// no deadline: a child's -timeout is the one its parent gives it.
func childGiveUpAt(t *testing.T) time.Time {
	t.Helper()
	end, bounded := GiveUpAt(t)
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

// A wait whose condition holds returns from its first question: it asks once and never sleeps
// at the pace first.
func TestAWaitThatHoldsReturnsAtOnce(t *testing.T) {
	asks := 0
	Eventually(t, "a condition that holds", func() bool { asks++; return true })
	if asks != 1 {
		t.Fatalf("Eventually asked a condition that holds %d times, want 1", asks)
	}
	asks = 0
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
	var stream Stream[string]
	stream.Add("already there")
	if got := Next(t, "an item already added", &stream, func(string) bool { return true }); got != "already there" {
		t.Fatalf("Next returned %q, want the item already added", got)
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

// Next reads a stream in order: it returns the first unread item that matches, and the items
// before it are passed over, so no later Next reads them.
func TestNextAdvancesItsCursor(t *testing.T) {
	var stream Stream[string]
	for _, item := range []string{"a", "b", "c"} {
		stream.Add(item)
	}
	if got := Next(t, "b", &stream, func(item string) bool { return item == "b" }); got != "b" {
		t.Fatalf("Next for b returned %q", got)
	}
	// a came before b, so the Next for b passed it over: a match for a or c finds c.
	if got := Next(t, "a or c", &stream, func(item string) bool { return item == "a" || item == "c" }); got != "c" {
		t.Fatalf("Next for a or c returned %q, want c: a was passed over by the Next for b", got)
	}
	// Every item is read: the next Next waits for an Add, which a producer makes.
	go stream.Add("d")
	if got := Next(t, "an item added after the wait began", &stream, func(string) bool { return true }); got != "d" {
		t.Fatalf("Next for a later item returned %q, want d", got)
	}
}

// Producers on goroutines of their own add while the test goroutine reads: every item arrives
// once, and the race detector sees the stream's lock rather than a race.
func TestNextReadsEveryItemThatConcurrentProducersAdd(t *testing.T) {
	const producers, each = 4, 100
	var stream Stream[string]
	var wrote sync.WaitGroup
	for producer := range producers {
		wrote.Go(func() {
			for item := range each {
				stream.Add(fmt.Sprintf("%d/%d", producer, item))
			}
		})
	}
	read := make([]string, 0, producers*each)
	for range producers * each {
		read = append(read, Next(t, "the next item a producer adds", &stream, func(string) bool { return true }))
	}
	wrote.Wait()
	slices.Sort(read)
	if unique := slices.Compact(slices.Clone(read)); len(unique) != producers*each {
		t.Fatalf("Next read %d distinct items of %d added", len(unique), producers*each)
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

// ReceiveInHelper returns a value a sender sends later with no error, and returns on a close with
// the zero value and no error.
func TestReceiveInHelperReturnsASentValueAndAClose(t *testing.T) {
	sent := make(chan string)
	go func() { sent <- "sent later" }()
	if got, err := ReceiveInHelper("a helper", "a value a sender sends later", sent); got != "sent later" || err != nil {
		t.Fatalf("ReceiveInHelper returned %q and %v, want the value sent and no error", got, err)
	}
	closed := make(chan string)
	close(closed)
	if got, err := ReceiveInHelper("a helper", "the close of a channel", closed); got != "" || err != nil {
		t.Fatalf("ReceiveInHelper on a closed channel returned %q and %v, want the zero value and no error", got, err)
	}
}

// Returns waits for its function, run on a goroutine of its own, to return, and what the function
// wrote is the caller's to read once it has. A function that never returns fails the test naming
// what, at the margin before the deadline and before Go's alarm, with the same line on stderr: the
// child (TestTheWaitsGiveUpChild's "returns") reads a clock it steps (stepClock), which reaches the
// margin once the Receive that Returns waits in waits for its timer.
func TestReturnsWaitsForItsFunctionAndFailsNamingOneThatNeverReturns(t *testing.T) {
	release := make(chan struct{})
	go close(release)
	wrote := ""
	Returns(t, "a function let go by another goroutine", func() {
		<-release
		wrote = "written"
	})
	if wrote != "written" {
		t.Fatalf("Returns returned before its function did: the function wrote %q, want %q", wrote, "written")
	}

	// The child is waited for through Returns itself, and runs on Context, which ends it where that
	// wait gives up, so this row cannot outlast its own binary's deadline.
	timeout := childTimeout(t)
	child := exec.CommandContext(Context(t), os.Args[0], "-test.run=^TestTheWaitsGiveUpChild$/^returns$", "-test.timeout="+timeout.String(), "-test.v")
	child.Env = append(os.Environ(), childEnv+"=1")
	var stdout, stderr bytes.Buffer
	child.Stdout, child.Stderr = &stdout, &stderr
	var ran error
	Returns(t, "the child under -timeout "+timeout.String()+" to exit", func() { ran = child.Run() })
	if ran == nil {
		t.Fatalf("the child was meant to fail; it passed:\n%s\nstderr:\n%s", &stdout, &stderr)
	}
	if strings.Contains(stdout.String(), "panic: test timed out") || strings.Contains(stderr.String(), "panic: test timed out") {
		t.Fatalf("the child's Returns ran into the binary's deadline rather than giving up before it:\n%s\nstderr:\n%s", &stdout, &stderr)
	}
	requireNoRace(t, stdout.String(), stderr.String())
	line := "testwait: TestTheWaitsGiveUpChild/returns: gave up waiting for a function that never returns 30s before the test binary's deadline"
	if !slices.Contains(strings.Split(stderr.String(), "\n"), line) {
		t.Errorf("the child wrote no stderr line %q; its stderr:\n%s", line, &stderr)
	}
	if !strings.Contains(stdout.String(), line+"\n") {
		t.Errorf("the child failed no test with %q; its output:\n%s", line, &stdout)
	}
}

// Context's deadline is the moment a wait gives up, and the context ends with the test that took it.
func TestContextEndsWhereAWaitGivesUpAndWithTheTest(t *testing.T) {
	var taken context.Context
	t.Run("taker", func(t *testing.T) {
		taken = Context(t)
		end, bounded := GiveUpAt(t)
		deadline, has := taken.Deadline()
		if has != bounded || !deadline.Equal(end) {
			t.Fatalf("Context's deadline is %v (%t), want the moment a wait gives up, %v (%t)", deadline, has, end, bounded)
		}
		if err := taken.Err(); err != nil {
			t.Fatalf("Context ended before its test did: %v", err)
		}
	})
	if err := taken.Err(); !errors.Is(err, context.Canceled) {
		t.Fatalf("Context outlived the test that took it: its error is %v, want context.Canceled", err)
	}
}

// A wait gives up the margin before the test binary's deadline, at the moment GiveUpAt names. A
// *testing.T names that deadline; a testing.TB without Deadline, and TestMain, read it from the
// -timeout flag and the process's start, which is no later than testing's own.
func TestAWaitGivesUpTheMarginBeforeTheDeadline(t *testing.T) {
	deadline, bounded := t.Deadline()
	end, ok := GiveUpAt(t)
	if ok != bounded || ok && !end.Equal(deadline.Add(-margin)) {
		t.Fatalf("a wait in a test whose deadline is %v (%t) gives up at %v (%t), want %s before it", deadline, bounded, end, ok, margin)
	}
	fromFlag, ok := GiveUpAt(noDeadline{t})
	if ok != bounded || ok && fromFlag.After(end) {
		t.Fatalf("a wait in a testing.TB without Deadline gives up at %v (%t), want no later than %v (%t)", fromFlag, ok, end, bounded)
	}
	if inMain, ok := mainGiveUpAt(); ok != bounded || !inMain.Equal(fromFlag) {
		t.Fatalf("a wait in TestMain gives up at %v (%t), want where a testing.TB without Deadline does, %v (%t)", inMain, ok, fromFlag, bounded)
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
	at := time.Now().Add(5 * pace)
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
			t.Fatalf("the wall clock's timer for %v had not fired %s after it was armed, which is %s past its moment", at, lateBound, lateBound-5*pace)
		}
	}
	if moment.Before(at) {
		t.Fatalf("the wall clock's timer for %v fired at %v, before its moment", at, moment)
	}
}

// InMain returns nil from its first question when its condition holds, and once it holds when it
// comes to.
func TestInMainReturnsNilOnceItsConditionHolds(t *testing.T) {
	asks := 0
	if err := InMain("a condition that holds", func() bool { asks++; return true }); err != nil || asks != 1 {
		t.Fatalf("InMain over a condition that holds returned %v after %d questions, want nil after 1", err, asks)
	}
	asks = 0
	if err := InMain("a condition that holds at its third question", func() bool { asks++; return asks == 3 }); err != nil || asks != 3 {
		t.Fatalf("InMain over a condition that holds at its third question returned %v after %d questions, want nil after 3", err, asks)
	}
}

// Span and Quiet hold the test for their span when given a reason, and Quiet passes when nothing
// comes.
func TestSpanAndQuietHoldTheirSpan(t *testing.T) {
	Span(t, time.Millisecond, "the test of Span itself: its span is what it proves")
	Quiet(t, "a channel no one sends on", make(chan int), time.Millisecond, "the test of Quiet itself: its span is what it proves")
}

// Span and Quiet refuse a span with no reason: one is a sleep.
func TestSpanAndQuietRefuseAnEmptyWhy(t *testing.T) {
	for _, why := range []string{"", " \t\n"} {
		failure := failureOf(t, func(tb testing.TB) { Span(tb, time.Hour, why) })
		if want := "testwait: " + t.Name() + ": Span needs a why"; !strings.HasPrefix(failure, want) {
			t.Fatalf("Span with the why %q failed with %q, want a failure that begins %q", why, failure, want)
		}
		failure = failureOf(t, func(tb testing.TB) { Quiet(tb, "a channel", make(chan int), time.Hour, why) })
		if want := "testwait: " + t.Name() + ": Quiet needs a why"; !strings.HasPrefix(failure, want) {
			t.Fatalf("Quiet with the why %q failed with %q, want a failure that begins %q", why, failure, want)
		}
	}
}

// Idle refuses a span with no reason, as Span and Quiet do: one is a sleep on a bubble's clock. The
// refusal comes before Idle's synctest.Wait, so it fails the test outside a bubble too.
func TestIdleRefusesAnEmptyWhy(t *testing.T) {
	for _, why := range []string{"", " \t\n"} {
		failure := failureOf(t, func(tb testing.TB) { Idle(tb, time.Hour, why) })
		if want := "testwait: " + t.Name() + ": Idle needs a why"; !strings.HasPrefix(failure, want) {
			t.Fatalf("Idle with the why %q failed with %q, want a failure that begins %q", why, failure, want)
		}
	}
}

// Idle returns once its span has passed on the bubble's clock and every other goroutine of the
// bubble has blocked again: what a timer due at the very end of the span woke has run by then, and
// a timer due past the span has not fired.
func TestIdleHoldsItsSpanAndWaitsForWhatItsEndWoke(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const span = time.Minute
		atTheEnd, pastTheEnd := make(chan struct{}), make(chan struct{})
		go func() {
			time.Sleep(span)
			close(atTheEnd)
		}()
		go func() {
			time.Sleep(span + time.Nanosecond)
			close(pastTheEnd)
		}()
		began := time.Now()
		Idle(t, span, "the test of Idle itself: its span is what it proves")
		if held := time.Since(began); held != span {
			t.Errorf("Idle held the bubble's clock for %v, want its span of %v", held, span)
		}
		select {
		case <-atTheEnd:
		default:
			t.Error("a goroutine woken at the end of Idle's span had not run when Idle returned")
		}
		select {
		case <-pastTheEnd:
			t.Error("a timer due past Idle's span fired before Idle returned")
		default:
		}
		<-pastTheEnd
	})
}

// Bubble hands its function a t whose deadline is what was left of the test's deadline as the
// bubble began, that far past the bubble's start on the bubble's clock, so a wait in the bubble
// gives up the margin before that moment (GiveUpAt). What was left is read on the real clock just
// before Bubble and just after it returns, and the span the bubble's t names lies between the two.
// Under -timeout 0 the test names no deadline, and neither does the bubble's t.
func TestBubbleNamesWhatWasLeftOfTheDeadlinePastItsStart(t *testing.T) {
	deadline, bounded := t.Deadline()
	before := time.Until(deadline)
	var named time.Duration
	var has bool
	Bubble(t, func(t testing.TB) {
		end, ok := GiveUpAt(t)
		named, has = end.Add(margin).Sub(time.Now()), ok
	})
	after := time.Until(deadline)
	switch {
	case has != bounded:
		t.Fatalf("a wait in a bubble names a deadline: %t, want %t, as the test that started the bubble does", has, bounded)
	case bounded && (named > before || named < after):
		t.Fatalf("a wait in a bubble gives up the margin before %v past the bubble's start, want what was left of the test's deadline as the bubble began, between %v and %v", named, after, before)
	}
}

// Quiet fails naming what yielded and the value, and a close counts as yielding.
func TestQuietFailsWhenItsChannelYields(t *testing.T) {
	sent := make(chan string, 1)
	sent <- "an early stop"
	failure := failureOf(t, func(tb testing.TB) { Quiet(tb, "the stop", sent, time.Hour, "the stop is reported nowhere else") })
	if want := "the stop yielded an early stop within 1h0m0s"; !strings.Contains(failure, want) {
		t.Fatalf("Quiet over a channel holding a value failed with %q, want it to say %q", failure, want)
	}
	closed := make(chan string)
	close(closed)
	failure = failureOf(t, func(tb testing.TB) { Quiet(tb, "the stop", closed, time.Hour, "the stop is reported nowhere else") })
	if want := "the stop was closed within 1h0m0s"; !strings.Contains(failure, want) {
		t.Fatalf("Quiet over a closed channel failed with %q, want it to say %q", failure, want)
	}
}

// Every wait gives up the margin before the test binary's deadline, before Go's alarm, and not
// before the margin: it writes its line to stderr and fails the test with the same text, which
// carries the wait's last reading where it has one. The child's waits read a clock the child moves
// (stepClock): its Until's third reading reaches the margin, a nanosecond after its second, so
// Until gives up at a step of the child, whenever the machine lets the child take it, and an Until
// that gave up early names its second reading and reads twice, and fails this row by name. Every
// wait after it begins at the margin, a wait in a cleanup included, asks once and fails. A wait in
// a testing/synctest bubble (Bubble), whose clock starts in 2000, gives up at what was left of the
// test's deadline past the bubble's start, less the margin, not at the real clock's deadline,
// decades past the bubble's clock: the child moves the clock it reads to the bubble's start, then
// past that moment, in steps of its own. The child ran under -timeout 35s before, so a child
// stalled 5 s before its first wait gave up, or 30 s after it, failed this row on a loaded machine
// (INFRA-3904).
func TestAWaitGivesUpWithItsLineOnStderrAndTheSameFailure(t *testing.T) {
	stdout, stderr := runChildMeantToFail(t, "TestTheWaitsGiveUpChild")
	name := "TestTheWaitsGiveUpChild/"
	ending := " 30s before the test binary's deadline"
	requireGaveUp(t, stdout, stderr, []string{
		"testwait: " + name + "until: gave up waiting for a reading that never holds" + ending + `; last read: a later\nreading`,
		"testwait: " + name + "eventually: gave up waiting for a condition that never holds" + ending,
		"testwait: " + name + "next_on_an_empty_stream: gave up waiting for an item no one adds" + ending + "; last read: none",
		"testwait: " + name + "next_past_unmatched_items: gave up waiting for an item that matches" + ending + `; last read: the latest\nitem`,
		"testwait: " + name + "receive: gave up waiting for a send no one makes" + ending,
		"testwait: " + name + "returns: gave up waiting for a function that never returns" + ending,
		"testwait: " + name + "cleanup: gave up waiting for a condition a cleanup waits for" + ending,
		"testwait: " + name + "span: gave up waiting for the end of a 1h0m0s span (a span past the margin)" + ending,
		"testwait: TestMain: gave up waiting for a condition TestMain waits for" + ending,
		"testwait: a helper: gave up waiting for a send no one makes to a helper" + ending,
		"testwait: " + name + "until_in_a_bubble: gave up waiting for a reading in a bubble that never holds" + ending + "; last read: the bubble's reading",
	})
	if !strings.Contains(stdout, "the condition was asked 1 time(s)") {
		t.Errorf("a wait that began after the margin did not ask exactly once; the child's output:\n%s", stdout)
	}
	for _, row := range []string{"until_read_three_times", "armed_no_timer", "past_the_margin_armed_only_the_margin"} {
		if !strings.Contains(stdout, "--- PASS: "+name+row+" (") {
			t.Errorf("the child's row %s did not pass; its output:\n%s", row, stdout)
		}
	}
}

// Under -timeout 0 there is no deadline: a wait has no moment to give up at and no timer, in a
// test or in TestMain, and Context has no deadline. So a wait that is not answered at once keeps
// asking rather than giving up at a false reading or an empty channel: Until returns the third
// reading after three asks, Eventually and InMain hold at their third question, and Receive takes
// a value sent an hour after it began to wait, on a synctest bubble's clock, so no span of its own
// shorter than that passes it. The child has no deadline, so a wait of its that never returns, or a
// Receive that never blocks and so holds the bubble's clock still, would hold it for good: the
// child runs under this test's Context, which ends the margin before this binary's deadline, and a
// child still running then is killed and fails the row by name, with the child's output up to the
// wait it hung in, rather than go test's alarm with none of it.
func TestUnderNoTimeoutAWaitHasNoEnd(t *testing.T) {
	ctx := Context(t)
	child := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestNoDeadlineChild$", "-test.timeout=0", "-test.v")
	child.Env = append(os.Environ(), childEnv+"=1")
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
// gives up on the third reading and names it ("a later\nreading"), and two rows then read that
// there were exactly three and that Until armed no timer. An Until that gave up any time before the
// margin, a nanosecond early or an hour, gives up on the second reading instead: it names that
// reading, which the parent finds no line for, and the count row fails. Every later wait but the
// last then begins at the margin, a wait in a cleanup included, and asks once, and a row reads that
// each timer they armed was for the margin or before it (ReceiveInHelper's is TestMain's moment,
// mainGiveUpAt, which is no later), fired once its wait waited on it, and had its tick taken, so
// no wait waited for a later moment of its own. The last wait runs in a testing/synctest bubble
// (Bubble), whose clock the child moves its stepClock to. It runs only when the parent re-executes
// this binary.
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
				stepped.set(end.Add(-time.Nanosecond))
				return "a reading a nanosecond before the margin", false
			}
			stepped.set(end)
			return "a later\nreading", false
		})
	})
	t.Run("until read three times", func(t *testing.T) {
		if asks != 3 {
			t.Errorf("Until read %d times, want 3: it gives up at the margin, which the third reading reaches, and not before", asks)
		}
	})
	t.Run("armed no timer", func(t *testing.T) {
		if armed := stepped.armed(); len(armed) != 0 {
			t.Errorf("Until armed timers for %v, want none: it reads the clock", armed)
		}
	})
	t.Run("eventually", func(t *testing.T) {
		asks := 0
		t.Cleanup(func() { t.Logf("the condition was asked %d time(s)", asks) })
		Eventually(t, "a condition that never holds", func() bool { asks++; return false })
	})
	t.Run("next on an empty stream", func(t *testing.T) {
		var stream Stream[string]
		Next(t, "an item no one adds", &stream, func(string) bool { return true })
	})
	t.Run("next past unmatched items", func(t *testing.T) {
		var stream Stream[string]
		stream.Add("an earlier item")
		stream.Add("the latest\nitem")
		Next(t, "an item that matches", &stream, func(string) bool { return false })
	})
	t.Run("receive", func(t *testing.T) {
		Receive(t, "a send no one makes", make(chan int))
	})
	t.Run("returns", func(t *testing.T) {
		never := make(chan struct{})
		Returns(t, "a function that never returns", func() { <-never })
	})
	t.Run("cleanup", func(t *testing.T) {
		t.Cleanup(func() { Eventually(t, "a condition a cleanup waits for", func() bool { return false }) })
	})
	t.Run("span", func(t *testing.T) {
		Span(t, time.Hour, "a span past the margin")
	})
	t.Run("in main", func(t *testing.T) {
		if err := InMain("a condition TestMain waits for", func() bool { return false }); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("receive in helper", func(t *testing.T) {
		if _, err := ReceiveInHelper("a helper", "a send no one makes to a helper", make(chan int)); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("past the margin armed only the margin", func(t *testing.T) {
		for _, timer := range stepped.armedTimers() {
			if timer.at.After(end) || !timer.sent || timer.untaken {
				t.Errorf("a wait begun at the margin, %v, armed a timer for %v (fired %t, tick left untaken %t), want one for the margin or before it that fired once the wait waited on it and whose tick the wait took: past the margin a wait asks once", end, timer.at, timer.sent, timer.untaken)
			}
		}
	})
	t.Run("until in a bubble", func(t *testing.T) {
		// The bubble's clock starts in 2000, and the wait in the bubble reads the stepClock moved
		// there: its first reading is at the bubble's start, and its second moves the clock by what
		// was left of this child's give-up just before the bubble began. A wait in a bubble gives up
		// the margin before what was left of the test's deadline as the bubble began, past the
		// bubble's start (Bubble), which is no later, so the second reading is the last: a wait that
		// gave up at the bubble's start names the first, and one that gave up at the real clock's
		// moment, decades past the bubble's, reads a third time, which fails the row.
		left := time.Until(end)
		Bubble(t, func(t testing.TB) {
			began := time.Now()
			stepped.set(began)
			asks := 0
			Until(t, "a reading in a bubble that never holds", func() (string, bool) {
				asks++
				switch asks {
				case 1:
					return "the bubble's first reading", false
				case 2:
					stepped.set(began.Add(left))
					return "the bubble's reading", false
				}
				t.Errorf("the wait in the bubble had not given up %v past the bubble's start, what was left of the child's give-up as the bubble began: it gives up at a moment past the bubble's", left)
				return "a reading past the bubble's give-up", true
			})
		})
	})
}

// Receive gives up when the margin passes and not before, and a Receive that starts past the
// margin takes only a value already sent and otherwise fails at once. The child's Receives read a
// clock the child moves (stepClock), which fires a timer they arm only once the Receive waits on
// it, and the child reads which timers they armed: the Receive begun before the margin must arm
// one, for the margin, and take its tick, and the Receives begun past the margin must arm theirs
// for the margin too, which has come, so the one with no value fails at its first wait. A Receive
// that armed its timer for a fixed span instead, of any length, arms it for another moment than
// the margin, and a Receive that gave up before the margin never waited for its tick: each fails a
// row by name, at whatever speed the machine runs the child. The child ran under -timeout 45s
// before, so a child stalled 30 s after its first Receive gave up ran into its alarm on a loaded
// machine (INFRA-3904).
func TestReceiveGivesUpAtTheMargin(t *testing.T) {
	stdout, stderr := runChildMeantToFail(t, "TestReceiveGivesUpAtTheMarginChild")
	name := "TestReceiveGivesUpAtTheMarginChild/"
	ending := " 30s before the test binary's deadline"
	want := []string{
		"testwait: " + name + "before_the_margin: gave up waiting for a send no one makes" + ending,
		"testwait: " + name + "no_value_past_the_margin: gave up waiting for a later send no one makes" + ending,
	}
	requireGaveUp(t, stdout, stderr, want)
	for _, row := range []string{"the_margin_ended_the_wait", "a_value_past_the_margin", "past_the_margin_armed_only_the_margin"} {
		if !strings.Contains(stdout, "--- PASS: "+name+row+" (") {
			t.Errorf("the child's row %s did not pass; its output:\n%s", row, stdout)
		}
	}
}

// runChildMeantToFail re-executes this binary to run only the test named child (RunChild), under
// -timeout childTimeout and with childEnv set, and returns what the child wrote to stdout and to
// stderr. It fails t unless the child failed before its deadline and with no report of the race
// detector: a child that passed, one whose waits ran into go test's alarm rather than giving up at
// the margin, and one the detector failed, fail t at once.
func runChildMeantToFail(t *testing.T, child string) (stdout, stderr string) {
	t.Helper()
	stdout, stderr, err := RunChild(t, childTimeout(t), "^"+child+"$", childEnv+"=1")
	if err == nil {
		t.Fatalf("the child was meant to fail; it passed:\n%s\nstderr:\n%s", stdout, stderr)
	}
	if strings.Contains(stdout, "panic: test timed out") || strings.Contains(stderr, "panic: test timed out") {
		t.Fatalf("the child's waits ran into the binary's deadline rather than giving up at the margin:\n%s\nstderr:\n%s", stdout, stderr)
	}
	requireNoRace(t, stdout, stderr)
	return stdout, stderr
}

// requireGaveUp fails t for each give-up line of want that the child did not write to stderr as a
// line of its own, and for each it did not fail a test with on stdout, where testing prints the
// failure's text and a newline.
func requireGaveUp(t *testing.T, stdout, stderr string, want []string) {
	t.Helper()
	written := strings.Split(stderr, "\n")
	for _, line := range want {
		if !slices.Contains(written, line) {
			t.Errorf("the child wrote no stderr line %q; its stderr:\n%s", line, stderr)
		}
		if !strings.Contains(stdout, line+"\n") {
			t.Errorf("the child failed no test with %q; its output:\n%s", line, stdout)
		}
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
		if timer := stepped.armedTimers()[0]; !timer.sent || timer.untaken {
			t.Errorf("the Receive begun before the margin gave up without waiting for its timer (fired %t, tick left untaken %t): it gave up before the margin at %v", timer.sent, timer.untaken, end)
		}
	})
	before := len(stepped.armed())
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
	t.Run("past the margin armed only the margin", func(t *testing.T) {
		for _, at := range stepped.armed()[before:] {
			if !at.Equal(end) {
				t.Errorf("a Receive begun past the margin at %v armed its timer for %v, want the margin, which had come: past the margin a Receive asks once and waits for no later moment", end, at)
			}
		}
	})
}

// TestNoDeadlineChild is the child of TestUnderNoTimeoutAWaitHasNoEnd, run under -timeout 0. Beside
// the moments and the timer a wait would give up on, which it reads, each wait runs as a subtest of
// its own, so one that gives up hides none of the others: Until reads false twice and true the
// third time, Eventually and InMain are answered true at their third question, and Receive's value
// is sent an hour after Receive began to wait on an empty channel. A wait that gave up on a false
// reading, on the empty channel or after a span of its own fails its subtest.
//
// Receive runs in a synctest bubble, whose clock moves only while every goroutine in it is durably
// blocked. The sender's synctest.Wait returns once the test goroutine is blocked in Receive, and
// at the bubble's start: an idle bubble wakes a goroutine blocked in Wait before it moves its clock
// (Go 1.27.3's runtime/synctest.go, maybeWakeLocked). The sender's hour then passes at once, but
// only once both goroutines are blocked again, and the bubble moves its clock to its earliest
// timer first: a timer of Receive's own shorter than the hour fires before the value is sent, and
// its give-up fails the subtest, as main's send 100 ms on the real clock failed a span under that.
// The sender also ends with the bubble's test (t.Context), so a Receive that gave up leaves no
// goroutine asleep in the bubble, which synctest.Test would end in a panic rather than in the
// subtest's failure. Receive takes the t Bubble hands its function, which names no deadline when
// the test that started the bubble names none. It runs only when the parent re-executes this
// binary.
func TestNoDeadlineChild(t *testing.T) {
	if os.Getenv(childEnv) == "" {
		t.Skip(childEnv + " is not set: this test is the child of TestUnderNoTimeoutAWaitHasNoEnd")
	}
	if end, ok := GiveUpAt(t); ok {
		t.Errorf("under -timeout 0 a wait gives up at %v, want never", end)
	}
	if end, ok := GiveUpAt(noDeadline{t}); ok {
		t.Errorf("under -timeout 0 a wait in a testing.TB without Deadline gives up at %v, want never", end)
	}
	if end, ok := mainGiveUpAt(); ok {
		t.Errorf("under -timeout 0 a wait in TestMain gives up at %v, want never", end)
	}
	expired, stop := expiry(t)
	stop()
	if expired != nil {
		t.Errorf("under -timeout 0 a wait has a timer to give up on, want none")
	}
	if deadline, ok := Context(t).Deadline(); ok {
		t.Errorf("under -timeout 0 Context has the deadline %v, want none", deadline)
	}
	t.Run("until", func(t *testing.T) {
		asks := 0
		if got := Until(t, "the third reading", func() (int, bool) { asks++; return asks, asks == 3 }); got != 3 || asks != 3 {
			t.Errorf("under -timeout 0 Until returned %d after %d readings, want the third reading after 3", got, asks)
		}
	})
	t.Run("eventually", func(t *testing.T) {
		asks := 0
		Eventually(t, "a condition that holds at its third question", func() bool { asks++; return asks == 3 })
		if asks != 3 {
			t.Errorf("under -timeout 0 Eventually returned after %d questions, want 3", asks)
		}
	})
	t.Run("in main", func(t *testing.T) {
		asks := 0
		if err := InMain("a condition that holds at its third question", func() bool { asks++; return asks == 3 }); err != nil || asks != 3 {
			t.Errorf("under -timeout 0 InMain returned %v after %d questions, want nil after 3", err, asks)
		}
	})
	t.Run("receive", func(t *testing.T) {
		Bubble(t, func(t testing.TB) {
			sent := make(chan string, 1)
			go func() {
				synctest.Wait()
				select {
				case <-time.After(time.Hour):
					sent <- "the value sent an hour after the wait began"
				case <-t.Context().Done():
				}
			}()
			if got := Receive(t, "a value sent an hour after the wait began", sent); got != "the value sent an hour after the wait began" {
				t.Errorf("under -timeout 0 Receive returned %q, want the value sent an hour after it began to wait", got)
			}
		})
	})
}

// noDeadline is a testing.TB with no Deadline method, as a benchmark's is.
type noDeadline struct{ testing.TB }

// failing is a testing.TB whose Fatalf keeps its text and ends the goroutine, as testing's does,
// so a test reads a failure the function under test raised without failing itself.
type failing struct {
	testing.TB
	text string
}

func (f *failing) Fatalf(format string, args ...any) {
	f.text = fmt.Sprintf(format, args...)
	runtime.Goexit()
}

// failureOf runs call on a goroutine of its own with a failing testing.TB, and returns the text it
// failed with, or "" when it returned.
func failureOf(t *testing.T, call func(testing.TB)) string {
	t.Helper()
	tb := &failing{TB: t}
	done := make(chan struct{})
	go func() {
		defer close(done)
		call(tb)
	}()
	Receive(t, "the call to end", done)
	return tb.text
}
