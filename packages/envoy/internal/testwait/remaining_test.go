package testwait

import (
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// spareEnv names, to a child of the spent-deadline rows (TestSpentDeadlineChild), the spare its
// wait asks Remaining for.
const spareEnv = "TESTWAIT_SPARE"

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
// longer than the wait's spare. The child's -timeout is what this test's own deadline leaves less
// the margin (childTimeout), and its wait asks for all but a second of it, so the wait finds the
// deadline spent a second after the child began its tests, whenever that is. The child ran under
// -timeout 60s with a spare of 59 s before, so a child stalled 59 s after its wait failed ran into
// its alarm on a loaded machine (INFRA-3904).
func TestASpentDeadlineNamesWhatRanBeforeTheWait(t *testing.T) {
	timeout := childTimeout(t)
	spare := timeout - time.Second
	failure := spentChild(t, timeout, spare)
	for _, part := range []string{
		"testwait: TestSpentDeadlineChild/waits: the test binary's deadline leaves ",
		" not more than the " + spare.String() + " this wait must leave its test: the binary began its tests ",
		" ago under -timeout " + timeout.String() + ", and what ran before this wait (an earlier test, or this test's own earlier waits) used up that deadline",
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
// raise it. The child's -timeout is childTimeout, and its wait asks for a minute more, so the
// child's alarm is as far off as this test's own deadline lets it be. The child ran under
// -timeout 30s before, so a child stalled 30 s after its wait failed ran into its alarm on a loaded
// machine (INFRA-3904).
func TestATimeoutNoLongerThanTheSpareAsksForALargerOne(t *testing.T) {
	timeout := childTimeout(t)
	spare := timeout + time.Minute
	failure := spentChild(t, timeout, spare)
	want := "testwait: TestSpentDeadlineChild/waits: the test binary's -timeout is " + timeout.String() + ", not more than the " + spare.String() + " this wait must leave its test before the deadline, so no test has room to wait: raise -timeout"
	if failure != want {
		t.Fatalf("the wait under -timeout %s failed with %q, want %q", timeout, failure, want)
	}
}

// Left fails nothing: with no more than the spare left it returns the line Remaining fails with,
// as an error, and the test goes on unfailed. A spare of a day is more than any -timeout a run of
// this package takes, so the line asks for a larger -timeout; under -timeout 0 there is no line.
func TestLeftReturnsTheLineAndFailsNothing(t *testing.T) {
	timeout, known := binaryTimeout()
	left, err := Left(t, 24*time.Hour)
	if _, bounded := t.Deadline(); !bounded {
		if left != unbounded || err != nil {
			t.Fatalf("under -timeout 0 Left is %s, %v, want %s and no error", left, err, unbounded)
		}
		return
	}
	want := "testwait: TestLeftReturnsTheLineAndFailsNothing: the test binary's -timeout is " + timeout.String() + ", not more than the 24h0m0s this wait must leave its test before the deadline, so no test has room to wait: raise -timeout"
	if !known || left != 0 || err == nil || err.Error() != want {
		t.Fatalf("Left with a day's spare is %s, %v (-timeout %s read: %t), want 0 and %q", left, err, timeout, known, want)
	}
	if t.Failed() {
		t.Fatal("Left failed the test, though it only returns the line")
	}
}

// spentChild runs TestSpentDeadlineChild under -timeout timeout, its wait asking for spare, and
// returns the one failure that wait wrote to stderr, failing t unless the child failed that way, by
// that wait and before Go's alarm.
func spentChild(t *testing.T, timeout, spare time.Duration) string {
	t.Helper()
	child := exec.Command(os.Args[0], "-test.run=^TestSpentDeadlineChild$", "-test.timeout="+timeout.String(), "-test.v")
	child.Env = append(os.Environ(), childEnv+"=1", spareEnv+"="+spare.String())
	var stdout, stderr strings.Builder
	child.Stdout, child.Stderr = &stdout, &stderr
	if err := child.Run(); err == nil {
		t.Fatalf("the child under -timeout %s was meant to fail; it passed:\n%s\nstderr:\n%s", timeout, stdout.String(), stderr.String())
	}
	if strings.Contains(stdout.String()+stderr.String(), "panic: test timed out") {
		t.Fatalf("the child under -timeout %s ran into its deadline rather than failing its wait:\n%s\nstderr:\n%s", timeout, stdout.String(), stderr.String())
	}
	requireNoRace(t, stdout.String(), stderr.String())
	var failures []string
	for line := range strings.Lines(stderr.String()) {
		if strings.HasPrefix(line, "testwait: TestSpentDeadlineChild/waits: ") {
			failures = append(failures, strings.TrimSuffix(line, "\n"))
		}
	}
	if len(failures) != 1 || !strings.Contains(stdout.String(), failures[0]+"\n") || !strings.Contains(stdout.String(), "--- FAIL: TestSpentDeadlineChild/waits (") {
		t.Fatalf("the child under -timeout %s did not fail its waiting row with one testwait line on stderr and the same failure:\n%s\nstderr:\n%s", timeout, stdout.String(), stderr.String())
	}
	return failures[0]
}

// TestSpentDeadlineChild is the child of the spent-deadline rows above. Its first row runs until
// the binary's deadline is the spare its parent names (spareEnv) away or nearer, which with a spare
// of all but a second of its -timeout is about a second and with a spare longer than its -timeout is
// at once: a fact of the deadline, not a span. Its second row then asks Remaining for that spare and
// fails. It runs only when a parent re-executes this binary.
func TestSpentDeadlineChild(t *testing.T) {
	if os.Getenv(childEnv) == "" {
		t.Skip(childEnv + " is not set: this test is the child of the spent-deadline rows")
	}
	deadline, bounded := t.Deadline()
	if !bounded {
		t.Fatal("the child runs under a -timeout its parent gives, and has no deadline")
	}
	spare, err := time.ParseDuration(os.Getenv(spareEnv))
	if err != nil {
		t.Fatalf("the child reads the spare its parent names in %s: %v", spareEnv, err)
	}
	t.Run("burns", func(t *testing.T) {
		for time.Until(deadline) > spare {
			time.Sleep(Poll)
		}
	})
	t.Run("waits", func(t *testing.T) {
		Remaining(t, spare)
		t.Error("Remaining returned, though the deadline it was asked about leaves no more than its spare")
	})
}
