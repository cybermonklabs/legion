package testwait

import (
	"testing"
	"time"
)

// giveUpMargin is how long before the test binary's deadline a wait bounded by Remaining gives
// up: margin, the time the failing test's cleanups and its failure have to run in before Go's
// alarm panics the binary, the one Eventually and Until leave too.
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

// Remaining is how long a wait that begins now in t may last: the test binary's deadline, the one
// go test's -timeout sets, less giveUpMargin, less now; and unboundedRemaining under -timeout 0.
// A wait bounded by it ends on its fact or gives up 30 s before the deadline, never at a fixed
// span, so a fact that is merely slow on a loaded machine still passes, and a wait that gives up
// still leaves its test the margin to fail in and run its cleanups. With giveUpMargin or less
// left there is no span a wait could run on, so Remaining fails t there, on one line naming the
// deadline and the margin, and returns nothing; it prints the goroutine stacks after it, as a wait
// begun past the margin does (stacksFor). Otherwise the span it gives ends at the deadline less
// giveUpMargin, which is watched (watchBound): a test that fails after its wait gave up there
// prints the goroutine stacks as that moment came. It calls t.FailNow, so it must be called from
// the goroutine running the test.
func Remaining(t *testing.T) time.Duration {
	t.Helper()
	deadline, ok := deadlineOf(t)
	if !ok {
		return unboundedRemaining
	}
	left := time.Until(deadline)
	if left <= giveUpMargin {
		stacks := stacksFor(t.Name(), true)
		t.Errorf("testwait.Remaining: the test binary's deadline %s is %s away, less than the %s margin a wait leaves its test to fail in and run its cleanups",
			deadline.Format(time.RFC3339Nano), left.Round(time.Millisecond), giveUpMargin)
		printStacks(t, stacks)
		t.FailNow()
	}
	watchBound(t, "Remaining", deadline.Add(-giveUpMargin), false)
	return left - giveUpMargin
}
