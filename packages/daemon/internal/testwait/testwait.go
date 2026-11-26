// Package testwait holds the bounded wait the daemon's tests share. A test that drives real
// processes, a real database or a real socket waits for something it cannot make happen on its
// own — a delivery, the transaction that answers it — and the wait is bounded by the test
// binary's own deadline rather than a fixed span: on a loaded machine a step that is merely slow
// is not a failure, and a condition that never holds still fails, naming what it waited for.
// Eventually is the wait a test usually wants, and Until is Eventually for a condition that can
// say what it read; Context gives a call that takes a context the same bound. The three give up
// 30 s (margin) before the test binary's deadline and at no other moment, with no cap of their
// own, and under -timeout 0 they have no end. WithinPolling takes a bound and a pace of its own;
// Holds answers rather than fails, for a caller whose failure text is its own; Bounded is the
// clamp alone, for a wait that holds the rest itself; and Receive waits for a channel with no
// bound but the test binary's deadline.
//
// A wait of Eventually or Until that gives up writes "testwait: <test>: gave up waiting for
// <what> 30s before the test binary's deadline; last read: <reading>" to stderr at once, then
// fails the test with the same text; "last read" is there only for Until. The direct write is
// there because Go's alarm panics without printing a running test's log, so a cleanup that
// outlasted the margin would otherwise take the message with it. The text is one line: a newline
// in what or in the reading is written as \n. The reading prints as %+v would, but a pointer in it
// prints what it points to (describe). After the failure of any wait here that gives up, Receive's
// included, the test's output carries the goroutine stacks as the wait gave up, the dump Go's alarm
// would print had the wait not given up before it: the waiting goroutine's, then those its test
// started, bounded (goroutineStacks). A wait with a failure text of its own that takes its bound
// from Bounded, Remaining or Context gets the same stacks, read at that bound's end and printed
// when its test fails (watchBound).
//
// The cost of no cap: a wait whose fact never comes holds its test binary until 30 s before the
// deadline (in CI's race step, -timeout 30m, about 29.5 minutes), and every later wait of
// Eventually or Until in that binary begins with the margin already passed, asks its question
// once and fails. Such a give-up adds ", a moment that had passed before this wait began: the
// first give-up line in time order is the cause" before its reading, so the first give-up line in
// time order names the wait that never held, and the later ones are its side effects. A wait in a
// cleanup after its test's wait gave up is one of those, so the cleanups keep the margin to run in;
// under a -timeout of 30 s or less every such wait is. After its failure such a wait prints a line
// that says it is collateral and names the give-up that used the deadline up, its test and what it
// waited for; it prints the goroutine stacks after that line only when no earlier give-up of the
// binary has (stacksFor).
package testwait

import (
	"context"
	"flag"
	"fmt"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"
)

// Poll is pace, the package's own, for a caller that asks a condition in a loop of its own. One
// value, so the package's waits and those loops cannot come to ask at two paces.
const Poll = pace

// margin is how long before the test binary's deadline a wait gives up: the time the failing
// test's cleanups and the failure itself have to run in before Go's alarm panics the binary.
const margin = 30 * time.Second

// pace is how often a condition is asked. It is short enough that a test which passes in
// milliseconds still does, and long enough that a waiting test costs a loaded machine little.
const pace = 10 * time.Millisecond

// started is when this process began, for a testing.TB that cannot name its deadline: testing
// arms its alarm at the -timeout flag's span from a moment later than this one, so the deadline
// read from here is a little early and never late.
var started = time.Now()

// Eventually waits until holds reports true.
func Eventually(t testing.TB, what string, holds func() bool) {
	t.Helper()
	poll(t, what, holds, nil)
}

// Until waits until read reports true, and returns what it read then. The failure prints the last
// value read.
func Until[T any](t testing.TB, what string, read func() (T, bool)) T {
	t.Helper()
	var value T
	poll(t, what, func() bool {
		var holds bool
		value, holds = read()
		return holds
	}, func() string { return describe(value) })
	return value
}

// Context gives a context whose deadline is the binary's deadline less the margin, cancelled when
// t ends. Under -timeout 0 it has no deadline. Its deadline is watched (watchBound): a test that
// fails after a call under it gave up prints the goroutine stacks as that deadline came.
func Context(t testing.TB) context.Context {
	end, bounded := giveUpAt(t)
	if !bounded {
		ctx, cancel := context.WithCancel(context.Background())
		t.Cleanup(cancel)
		return ctx
	}
	watchBound(t, "Context", end, false)
	ctx, cancel := context.WithDeadline(context.Background(), end)
	t.Cleanup(cancel)
	return ctx
}

