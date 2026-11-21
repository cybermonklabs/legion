package testwait

import (
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// remainingChildEnv tells the re-executed test binary to run TestRemainingNoDeadlineChild, which
// skips in an ordinary run.
const remainingChildEnv = "TESTWAIT_REMAINING_CHILD"

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
	if now := time.Now(); !now.Add(got).After(now) {
		t.Fatalf("now plus Remaining's %s is %s, not after now %s", got, now.Add(got), now)
	}
}
