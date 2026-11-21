package testwait

import (
	"testing"
	"time"
)

// giveUpMargin is how long before the test binary's deadline a wait bounded by Remaining gives
// up: the time the failing test's cleanups and its failure have to run in before Go's alarm
// panics the binary.
const giveUpMargin = 30 * time.Second

// unboundedRemaining is Remaining's answer when the test binary has no deadline (-timeout 0):
// 1 << 62 ns, about 146 years, a span no wait reaches, which time.Now().Add takes without
// overflowing, as a read deadline or a timer built from it needs.
const unboundedRemaining = time.Duration(1 << 62)

// Remaining is how long a wait that begins now in t may last: the test binary's deadline, the one
// go test's -timeout sets, less giveUpMargin, less now; and unboundedRemaining under -timeout 0.
// A wait bounded by it ends on its fact or gives up 30 s before the deadline, never at a fixed
// span, so a fact that is merely slow on a loaded machine still passes, and a wait that gives up
// still leaves its test the margin to fail in and run its cleanups. With giveUpMargin or less
// left there is no span a wait could run on, so Remaining fails t there, on one line naming the
// deadline and the margin, and returns nothing. It calls t.Fatalf, so it must be called from the
// goroutine running the test.
func Remaining(t *testing.T) time.Duration {
	t.Helper()
	deadline, ok := t.Deadline()
	if !ok {
		return unboundedRemaining
	}
	left := time.Until(deadline)
	if left <= giveUpMargin {
		t.Fatalf("testwait.Remaining: the test binary's deadline %s is %s away, less than the %s margin a wait leaves its test to fail in and run its cleanups",
			deadline.Format(time.RFC3339Nano), left.Round(time.Millisecond), giveUpMargin)
	}
	return left - giveUpMargin
}