// Bounded is bound, or what is left of the test binary's own deadline less spare when that is
// sooner, so a -timeout smaller than bound still reports the wait rather than the binary's panic.
// A wait with a span or a spare of its own takes those; Eventually and Receive have none. spare is
// the caller's, not this package's margin. The bound it gives is watched (watchBound): a test that
// fails after its wait gave up at that bound's end prints the goroutine stacks as that end came.
func Bounded(t *testing.T, bound, spare time.Duration) time.Duration {
	if testDeadline, ok := t.Deadline(); ok {
		if left := time.Until(testDeadline) - spare; left < bound {
			watchBound(t, "Bounded", testDeadline.Add(-spare), false)
			return left
		}
	}
	watchBound(t, "Bounded", time.Now().Add(bound), true)
	return bound
}

// WithinPolling waits for condition under the caller's own bound and its own pace, with no
// clamp to the test binary's deadline, failing the test with what it waited for when the bound
// passes: a wait whose span is the thing under test says how long it means to wait.
func WithinPolling(t *testing.T, bound, poll time.Duration, what string, condition func() bool) {
	t.Helper()
	if waitUntil(time.Now().Add(bound), poll, condition) {
		return
	}
	t.Fatalf("timed out waiting for %s", what)
}

// Holds reports whether condition held before bound passed, and fails nothing itself: a caller
// that keeps a failure text of its own, prints what it read when it gave up, or carries on
// either way takes the answer rather than the test. The caller gives the pace as well, because
// a question that costs something — a database read, a walk of /proc — or that is itself an
// attempt must not be asked a hundred times a second; Poll is this package's own.
func Holds(bound, poll time.Duration, condition func() bool) bool {
	return waitUntil(time.Now().Add(bound), poll, condition)
}

// waitUntil asks condition every poll until it holds or deadline passes, reporting which. It
// asks once more after the deadline, so a condition that came true during the last sleep is not
// reported false: the alternative loses a race against the thing it waits for, and the cost is
// one more question on the path where the wait has already failed.
func waitUntil(deadline time.Time, poll time.Duration, condition func() bool) bool {
	for time.Now().Before(deadline) {
		if condition() {
			return true
		}
		time.Sleep(poll)
	}
	return condition()
}

// marginCap is the most Receive gives up before the test binary's deadline: margin, the time the
// failing test's cleanups have to stop what it ran (a launcher, its connections) before Go's alarm
// panics the binary. Under a -test.timeout shorter than marginShare times marginCap, the margin is
// that timeout divided by marginShare instead, so a wait under a -timeout of 30 s or less still
// waits (a quarter of a 25 s -timeout is 6.25 s, and its waits give up 18.75 s after the binary
// began).
const (
	marginCap   = margin
	marginShare = 4
)

// marginOf is the margin of a test binary whose -test.timeout is timeout.
func marginOf(timeout time.Duration) time.Duration {
	return min(marginCap, timeout/marginShare)
}

// clock is the time Receive, poll and asked read and the timer they wait on: waitClock, the real
// one but in this package's own tests, which hand them one they move, so the moment a wait gives
// up is read as an event rather than measured.
type clock interface {
	Now() time.Time
	After(d time.Duration) <-chan time.Time
}

// realClock is the wall clock and its timers.
type realClock struct{}

func (realClock) Now() time.Time                         { return time.Now() }
func (realClock) After(d time.Duration) <-chan time.Time { return time.After(d) }

// Receive waits for one receive from ch (a value, or the close of ch, which gives the zero
// value) and returns it. Its one bound is the test binary's deadline, the one go test's -timeout
// sets: it gives up the margin (marginOf the -timeout) before it and at no other moment, and
// under -timeout 0 it has no end. A row whose verdict is what a call answers waits for that answer
// here, so a call that waits for ever fails by name and a call that a loaded machine makes slow
// passes. A wait that gives up writes "testwait: <test>: gave up waiting for <what> after <waited>
// at <when>, <margin> before the test binary's deadline <deadline> (-test.timeout <timeout>)" to
// stderr at once, and then fails the test with the same text: Go's alarm panics without printing a
// running test's log, so a cleanup that outlasted the margin would otherwise take the failure with
// it. The time waited tells a wait that hung from one that began after an earlier one had spent
// the deadline. <when> is the clock's reading as the wait gave up, so a reader can hold that it
// gave up no earlier than the margin before the deadline. The text is one line: a newline in what
// is written as \n.
func Receive[T any](t *testing.T, what string, ch <-chan T) T {
	t.Helper()
	start := waitClock.Now()
	deadline, bounded := deadlineOf(t)
	timeout := timeoutFlag()
	value, ended, ok := receive(waitClock, deadline, bounded, timeout, ch)
	if ok {
		return value
	}
	stacks := stacksFor(t, what, bounded && !start.Before(deadline.Add(-marginOf(timeout))))
	text := oneLine.Replace(fmt.Sprintf(
		"testwait: %s: gave up waiting for %s after %s at %s, %s before the test binary's deadline %s (-test.timeout %s)",
		t.Name(), what, ended.Sub(start).Round(time.Millisecond), ended.UTC().Format(time.RFC3339Nano), marginOf(timeout),
		deadline.UTC().Format(time.RFC3339Nano), timeout))
	_, _ = os.Stderr.WriteString(text + "\n")
	t.Errorf("%s", text)
	printStacks(t, stacks)
	t.FailNow()
	return value
}

