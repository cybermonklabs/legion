package testwait

import (
	"testing"
	"time"
)

// giveUpMargin is how long before the test binary's deadline the budget a wait may take ends:
// margin, the time the failing test's cleanups and its failure have to run in before Go's alarm
// panics the binary, the one Eventually and Until leave too. Remaining takes its share of what is
// left before this moment, so a wait of its gives up earlier still; WholeRemaining runs to it.
const giveUpMargin = margin

// unboundedRemaining is Remaining's answer when the test binary has no deadline (-timeout 0):
// 1 << 60 ns, about 36 years, a span no wait reaches. Callers multiply it by small factors (a
// consumer's handler limit is intake.WindowsPerLimit ack waits, intake.HandlerLimitFor), so it
// leaves room in an int64 for up to seven times itself (eight times is 1 << 63, which wraps):
// 1 << 62 times 4 wraps to 0, which made such a limit fall to its floor. Now plus seven times it
// is still a time after now, as a read deadline or a timer built from it needs.
const unboundedRemaining = time.Duration(1 << 60)

// deadlineOf is where Remaining and Receive read the test binary's deadline: t.Deadline. Only the
// children of TestRemainingWithinTheMarginFailsTheTestByName and TestReceiveFailsNamingWhenItGaveUp
// replace it, with a deadline of their own, so each child shows its wait's end while its own alarm
// is what is left of its parent's deadline: no starved child reaches that alarm first.
var deadlineOf = (*testing.T).Deadline

// Remaining is how long a wait that begins now in t may last: a share (Share) of the time to the
// test binary's deadline, the one go test's -timeout sets, less giveUpMargin; and
// unboundedRemaining under -timeout 0. A wait bounded by it ends on its fact or gives up at its
// share, never at a fixed span, so a fact that is merely slow on a loaded machine still passes, a
// wait that gives up leaves its test the margin to fail in and run its cleanups, and a wait whose
// fact never comes leaves the tests behind it their own time to report. A wait that truly needs
// the whole of what is left calls WholeRemaining. With giveUpMargin or less left there is no span
// a wait could run on, so Remaining fails t there, on one line naming the deadline and the margin,
// and returns nothing. It calls t.Fatalf, so it must be called from the goroutine running the test.
func Remaining(t *testing.T) time.Duration {
	t.Helper()
	whole := leftBeforeTheMargin(t, "Remaining")
	if whole == unboundedRemaining {
		return unboundedRemaining
	}
	return shareOf(whole)
}

// WholeRemaining is the whole time to the test binary's deadline less giveUpMargin, and
// unboundedRemaining under -timeout 0: the one named way for a wait to take all of what is left,
// for a bound that must outlast the waits beside it or one handed to a child process whose own
// deadline is set from this test's. Every other deadline-bound wait takes a share (Remaining).
// With giveUpMargin or less left it fails t, as Remaining does.
func WholeRemaining(t *testing.T) time.Duration {
	t.Helper()
	return leftBeforeTheMargin(t, "WholeRemaining")
}

// leftBeforeTheMargin is the time to the test binary's deadline less giveUpMargin, and
// unboundedRemaining under -timeout 0. With giveUpMargin or less left there is no span a wait
// could run on, so it fails t there, naming the function the caller called rather than this one:
// a reader of that line is looking for the call in their own test.
func leftBeforeTheMargin(t *testing.T, called string) time.Duration {
	t.Helper()
	deadline, ok := deadlineOf(t)
	if !ok {
		return unboundedRemaining
	}
	left := time.Until(deadline)
	if left <= giveUpMargin {
		t.Fatalf("testwait.%s: the test binary's deadline %s is %s away, less than the %s margin a wait leaves its test to fail in and run its cleanups",
			called, deadline.Format(time.RFC3339Nano), left.Round(time.Millisecond), giveUpMargin)
	}
	return left - giveUpMargin
}
