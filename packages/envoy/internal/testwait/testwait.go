// Package testwait holds the bounded wait Envoy's tests share. A test that drives real
// processes, a real database or a real socket waits for something it cannot make happen on its
// own — a delivery, the transaction that answers it — and the wait is bounded by the test
// binary's own deadline rather than a fixed span: on a loaded machine a step that is merely slow
// is not a failure, and a condition that never holds still fails, naming what it waited for.
// Eventually is the wait a test usually wants; WithinPolling takes a bound and a pace of its
// own; Holds answers rather than fails, for a caller whose failure text is its own; and Bounded
// is the clamp alone, for a wait that holds the rest itself.
//
// Until and Receive take no bound at all, not even Eventually's minute: each gives up margin (30 s)
// before the test binary's deadline, the one go test's -timeout sets, and at no other moment, so
// only the fact it waits for ends it, and under -timeout 0 it has no end. A wait that begins with
// 30 s or less of the binary's deadline left, as every wait does under a -timeout of 30 s or less,
// has already passed that moment: Until asks once, Receive takes only a value already sent or a
// close, and either fails the test at once when that answer is not there. Until waits until a
// reading holds and returns it, asking every Poll, and its failure prints the last value it read;
// UntilPolling is Until at a pace the caller gives, for a reading that costs something, such as a
// request to a cluster. Receive waits for one receive from a channel. Context is a context whose
// deadline is that same moment, the test binary's deadline less margin, for a call that takes a
// context or a socket whose deadline is the context's. A wait that gives up writes
// "testwait: <test>: gave up waiting for <what> 30s before the test binary's deadline; last read:
// <reading>" to stderr at once, then fails the test with the same text: without -v, Go's alarm
// panics without printing a running test's log, so a cleanup that outlasted the margin would
// otherwise take the message with it. The text is one line: a newline in what or in the reading is
// written as \n. The reading describes the value snapshot its callback handed it; pointers into
// live objects print as addresses, so a callback that needs fields behind one copies them while it
// holds that object's lock. After the failure the test's output carries the goroutine stacks as the
// wait gave up, the dump Go's alarm would print had the wait not given up before it: the waiting
// goroutine's, then those its test started, bounded (goroutineStacks). A wait that began past the
// margin did not wait, so its failure is collateral: it prints a line that says so and names the
// longest completed testwait wait recorded so far, and the stacks only when no earlier give-up of
// the binary has printed them (diagnostics). A wait with a failure text of its own that takes its
// bound from Bounded, Remaining, Left or Context gets the same stacks, read at that bound's end and
// printed when its test fails (watchBound).
package testwait