// receive waits on c for one receive from ch, until the margin of timeout before deadline when
// bounded. It returns the value and true, or the zero value, false and c's reading at the moment
// it gave up.
func receive[T any](c clock, deadline time.Time, bounded bool, timeout time.Duration, ch <-chan T) (T, time.Time, bool) {
	var expired <-chan time.Time
	if bounded {
		expired = c.After(deadline.Add(-marginOf(timeout)).Sub(c.Now()))
	}
	select {
	case value := <-ch:
		return value, time.Time{}, true
	case <-expired:
	}
	// The margin has passed: a value that came beside the timer is still taken.
	select {
	case value := <-ch:
		return value, time.Time{}, true
	default:
	}
	var zero T
	return zero, c.Now(), false
}

// ReceiveWithin is one receive from ch (a value, or the close of ch, which gives the zero value)
// within bound of real time, for a wait whose span is the thing under test, as WithinPolling is
// for a condition: an answer that comes late is the failure, so the bound is the caller's alone,
// with no clamp to the test binary's deadline. A receive that has not come by then fails the test
// with "<what> did not come within <bound>: <why>", why being the caller's reading of the late
// answer, what the wait proves the call does not do.
func ReceiveWithin[T any](t *testing.T, bound time.Duration, what, why string, ch <-chan T) T {
	t.Helper()
	timer := time.NewTimer(bound)
	defer timer.Stop()
	select {
	case value := <-ch:
		return value
	case <-timer.C:
	}
	t.Fatalf("%s did not come within %s: %s", what, bound, why)
	var zero T
	return zero
}

// timeoutFlag is the test binary's -test.timeout, and 0 when it has none: the span flagDeadline,
// the package's one reader of that flag, adds to the process's start.
func timeoutFlag() time.Duration {
	deadline, ok := flagDeadline()
	if !ok {
		return 0
	}
	return deadline.Sub(started)
}

// giveUpText matches Receive's failure: the test, what it waited for, the time waited (not
// captured), the give-up moment, the margin, the deadline and the -timeout.
var giveUpText = regexp.MustCompile(`testwait: (\S+): gave up waiting for (.+?) after \S+ at (\S+), (\S+) before the test binary's deadline (\S+) \(-test\.timeout (\S+)\)`)

// RequireGaveUp fails t unless output, what a child test binary printed, holds Receive's give-up
// text for what exactly twice (on stderr at once, then as the test's failure). The text must name
// timeout, the child's -test.timeout, and its margin (marginOf), and a give-up moment no earlier
// than that margin before the deadline it names and no later than that deadline. A timer never
// fires early, so only a wait that gave up before its margin reads an earlier moment, and a loaded
// machine can only make the moment later.
func RequireGaveUp(t testing.TB, output, what string, timeout time.Duration) {
	t.Helper()
	found := giveUpText.FindAllStringSubmatch(output, -1)
	if len(found) != 2 || found[0][0] != found[1][0] {
		t.Fatalf("the child's output holds Receive's give-up text %d times, want the same text twice (stderr at once, then the failure); its output:\n%s", len(found), output)
	}
	parts, receiveMargin := found[0], marginOf(timeout)
	gaveUp, err := time.Parse(time.RFC3339Nano, parts[3])
	if err != nil {
		t.Fatalf("the give-up moment %q: %v", parts[3], err)
	}
	deadline, err := time.Parse(time.RFC3339Nano, parts[5])
	if err != nil {
		t.Fatalf("the deadline %q: %v", parts[5], err)
	}
	switch {
	case parts[2] != what:
		t.Fatalf("the wait gave up waiting for %q, want %q", parts[2], what)
	case parts[6] != timeout.String() || parts[4] != receiveMargin.String():
		t.Fatalf("the wait names the -timeout %s and the margin %s, want %s and %s", parts[6], parts[4], timeout, receiveMargin)
	case gaveUp.Before(deadline.Add(-receiveMargin)):
		t.Fatalf("the wait gave up at %s, before its give-up moment %s, the margin %s before the deadline %s",
			gaveUp.Format(time.RFC3339Nano), deadline.Add(-receiveMargin).Format(time.RFC3339Nano), receiveMargin, deadline.Format(time.RFC3339Nano))
	case gaveUp.After(deadline):
		t.Fatalf("the wait gave up at %s, past the deadline %s", gaveUp.Format(time.RFC3339Nano), deadline.Format(time.RFC3339Nano))
	}
}

