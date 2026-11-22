package testwait

import (
	"bytes"
	"os"
	"os/exec"
	"slices"
	"strings"
	"testing"
	"time"
)

// childEnv tells the re-executed test binary to run the helpers below, which skip in an ordinary
// run. One gives up on purpose under a short -timeout and the other waits under -timeout 0, and
// the parent names each flag, so each runs in a binary of its own.
const childEnv = "TESTWAIT_CHILD"

// childTimeout is the -timeout the parent gives the child that gives up: its waits give up 5 s
// into the run, 30 s before its deadline.
const childTimeout = "35s"

// A wait whose fact is already there returns from its first question: Until asks once and never
// sleeps at the pace first, and Receive takes a value already sent.
func TestAWaitThatHoldsReturnsAtOnce(t *testing.T) {
	asks := 0
	if got := Until(t, "a reading that holds", func() (string, bool) { asks++; return "the reading", true }); got != "the reading" {
		t.Fatalf("Until returned %q, want the reading that held", got)
	}
	if asks != 1 {
		t.Fatalf("Until asked a reading that holds %d times, want 1", asks)
	}
	sent := make(chan int, 1)
	sent <- 7
	if got := Receive(t, "a value already sent", sent); got != 7 {
		t.Fatalf("Receive returned %d, want the 7 sent", got)
	}
}

// Until returns the reading that held, not one it read before it.
func TestUntilReturnsTheReadingThatHeld(t *testing.T) {
	asks := 0
	got := Until(t, "the third reading", func() (int, bool) { asks++; return asks, asks == 3 })
	if got != 3 {
		t.Fatalf("Until returned %d, want the third reading, the one that held", got)
	}
}

// UntilPolling asks at the caller's pace: two false readings before the one that holds cost it at
// least two of its polls. Sleep never ends early, so the bound below is a floor no load can push
// the row under.
func TestUntilPollingAsksAtItsPace(t *testing.T) {
	const poll = 50 * time.Millisecond
	asks := 0
	started := time.Now()
	got := UntilPolling(t, poll, "the third reading", func() (int, bool) { asks++; return asks, asks == 3 })
	if elapsed := time.Since(started); got != 3 || elapsed < 2*poll {
		t.Fatalf("UntilPolling returned %d after %s, want the third reading after at least two polls of %s", got, elapsed, poll)
	}
}

// Receive returns a value a sender sends later, and returns on a close with the zero value.
func TestReceiveReturnsASentValueAndAClose(t *testing.T) {
	sent := make(chan string)
	go func() { sent <- "sent later" }()
	if got := Receive(t, "a value a sender sends later", sent); got != "sent later" {
		t.Fatalf("Receive returned %q, want the value sent", got)
	}
	closed := make(chan string)
	close(closed)
	if got := Receive(t, "the close of a channel", closed); got != "" {
		t.Fatalf("Receive on a closed channel returned %q, want the zero value", got)
	}
}

// Until and Receive give up the margin before the test binary's deadline, which the test names.
func TestAWaitGivesUpTheMarginBeforeTheDeadline(t *testing.T) {
	deadline, bounded := t.Deadline()
	end, ok := giveUpAt(t)
	if ok != bounded || ok && !end.Equal(deadline.Add(-margin)) {
		t.Fatalf("a wait in a test whose deadline is %v (%t) gives up at %v (%t), want %s before it", deadline, bounded, end, ok, margin)
	}
}

