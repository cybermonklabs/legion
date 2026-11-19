package testwait

import (
	"testing"
	"time"
)

// This file holds the bounded waits main had before the one wait of testwait.go, with main's
// bodies, for the tests of the Envoy module that have not moved to that wait yet. No new caller
// takes them.

// Poll is how often a condition is asked.
// A2's callers still use this; A2 deletes it with its last caller.
const Poll = pace

// Bounded is bound, or what is left of the test binary's own deadline less margin when that is
// sooner, so a -timeout smaller than bound still reports the wait rather than the binary's panic.
// A2's callers still use this; A2 deletes it with its last caller.
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
// passes.
// A2's callers still use this; A2 deletes it with its last caller.
func WithinPolling(t *testing.T, bound, poll time.Duration, what string, condition func() bool) {
	t.Helper()
	if waitUntil(time.Now().Add(bound), poll, condition) {
		return
	}
	t.Fatalf("timed out waiting for %s", what)
}

// Holds reports whether condition held before bound passed, asking it every poll, and fails
// nothing itself.
// A2's callers still use this; A2 deletes it with its last caller.
func Holds(bound, poll time.Duration, condition func() bool) bool {
	return waitUntil(time.Now().Add(bound), poll, condition)
}

// waitUntil asks condition every poll until it holds or deadline passes, reporting which. It
// asks once more after the deadline, so a condition that came true during the last sleep is not
// reported false.
// A2's callers still use this, through the three above; A2 deletes it with its last caller.
func waitUntil(deadline time.Time, poll time.Duration, condition func() bool) bool {
	for time.Now().Before(deadline) {
		if condition() {
			return true
		}
		time.Sleep(poll)
	}
	return condition()
}
