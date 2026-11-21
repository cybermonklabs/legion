// Package testwait holds the bounded wait the daemon's tests share. A test that drives real
// processes, a real database or a real socket waits for something it cannot make happen on its
// own — a delivery, the transaction that answers it — and the wait is bounded by the test
// binary's own deadline rather than a fixed span: on a loaded machine a step that is merely slow
// is not a failure, and a condition that never holds still fails, naming what it waited for.
// Eventually is the wait a test usually wants; WithinPolling takes a bound and a pace of its
// own; Holds answers rather than fails, for a caller whose failure text is its own; and Bounded
// is the clamp alone, for a wait that holds the rest itself.
package testwait

import (
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

// giveUpMargin is how long before the test binary's deadline a wait bounded by Remaining gives
// up: the time the failing test's cleanups and its failure have to run in before Go's alarm
// panics the binary.
const giveUpMargin = 30 * time.Second

// noDeadline is Remaining's answer when the test binary has no deadline (-timeout 0): about 146
// years, a span no wait reaches, which time.Now().Add takes without overflowing.
const noDeadline = time.Duration(1 << 62)

// Remaining is how long a wait in t may last: the time until the test binary's deadline, the one
// go test's -timeout sets, less giveUpMargin, which is zero or less once that moment has passed;
// and noDeadline under -timeout 0. A wait bounded by it ends on its fact or gives up 30 s before
// the deadline, never at a fixed span, so a fact that is merely slow on a loaded machine still
// passes, and the failing test still has the margin to report in.
func Remaining(t *testing.T) time.Duration {
	deadline, ok := t.Deadline()
	if !ok {
		return noDeadline
	}
	return time.Until(deadline) - giveUpMargin
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