import (
	"context"
	"fmt"
	"os"
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

// Bounded is bound, or what is left of the test binary's own deadline less spare when that is
// sooner, so a -timeout smaller than bound still reports the wait rather than the binary's panic.
// Eventually is Bounded of a minute and a second; a wait with a span or a spare of its own takes
// those. spare is the caller's, not this package's margin. The bound it gives is watched
// (watchBound): a test that fails after its wait gave up at that bound's end prints the goroutine
// stacks as that end came.
func Bounded(t *testing.T, bound, spare time.Duration) time.Duration {
	if testDeadline, ok := t.Deadline(); ok {
		if left := time.Until(testDeadline) - spare; left < bound {
			watchBound(t, "Bounded", testDeadline.Add(-spare), false, "the test binary's deadline less "+spare.String(), callerSite())
			return left
		}
	}
	watchBound(t, "Bounded", time.Now().Add(bound), true, "the wait's own span of "+bound.String(), callerSite())
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

// Margin is how long before the test binary's deadline Until and Receive give up: the time the
// failing test's cleanups and the failure itself have to run in before Go's alarm panics the
// binary. A bound of another package's that must end before that alarm too (testdb's connect
// timeout) leaves the same.
const Margin = 30 * time.Second

// PastTheRun is a span longer than any run of a test binary: longer than the longest -timeout a
// command of this repository gives one (the API race step's 70 minutes,
// .github/workflows/ci-job-envoy-go-race-api.yaml). A test sets a product's window, interval or
// timeout to it where the row needs that span never to end during the run (a hold that must stay
// live, a ping on a healthy connection, a publish bound the row does not race), so a loaded machine
// cannot end it before the binary's deadline ends the test.
const PastTheRun = 24 * time.Hour

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
	started := time.Now()
	var value T
	end, bounded := giveUpAt(t)
	spent := bounded && !clock.Now().Before(end)
	holds := func() bool {
		var ok bool
		value, ok = read()
		return ok
	}
	if !asked(end, bounded, poll, holds) {
		gaveUp(t, what, started, end, spent, func() string { return describe(value) })
	}
	return value
}

// Receive waits for one receive from ch (a value, or the close of ch, which gives the zero value).
// Its one bound is the test binary's deadline less margin. A Receive begun once that moment has
// passed asks ch once and does not wait: it reads the clock rather than race an expired timer
// against ch, so a value already sent is taken every time, not on the half of the runs where the
// select happens to pick ch.
func Receive[T any](t *testing.T, what string, ch <-chan T) T {
	t.Helper()
	started := time.Now()
	end, bounded := giveUpAt(t)
	spent := bounded && !clock.Now().Before(end)
	if !spent {
		expired, stop := expiry(t)
		defer stop()
		select {
		case value := <-ch:
			return value
		case <-expired:
		}
	}
	// The margin has passed, before the wait began or during it: ask once more, so a value already
	// sent, or one that came beside the timer, is taken.
	select {
	case value := <-ch:
		return value
	default:
	}
	gaveUp(t, what, started, end, spent, nil)
	var zero T
	return zero
}

// Context gives a context whose deadline is the test binary's deadline less margin, cancelled
// when t ends: Receive's end, for a call that takes a context, or a socket whose deadline is the
// context's. Under -timeout 0 it has no deadline; under a -timeout of 30 s or less its deadline
// has already passed when the test starts, so a call under it fails at once, as every wait here
// does. Its deadline is watched (watchBound): a test that fails after a call under it gave up
// prints the goroutine stacks as that deadline came.
func Context(t *testing.T) context.Context {
	end, bounded := giveUpAt(t)
	if !bounded {
		ctx, cancel := context.WithCancel(context.Background())
		t.Cleanup(cancel)
		return ctx
	}
	watchBound(t, "Context", end, false, "the test binary's deadline less "+margin.String(), callerSite())
	ctx, cancel := context.WithDeadline(context.Background(), end)
	t.Cleanup(cancel)
	return ctx
}

// asked asks holds every poll until it reports true, and reports whether it did. Once end has
// passed a false answer ends the asking, so the question is asked once after end; with no end
// (bounded false) it asks until holds reports true.
func asked(end time.Time, bounded bool, poll time.Duration, holds func() bool) bool {
	for !holds() {
		if bounded && !clock.Now().Before(end) {
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
	return clock.Timer(end)
}

// giveUpAt is the moment Until, Receive and Context give up in t: the test binary's deadline less
// margin, and false when the binary has no deadline (-timeout 0).
func giveUpAt(t *testing.T) (time.Time, bool) {
	deadline, bounded := t.Deadline()
	if !bounded {
		return time.Time{}, false
	}
	return deadline.Add(-Margin), true
}

// oneLine writes a newline or a carriage return as its escape, so a failure text is one line.
var oneLine = strings.NewReplacer("\n", `\n`, "\r", `\r`)

// gaveUp ends a wait in t that ended at the test binary's deadline less margin: it writes the
// failure's text to stderr at once, then records the failure, its diagnostics, and its stop in one
// shared tail (fail). reading is the wait's last reading, or nil for a wait that has none.
func gaveUp(t *testing.T, what string, started, end time.Time, spent bool, reading func() string) {
	t.Helper()
	text := fmt.Sprintf("testwait: %s: gave up waiting for %s %s before the test binary's deadline", t.Name(), what, Margin)
	if reading != nil {
		text += "; last read: " + reading()
	}
	text = oneLine.Replace(text)
	_, _ = os.Stderr.WriteString(text + "\n")
	stacks := diagnostics(t, what, started, end, spent, "the test binary's deadline less "+margin.String())
	fail(t, text, stacks)
}
