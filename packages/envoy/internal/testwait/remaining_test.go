package testwait

import (
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// spentSpare is the spare the children below ask Remaining for: more than the deadline a 30 s
// -timeout gives, and a second less than a minute's.
const spentSpare = 59 * time.Second

// Remaining is the test binary's deadline less the spare, read as the call reads it: no earlier
// than the deadline less the spare once the call has returned, and no later than it was before the
// call began.
func TestRemainingIsTheDeadlineLessTheSpare(t *testing.T) {
	deadline, bounded := t.Deadline()
	if !bounded {
		if got := Remaining(t, time.Second); got != unbounded {
			t.Fatalf("under -timeout 0 Remaining is %s, want %s", got, unbounded)
		}
		return
	}
	spare := time.Second
	before := time.Until(deadline) - spare
	got := Remaining(t, spare)
	after := time.Until(deadline) - spare
	if got > before || got < after {
		t.Fatalf("Remaining is %s, want between %s and %s, the deadline less %s read after and before the call", got, after, before, spare)
	}
}

// A wait that finds the binary's deadline used up by the tests that ran before it fails by saying
// so: how long the binary ran its tests under which -timeout, and that what ran before used it up.
// It does not call the run misconfigured or ask for a larger -timeout, since the -timeout was
// longer than the wait's spare.
func TestASpentDeadlineNamesWhatRanBeforeTheWait(t *testing.T) {
	failure := spentChild(t, "60s")
	for _, part := range []string{
		"testwait.Remaining: the test binary's deadline leaves ",
		" not more than the 59s this wait must leave its test: the binary began its tests ",
		" ago under -timeout 1m0s, and what ran before this wait (an earlier test, or this test's own earlier waits) used up that deadline",
	} {
		if !strings.Contains(failure, part) {
			t.Errorf("the spent wait's failure %q does not say %q", failure, part)
		}
	}
	for _, wrong := range []string{"misconfigured", "raise -timeout"} {
		if strings.Contains(failure, wrong) {
			t.Errorf("the spent wait's failure %q says %q, though the binary's -timeout was longer than the wait's spare", failure, wrong)
		}
	}
}

// A -timeout no longer than the wait's spare leaves no test room to wait, and the failure says to
// raise it.
func TestATimeoutNoLongerThanTheSpareAsksForALargerOne(t *testing.T) {
	failure := spentChild(t, "30s")
	want := "testwait.Remaining: the test binary's -timeout is 30s, not more than the 59s this wait must leave its test before the deadline, so no test has room to wait: raise -timeout"
	if failure != want {
		t.Fatalf("the wait under -timeout 30s failed with %q, want %q", failure, want)
	}
}

// spentChild runs TestSpentDeadlineChild under -timeout timeout and returns the one failure its
// wait wrote to stderr, failing t unless the child failed that way, by that wait and before Go's
// alarm.
func spentChild(t *testing.T, timeout string) string {
	t.Helper()
	child := exec.Command(os.Args[0], "-test.run=^TestSpentDeadlineChild$", "-test.timeout="+timeout, "-test.v")
	child.Env = append(os.Environ(), childEnv+"=1")
	var stdout, stderr strings.Builder
	child.Stdout, child.Stderr = &stdout, &stderr
	if err := child.Run(); err == nil {
		t.Fatalf("the child under -timeout %s was meant to fail; it passed:\n%s\nstderr:\n%s", timeout, stdout.String(), stderr.String())
	}
	if strings.Contains(stdout.String()+stderr.String(), "panic: test timed out") {
		t.Fatalf("the child under -timeout %s ran into its deadline rather than failing its wait:\n%s\nstderr:\n%s", timeout, stdout.String(), stderr.String())
	}
	var failures []string
	for line := range strings.Lines(stderr.String()) {
		if strings.HasPrefix(line, "testwait.Remaining: ") {
			failures = append(failures, strings.TrimSuffix(line, "\n"))
		}
	}
	if len(failures) != 1 || !strings.Contains(stdout.String(), failures[0]+"\n") || !strings.Contains(stdout.String(), "--- FAIL: TestSpentDeadlineChild/waits (") {
		t.Fatalf("the child under -timeout %s did not fail its waiting row with one testwait.Remaining line on stderr and the same failure:\n%s\nstderr:\n%s", timeout, stdout.String(), stderr.String())
	}
	return failures[0]
}

// TestSpentDeadlineChild is the child of the spent-deadline rows above. Its first row runs until
// the binary's deadline is spentSpare away or nearer, which under -timeout 60s is about a second
// and under -timeout 30s is at once: a fact of the deadline, not a span. Its second row then asks
// Remaining for spentSpare and fails. It runs only when a parent re-executes this binary.
func TestSpentDeadlineChild(t *testing.T) {
	if os.Getenv(childEnv) == "" {
		t.Skip(childEnv + " is not set: this test is the child of the spent-deadline rows")
	}
	deadline, bounded := t.Deadline()
	if !bounded {
		t.Fatal("the child runs under a -timeout its parent gives, and has no deadline")
	}
	t.Run("burns", func(t *testing.T) {
		for time.Until(deadline) > spentSpare {
			time.Sleep(Poll)
		}
	})
	t.Run("waits", func(t *testing.T) {
		Remaining(t, spentSpare)
		t.Error("Remaining returned, though the deadline it was asked about leaves no more than its spare")
	})
}