// poll asks holds at the package's pace until it reports true. Once the moment giveUpAt names has
// passed and holds still reports false, gaveUp fails t, with reading's text when the wait has one.
// A wait that begins with that moment already passed (spent) asks once, and its failure says so:
// the deadline was spent before this wait began, by an earlier wait that never held or by the run
// itself, so the first give-up line in time order is the cause and this one its side effect.
func poll(t testing.TB, what string, holds func() bool, reading func() string) {
	t.Helper()
	end, bounded := giveUpAt(t)
	spent := bounded && !waitClock.Now().Before(end)
	if !asked(end, bounded, holds) {
		gaveUp(t, what, spent, reading)
	}
}

// waitClock is the time Receive, poll and asked read and the timer they wait on: the real one.
// Only this package's own tests replace it (TestEventuallyHasNoCapOfItsOwn, and the children of
// TestAWaitGivesUpWithItsLineOnStderrAndTheSameFailure and TestReceiveFailsNamingWhenItGaveUp),
// with a clock that moves only as a wait waits, so a wait's span is read without that span of
// real time. No test of this package runs in parallel.
var waitClock clock = realClock{}

// asked asks holds at the package's pace until it reports true, and reports whether it did. Once
// end has passed a false answer ends the asking, so the question is asked once after end; with no
// end (bounded false) it asks until holds reports true.
func asked(end time.Time, bounded bool, holds func() bool) bool {
	for !holds() {
		if bounded && !waitClock.Now().Before(end) {
			return false
		}
		<-waitClock.After(pace)
	}
	return true
}

// giveUpAt is the moment a wait in t gives up: the test binary's deadline less margin, and false
// when the binary has no deadline (-timeout 0). A *testing.T, or any testing.TB with a Deadline
// method, names the deadline; for another (a benchmark) it is the -timeout flag's span from the
// process's start, which is how testing arms its own alarm.
func giveUpAt(t testing.TB) (time.Time, bool) {
	if named, ok := t.(interface{ Deadline() (time.Time, bool) }); ok {
		return lessMargin(named.Deadline())
	}
	return lessMargin(flagDeadline())
}

// lessMargin is deadline less margin, and false when there is no deadline.
func lessMargin(deadline time.Time, bounded bool) (time.Time, bool) {
	if !bounded {
		return time.Time{}, false
	}
	return deadline.Add(-margin), true
}

// flagDeadline is the -timeout flag's span from the process's start, and false when the flag is
// absent or 0.
func flagDeadline() (time.Time, bool) {
	timeout := flag.Lookup("test.timeout")
	if timeout == nil {
		return time.Time{}, false
	}
	getter, ok := timeout.Value.(flag.Getter)
	if !ok {
		return time.Time{}, false
	}
	span, ok := getter.Get().(time.Duration)
	if !ok || span <= 0 {
		return time.Time{}, false
	}
	return started.Add(span), true
}

// oneLine writes a newline or a carriage return as its escape, so a failure text is one line.
var oneLine = strings.NewReplacer("\n", `\n`, "\r", `\r`)

// announce is the text of a wait the margin ended, in the test named name: it writes the text to
// stderr at once, in one write, and returns it. spent says the margin had passed before the wait
// began. reading is the wait's last reading, or nil for a wait that has none.
func announce(name, what string, spent bool, reading func() string) string {
	text := fmt.Sprintf("testwait: %s: gave up waiting for %s %s before the test binary's deadline", name, what, margin)
	if spent {
		text += ", a moment that had passed before this wait began: the first give-up line in time order is the cause"
	}
	if reading != nil {
		text += "; last read: " + reading()
	}
	text = oneLine.Replace(text)
	_, _ = os.Stderr.WriteString(text + "\n")
	return text
}

// gaveUp ends a wait in t that the margin ended: it announces the failure, fails t with the same
// text, and prints the goroutine stacks as the wait gave up beside it (stacksFor), read before
// anything else is written.
func gaveUp(t testing.TB, what string, spent bool, reading func() string) {
	t.Helper()
	stacks := stacksFor(t, what, spent)
	t.Errorf("%s", announce(t.Name(), what, spent, reading))
	printStacks(t, stacks)
	t.FailNow()
}