// Under a short -timeout Until and Receive give up 30 s before the deadline, before Go's alarm:
// each writes its line to stderr and fails the test with the same text, which carries Until's last
// reading.
func TestAWaitGivesUpWithItsLineOnStderrAndTheSameFailure(t *testing.T) {
	child := exec.Command(os.Args[0], "-test.run=^TestTheWaitsGiveUpChild$", "-test.timeout="+childTimeout, "-test.v")
	child.Env = append(os.Environ(), childEnv+"=1")
	var stdout, stderr bytes.Buffer
	child.Stdout, child.Stderr = &stdout, &stderr
	if err := child.Run(); err == nil {
		t.Fatalf("the child was meant to fail; it passed:\n%s\nstderr:\n%s", &stdout, &stderr)
	}
	if strings.Contains(stdout.String(), "panic: test timed out") || strings.Contains(stderr.String(), "panic: test timed out") {
		t.Fatalf("the child's waits ran into the binary's deadline rather than giving up before it:\n%s\nstderr:\n%s", &stdout, &stderr)
	}
	name := "TestTheWaitsGiveUpChild/"
	ending := " 30s before the test binary's deadline"
	want := []string{
		"testwait: " + name + "until: gave up waiting for a reading that never holds" + ending + `; last read: a later\nreading`,
		"testwait: " + name + "receive: gave up waiting for a send no one makes" + ending,
	}
	written := strings.Split(stderr.String(), "\n")
	for _, line := range want {
		if !slices.Contains(written, line) {
			t.Errorf("the child wrote no stderr line %q; its stderr:\n%s", line, &stderr)
		}
		if !strings.Contains(stdout.String(), line+"\n") {
			t.Errorf("the child failed no test with %q; its output:\n%s", line, &stdout)
		}
	}
}

// Under -timeout 0 there is no deadline: Until asks until its reading holds, however many false
// readings come first, and Receive waits until a value comes.
func TestUnderNoTimeoutAWaitHasNoEnd(t *testing.T) {
	child := exec.Command(os.Args[0], "-test.run=^TestNoDeadlineChild$", "-test.timeout=0", "-test.v")
	child.Env = append(os.Environ(), childEnv+"=1")
	output, err := child.CombinedOutput()
	if err != nil || !strings.Contains(string(output), "--- PASS: TestNoDeadlineChild (") {
		t.Fatalf("under -timeout 0 the child did not pass: %v\n%s", err, output)
	}
}

// TestTheWaitsGiveUpChild is the child of TestAWaitGivesUpWithItsLineOnStderrAndTheSameFailure,
// run under -timeout 35s: its first wait gives up 5 s in, and the later one starts past the margin
// and asks once. It runs only when the parent re-executes this binary.
func TestTheWaitsGiveUpChild(t *testing.T) {
	if os.Getenv(childEnv) == "" {
		t.Skip(childEnv + " is not set: this test is the child of TestAWaitGivesUpWithItsLineOnStderrAndTheSameFailure")
	}
	t.Run("until", func(t *testing.T) {
		asks := 0
		Until(t, "a reading that never holds", func() (string, bool) {
			asks++
			if asks == 1 {
				return "the first reading", false
			}
			return "a later\nreading", false
		})
	})
	t.Run("receive", func(t *testing.T) {
		Receive(t, "a send no one makes", make(chan int))
	})
}

// TestNoDeadlineChild is the child of TestUnderNoTimeoutAWaitHasNoEnd, run under -timeout 0: Until
// reads false twice and true the third time, and Receive's value is sent 100 ms after it begins.
// A wait that gave up on a false reading or on an empty channel fails it. The 100 ms decides no
// verdict here, since the wait has no end; it keeps the channel empty when the wait begins, so a
// Receive that gave up at once would find nothing. It runs only when the parent re-executes this
// binary.
func TestNoDeadlineChild(t *testing.T) {
	if os.Getenv(childEnv) == "" {
		t.Skip(childEnv + " is not set: this test is the child of TestUnderNoTimeoutAWaitHasNoEnd")
	}
	asks := 0
	if got := Until(t, "the third reading", func() (int, bool) { asks++; return asks, asks == 3 }); got != 3 || asks != 3 {
		t.Errorf("under -timeout 0 Until returned %d after %d readings, want the third reading after 3", got, asks)
	}
	sent := make(chan string)
	go func() {
		time.Sleep(10 * Poll)
		sent <- "sent later"
	}()
	if got := Receive(t, "a value sent 100 ms after the wait began", sent); got != "sent later" {
		t.Errorf("under -timeout 0 Receive returned %q, want the value sent", got)
	}
}
