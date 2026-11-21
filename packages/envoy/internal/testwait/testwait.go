// Package testwait holds the bounded wait Envoy's tests share. A test that drives real
// processes, a real database or a real socket waits for something it cannot make happen on its
// own — a delivery, the transaction that answers it — and the wait is bounded by the test
// binary's own deadline rather than a fixed span: on a loaded machine a step that is merely slow
// is not a failure, and a condition that never holds still fails, naming what it waited for.
// Eventually is the wait a test usually wants; WithinPolling takes a bound and a pace of its
// own; Holds answers rather than fails, for a caller whose failure text is its own; and Bounded
// is the clamp alone, for a wait that holds the rest itself.
//
// Eventually, Until and Receive take no bound of their own: each gives up margin (30 s)
// before the test binary's deadline, the one go test's -timeout sets, and at no other moment, so
// only the fact it waits for ends it, and under -timeout 0 it has no end. A wait that begins with
// 30 s or less of the binary's deadline left, as every wait does under a -timeout of 30 s or less,
// has already passed that moment: Eventually and Until ask once, Receive takes only a value
// already sent or a close, and each fails the test at once when that answer is not there.
// Eventually waits until a condition holds, asking every Poll. Until waits until a
// reading holds and returns it, asking every Poll, and its failure prints the last value it read;
// UntilPolling is Until at a pace the caller gives, for a reading that costs something, such as a
// request to a cluster. Receive waits for one receive from a channel. A wait that gives up writes
// "testwait: <test>: gave up waiting for <what> 30s before the test binary's deadline; last read:
// <reading>" to stderr at once, then fails the test with the same text: without -v, Go's alarm
// panics without printing a running test's log, so a cleanup that outlasted the margin would
// otherwise take the message with it. The text is one line: a newline in what or in the reading is
// written as \n.
package testwait

import (
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
)

// Poll is how often a condition is asked. It is short enough that a test which passes in
// milliseconds still does, and long enough that a waiting test costs a loaded machine little.
const Poll = 10 * time.Millisecond

// Eventually waits until condition holds, asking every Poll. Its one bound is the test binary's
// deadline less margin, as Until's is, and its failure names what it waited for: it is Until for
// a condition with no reading worth printing.
func Eventually(t *testing.T, what string, condition func() bool) {
	t.Helper()
	end, bounded := giveUpAt(t)
	if !asked(end, bounded, Poll, condition) {
		gaveUp(t, what, nil)
	}
}

// Bounded is bound, or what is left of the test binary's own deadline less margin when that is
// sooner, so a -timeout smaller than bound still reports the wait rather than the binary's panic.
// A wait with a span or a margin of its own takes those; Eventually, Until and Receive have none.
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

// margin is how long before the test binary's deadline Eventually, Until and Receive give up: the
// time the failing test's cleanups and the failure itself have to run in before Go's alarm panics
// the binary.
const margin = 30 * time.Second

// Until waits until read reports true, asking every Poll, and returns what it read then. Its one
// bound is the test binary's deadline less margin, and its failure prints the last value read.
func Until[T any](t *testing.T, what string, read func() (T, bool)) T {
	t.Helper()
	return UntilPolling(t, Poll, what, read)
}

// UntilPolling is Until asking every poll, for a reading too costly to ask a hundred times a
// second (Holds).
func UntilPolling[T any](t *testing.T, poll time.Duration, what string, read func() (T, bool)) T {
	t.Helper()
	var value T
	end, bounded := giveUpAt(t)
	holds := func() bool {
		var ok bool
		value, ok = read()
		return ok
	}
	if !asked(end, bounded, poll, holds) {
		gaveUp(t, what, func() string { return fmt.Sprintf("%+v", value) })
	}
	return value
}

// Receive waits for one receive from ch (a value, or the close of ch, which gives the zero value).
// Its one bound is the test binary's deadline less margin.
func Receive[T any](t *testing.T, what string, ch <-chan T) T {
	t.Helper()
	expired, stop := expiry(t)
	defer stop()
	select {
	case value := <-ch:
		return value
	case <-expired:
	}
	// The margin has passed: ask once more, so a value that came beside the timer is taken.
	select {
	case value := <-ch:
		return value
	default:
	}
	gaveUp(t, what, nil)
	var zero T
	return zero
}

// asked asks holds every poll until it reports true, and reports whether it did. Once end has
// passed a false answer ends the asking, so the question is asked once after end; with no end
// (bounded false) it asks until holds reports true.
func asked(end time.Time, bounded bool, poll time.Duration, holds func() bool) bool {
	for !holds() {
		if bounded && !time.Now().Before(end) {
			return false
		}
		time.Sleep(poll)
	}
	return true
}

// expiry is a channel that receives once the moment giveUpAt names for t has passed, nil (which
// never receives) under -timeout 0, and the function that releases its timer.
func expiry(t *testing.T) (<-chan time.Time, func() bool) {
	end, bounded := giveUpAt(t)
	if !bounded {
		return nil, func() bool { return false }
	}
	timer := time.NewTimer(time.Until(end))
	return timer.C, timer.Stop
}

// giveUpAt is the moment Eventually, Until and Receive give up in t: the test binary's deadline
// less margin, and false when the binary has no deadline (-timeout 0).
func giveUpAt(t *testing.T) (time.Time, bool) {
	deadline, bounded := t.Deadline()
	if !bounded {
		return time.Time{}, false
	}
	return deadline.Add(-margin), true
}

// oneLine writes a newline or a carriage return as its escape, so a failure text is one line.
var oneLine = strings.NewReplacer("\n", `\n`, "\r", `\r`)

// gaveUp ends a wait in t that the margin ended: it writes the failure's text to stderr at once,
// in one write, then fails t with the same text. reading is the wait's last reading, or nil for a
// wait that has none.
func gaveUp(t *testing.T, what string, reading func() string) {
	t.Helper()
	text := fmt.Sprintf("testwait: %s: gave up waiting for %s %s before the test binary's deadline", t.Name(), what, margin)
	if reading != nil {
		text += "; last read: " + reading()
	}
	text = oneLine.Replace(text)
	_, _ = os.Stderr.WriteString(text + "\n")
	t.Fatalf("%s", text)
}
