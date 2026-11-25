package testwait

import (
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// remainingChildEnv tells the re-executed test binary to run the child tests below, which skip in
// an ordinary run. The parent names the child it wants with -test.run.
const remainingChildEnv = "TESTWAIT_REMAINING_CHILD"

// remainingChildTimeout is the -test.timeout of the child that starts within the margin: 10 s,
// less than giveUpMargin, so its first call of Remaining has less than the margin left.
const remainingChildTimeout = "10s"

// Remaining is the time until the test binary's deadline less giveUpMargin: read just before the
// call that time is no less than Remaining's answer, and read just after it no more. The reads and
// the answer come from one monotonic clock, so the bounds need no tolerance.
func TestRemainingIsTheTimeToTheDeadlineLessTheMargin(t *testing.T) {
	deadline, ok := t.Deadline()
	if !ok {
		t.Skip("the binary runs under -timeout 0, so there is no deadline to measure; TestUnderNoTimeoutRemainingHasNoEnd covers that")
	}
	most := time.Until(deadline) - giveUpMargin
	got := Remaining(t)
	least := time.Until(deadline) - giveUpMargin
	if got < least || got > most {
		t.Fatalf("Remaining = %s, want between %s and %s: the time to the deadline %s less %s",
			got, least, most, deadline.Format(time.RFC3339Nano), giveUpMargin)
	}
}

// Under -timeout 0 the test binary has no deadline, and Remaining is a span no wait reaches.
func TestUnderNoTimeoutRemainingHasNoEnd(t *testing.T) {
	child := exec.Command(os.Args[0], "-test.run=^TestRemainingNoDeadlineChild$", "-test.timeout=0", "-test.v")
	child.Env = append(os.Environ(), remainingChildEnv+"=1")
	output, err := child.CombinedOutput()
	if err != nil || !strings.Contains(string(output), "--- PASS: TestRemainingNoDeadlineChild (") {
		t.Fatalf("under -timeout 0 the child did not pass: %v\n%s", err, output)
	}
}

// TestRemainingNoDeadlineChild is the child of TestUnderNoTimeoutRemainingHasNoEnd, run under
// -timeout 0: Remaining is unboundedRemaining, and a deadline that far off is still after now, as
// a read deadline or a timer built from it needs. It runs only when the parent re-executes this
// binary.
func TestRemainingNoDeadlineChild(t *testing.T) {
	if os.Getenv(remainingChildEnv) == "" {
		t.Skip(remainingChildEnv + " is not set: this test is the child of TestUnderNoTimeoutRemainingHasNoEnd")
	}
	if deadline, ok := t.Deadline(); ok {
		t.Fatalf("the child has the deadline %s; it was meant to run under -timeout 0", deadline.Format(time.RFC3339Nano))
	}
	got := Remaining(t)
	if got != unboundedRemaining {
		t.Fatalf("under -timeout 0 Remaining = %s, want %s, a span no wait reaches", got, unboundedRemaining)
	}
	if now := time.Now(); !now.Add(4 * got).After(now) {
		t.Fatalf("now plus four times Remaining's %s is %s, not after now %s", got, now.Add(4*got), now)
	}
}

// unboundedRemaining survives the small factors its callers multiply it by (a consumer's handler
// limit is intake.WindowsPerLimit ack waits): every product up to seven times it is still
// positive. 1 << 62 times 4 wraps an int64 to 0, which is what this row exists to catch.
func TestUnboundedRemainingStaysPositiveTimesASmallFactor(t *testing.T) {
	for factor := time.Duration(1); factor <= 7; factor++ {
		if product := factor * unboundedRemaining; product <= 0 {
			t.Errorf("%d times unboundedRemaining (%d ns) is %s, want a positive span", factor, int64(unboundedRemaining), product)
		}
	}
}

// Called with less than the margin left before the deadline, Remaining fails the test by name, on
// one line naming the deadline and the margin, and returns nothing a wait could run on: the child
// runs under -timeout 10s, and the line it logs after Remaining returns never comes.
func TestRemainingWithinTheMarginFailsTheTestByName(t *testing.T) {
	child := exec.Command(os.Args[0], "-test.run=^TestRemainingWithinTheMarginChild$", "-test.timeout="+remainingChildTimeout, "-test.v")
	child.Env = append(os.Environ(), remainingChildEnv+"=1")
	output, err := child.CombinedOutput()
	text := string(output)
	if err == nil || !strings.Contains(text, "--- FAIL: TestRemainingWithinTheMarginChild (") {
		t.Fatalf("the child under -timeout %s was meant to fail; it did not (%v):\n%s", remainingChildTimeout, err, text)
	}
	if strings.Contains(text, "panic: test timed out") {
		t.Fatalf("the child ran into the binary's deadline rather than failing at once:\n%s", text)
	}
	if strings.Contains(text, "Remaining returned") {
		t.Fatalf("Remaining returned within the margin rather than failing the test:\n%s", text)
	}
	var named string
	for _, line := range strings.Split(text, "\n") {
		if strings.Contains(line, "testwait.Remaining: the test binary's deadline ") {
			named = line
		}
	}
	if named == "" || !strings.Contains(named, "less than the 30s margin") || !strings.Contains(named, "remaining_test.go:") {
		t.Fatalf("the child failed with no line from Remaining naming the deadline and the 30s margin at its caller's line:\n%s", text)
	}
}

// TestRemainingWithinTheMarginChild is the child of TestRemainingWithinTheMarginFailsTheTestByName,
// run under -timeout 10s, so less than the margin is left at once. It runs only when the parent
// re-executes this binary.
func TestRemainingWithinTheMarginChild(t *testing.T) {
	if os.Getenv(remainingChildEnv) == "" {
		t.Skip(remainingChildEnv + " is not set: this test is the child of TestRemainingWithinTheMarginFailsTheTestByName")
	}
	got := Remaining(t)
	t.Logf("Remaining returned %s", got)
}
