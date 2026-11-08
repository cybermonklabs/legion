// Package testwait holds the bounded wait the daemon's tests share. A test that drives real
// processes, a real database or a real socket waits for something it cannot make happen on its
// own — a delivery, the transaction that answers it — and the wait is bounded by the test
// binary's own deadline rather than a fixed span: on a loaded machine a step that is merely slow
// is not a failure, and a condition that never holds still fails, naming what it waited for.
package testwait

import (
	"testing"
	"time"
)

// poll is how often a condition is asked. It is short enough that a test which passes in
// milliseconds still does, and long enough that a waiting test costs a loaded machine little.
const poll = 10 * time.Millisecond

// Eventually waits for condition, failing the test with what it waited for when the deadline
// passes. The deadline is a minute, or the test binary's own less a second when that is sooner,
// so a `-timeout` smaller than a minute still reports the wait rather than the binary's panic.
func Eventually(t *testing.T, what string, condition func() bool) {
	t.Helper()
	EventuallyWithin(t, time.Minute, what, condition)
}

// EventuallyWithin is Eventually on a longer leash, for a condition a minute is the wrong bound
// for. Two kinds take it, and a test that takes it names which in what: a condition that is many
// facts rather than one, where a wave of forty claims settles forty rows through the daemon's own
// pacing and a machine running the rest of this package beside it is legitimately slower than a
// minute; and a condition another process reaches on its own clock, where the idle rig waits for
// a pod to come back. Everything else takes Eventually, whose minute is the package's standard
// and whose expiry is a real failure.
//
// The bound is still cut to the test binary's own deadline less a second, so a `-timeout`
// smaller than it still reports the wait rather than the binary's panic.
func EventuallyWithin(t *testing.T, bound time.Duration, what string, condition func() bool) {
	t.Helper()
	EventuallyEvery(t, bound, poll, what, condition)
}

// EventuallyEvery is EventuallyWithin at a pace of its own, for a condition whose own reading
// costs the machine something: the daemon's whole state, a listener's HTTP call, a server's
// process. Asked a hundred times a second beside the rest of this package, such a read competes
// with the work it is waiting for and can keep it from finishing — the wait then fails on its
// own weight rather than on its subject. A test takes this only where the read is the cost, and
// names the pace in its comment; everything else takes Eventually, whose 10 ms is right for a
// read of one row or one field.
func EventuallyEvery(t *testing.T, bound, every time.Duration, what string, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(bound)
	if testDeadline, ok := t.Deadline(); ok && testDeadline.Add(-time.Second).Before(deadline) {
		deadline = testDeadline.Add(-time.Second)
	}
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(every)
	}
	t.Fatalf("timed out waiting for %s", what)
}
