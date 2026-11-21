// Package testwait holds the bounded wait the daemon's tests share. A test that drives real
// processes, a real database or a real socket waits for something it cannot make happen on its
// own — a delivery, the transaction that answers it — and the wait is bounded by the test
// binary's own deadline rather than a fixed span: on a loaded machine a step that is merely slow
// is not a failure, and a condition that never holds still fails, naming what it waited for.
// Eventually is the wait a test usually wants; WithinPolling takes a bound and a pace of its
// own; Holds answers rather than fails, for a caller whose failure text is its own; Bounded
// is the clamp alone, for a wait that holds the rest itself; and Receive waits for a channel
// with no bound but the test binary's deadline.
package testwait

import (
	"flag"
	"fmt"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"
)

// Poll is how often a condition is asked. It is short enough that a test which passes in
// milliseconds still does, and long enough that a waiting test costs a loaded machine little.
const Poll = 10 * time.Millisecond

// Eventually waits for condition, failing the test with what it waited for when the deadline
// passes. The deadline is a minute, or the test binary's own less a second when that is sooner,
// so a `-timeout` smaller than a minute still reports the wait rather than the binary's panic.
func Eventually(t *testing.T, what string, condition func() bool) {
	t.Helper()
	WithinPolling(t, Bounded(t, time.Minute, time.Second), Poll, what, condition)
}

// Bounded is bound, or what is left of the test binary's own deadline less margin when that is
// sooner, so a -timeout smaller than bound still reports the wait rather than the binary's panic.
// Eventually is Bounded of a minute and a second; a wait with a span or a margin of its own takes
// those.
func Bounded(t *testing.T, bound, margin time.Duration) time.Duration {
	if testDeadline, ok := t.Deadline(); ok {
		if left := time.Until(testDeadline) - margin; left < bound {
			bound = left
		}
	}
	return bound
}

// WithinPolling waits for condition under the caller's own bound and its own pace, with no
// clamp to the test binary's deadline, failing the test with what it waited for when the bound
// passes: a wait whose span is the thing under test, or one that must outlast a minute, says
// how long it means to wait.
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

// marginCap is the most Receive gives up before the test binary's deadline: the time the failing
// test's cleanups have to stop what it ran (a launcher, its connections) before Go's alarm panics
// the binary. Under a -test.timeout shorter than marginShare times marginCap, the margin is that
// timeout divided by marginShare instead, so a wait under a -timeout of 30 s or less still waits
// (a quarter of a 25 s -timeout is 6.25 s, and its waits give up 18.75 s after the binary began).
const (
	marginCap   = 30 * time.Second
	marginShare = 4
)

// marginOf is the margin of a test binary whose -test.timeout is timeout.
func marginOf(timeout time.Duration) time.Duration {
	return min(marginCap, timeout/marginShare)
}

// clock is the time Receive reads and the timer it waits on. Receive's is the real one; this
// package's own tests hand receive one they move, so the moment a wait gives up is read as an
// event rather than measured.
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
// under -timeout 0 it has no end. A wait that gives up writes "testwait: <test>: gave up waiting
// for <what> at <when>, <margin> before the test binary's deadline <deadline> (-test.timeout
// <timeout>)" to stderr at once, and then fails the test with the same text: Go's alarm panics
// without printing a running test's log, so a cleanup that outlasted the margin would otherwise
// take the failure with it. <when> is the clock's reading as the wait gave up, so a reader can
// hold that it gave up no earlier than the margin before the deadline.
func Receive[T any](t *testing.T, what string, ch <-chan T) T {
	t.Helper()
	deadline, bounded := t.Deadline()
	timeout := timeoutFlag()
	value, gaveUp, ok := receive(realClock{}, deadline, bounded, timeout, ch)
	if ok {
		return value
	}
	text := strings.NewReplacer("\n", `\n`, "\r", `\r`).Replace(fmt.Sprintf(
		"testwait: %s: gave up waiting for %s at %s, %s before the test binary's deadline %s (-test.timeout %s)",
		t.Name(), what, gaveUp.UTC().Format(time.RFC3339Nano), marginOf(timeout), deadline.UTC().Format(time.RFC3339Nano), timeout))
	_, _ = os.Stderr.WriteString(text + "\n")
	t.Fatalf("%s", text)
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

// timeoutFlag is the test binary's -test.timeout, and 0 when it has none.
func timeoutFlag() time.Duration {
	timeout := flag.Lookup("test.timeout")
	if timeout == nil {
		return 0
	}
	getter, ok := timeout.Value.(flag.Getter)
	if !ok {
		return 0
	}
	span, _ := getter.Get().(time.Duration)
	return span
}

// giveUpText matches Receive's failure: the test, what it waited for, the give-up moment, the
// margin, the deadline and the -timeout.
var giveUpText = regexp.MustCompile(`testwait: (\S+): gave up waiting for (.+?) at (\S+), (\S+) before the test binary's deadline (\S+) \(-test\.timeout (\S+)\)`)

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
	parts, margin := found[0], marginOf(timeout)
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
	case parts[6] != timeout.String() || parts[4] != margin.String():
		t.Fatalf("the wait names the -timeout %s and the margin %s, want %s and %s", parts[6], parts[4], timeout, margin)
	case gaveUp.Before(deadline.Add(-margin)):
		t.Fatalf("the wait gave up at %s, before its give-up moment %s, the margin %s before the deadline %s",
			gaveUp.Format(time.RFC3339Nano), deadline.Add(-margin).Format(time.RFC3339Nano), margin, deadline.Format(time.RFC3339Nano))
	case gaveUp.After(deadline):
		t.Fatalf("the wait gave up at %s, past the deadline %s", gaveUp.Format(time.RFC3339Nano), deadline.Format(time.RFC3339Nano))
	}
}
