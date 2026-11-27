package testwait

import (
	"fmt"
	"testing"
	"time"
)

// This file holds the waits and constants main had before the one wait of testwait.go, with
// main's behaviour, for the tests of the Envoy module that have not moved to that wait yet. Their
// callers are tests of other packages (this package's own tests only hold UntilPolling to its pace
// and sleep a Poll): the change that moves a package's tests onto the one wait moves its callers
// off these, and each goes with its last caller.

// Poll is how often a condition is asked.
// Callers not yet moved still use this; it goes with its last caller.
const Poll = pace

// Margin is how long before the test binary's deadline every wait of testwait.go but Eventually
// gives up (margin): the time the failing test's cleanups and the failure itself have to run in
// before Go's alarm panics the binary. A bound of another package's that must end before that alarm
// too (testdb's connect timeout) leaves the same.
// Callers not yet moved still use this; it goes with its last caller.
const Margin = margin

// PastTheRun is a span longer than any run of a test binary: longer than the longest -timeout a
// command of this repository gives one (the API race step's 70 minutes,
// .github/workflows/ci-job-envoy-go-race-api.yaml). A test sets a product's window, interval or
// timeout to it where the row needs that span never to end during the run (a hold that must stay
// live, a ping on a healthy connection, a publish bound the row does not race), so a loaded machine
// cannot end it before the binary's deadline ends the test.
// Callers not yet moved still use this; it goes with its last caller.
const PastTheRun = 24 * time.Hour

// UntilPolling is Until asking every poll rather than at the package's pace, for a reading too
// costly to ask a hundred times a second. Its bound and its failure are Until's: it gives up where
// GiveUpAt says, asking once after that moment, and its failure prints the last value read.
// Callers not yet moved still use this; it goes with its last caller.
func UntilPolling[T any](t testing.TB, poll time.Duration, what string, read func() (T, bool)) T {
	t.Helper()
	var value T
	end, bounded := GiveUpAt(t)
	for {
		var holds bool
		if value, holds = read(); holds {
			return value
		}
		if bounded && !currentClock().Now().Before(end) {
			gaveUp(t, what, func() string { return fmt.Sprintf("%+v", value) })
			return value
		}
		time.Sleep(poll)
	}
}

// Bounded is bound, or what is left of the test binary's own deadline less spare when that is
// sooner, so a -timeout smaller than bound still reports the wait rather than the binary's panic.
// spare is the caller's, not this package's margin.
// Callers not yet moved still use this; it goes with its last caller.
func Bounded(t *testing.T, bound, spare time.Duration) time.Duration {
	if testDeadline, ok := t.Deadline(); ok {
		if left := time.Until(testDeadline) - spare; left < bound {
			bound = left
		}
	}
	return bound
}

// WithinPolling waits for condition under the caller's own bound and its own pace, with no
// clamp to the test binary's deadline, failing the test with what it waited for when the bound
// passes.
// Callers not yet moved still use this; it goes with its last caller.
func WithinPolling(t *testing.T, bound, poll time.Duration, what string, condition func() bool) {
	t.Helper()
	if waitUntil(time.Now().Add(bound), poll, condition) {
		return
	}
	t.Fatalf("timed out waiting for %s", what)
}

// Holds reports whether condition held before bound passed, asking it every poll, and fails
// nothing itself.
// Callers not yet moved still use this; it goes with its last caller.
func Holds(bound, poll time.Duration, condition func() bool) bool {
	return waitUntil(time.Now().Add(bound), poll, condition)
}

// waitUntil asks condition every poll until it holds or deadline passes, reporting which. It
// asks once more after the deadline, so a condition that came true during the last sleep is not
// reported false.
// Callers not yet moved still use this, through the three above; it goes with its last caller.
func waitUntil(deadline time.Time, poll time.Duration, condition func() bool) bool {
	for time.Now().Before(deadline) {
		if condition() {
			return true
		}
		time.Sleep(poll)
	}
	return condition()
}
