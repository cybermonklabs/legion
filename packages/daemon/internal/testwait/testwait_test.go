package testwait

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

// receiveChildEnv marks the child run of TestReceiveEndsOnTheValueOrThirtySecondsBeforeTheDeadline
// that waits on a channel nothing sends to.
const receiveChildEnv = "TESTWAIT_RECEIVE_CHILD"

// Receive returns what the channel yields, the zero value of a closed one, and otherwise waits
// until 30 s before the test binary's deadline, then fails naming what it waited for, before the
// binary's own alarm. The child run waits under a -timeout of 31 s.
func TestReceiveEndsOnTheValueOrThirtySecondsBeforeTheDeadline(t *testing.T) {
	if os.Getenv(receiveChildEnv) == "1" {
		Receive(t, "a value nothing sends", make(chan int))
		return
	}
	sent := make(chan int, 1)
	sent <- 7
	if got := Receive(t, "the value sent", sent); got != 7 {
		t.Fatalf("Receive = %d, want the 7 sent", got)
	}
	closed := make(chan int)
	close(closed)
	if got := Receive(t, "the close", closed); got != 0 {
		t.Fatalf("Receive of a closed channel = %d, want 0", got)
	}

	child := exec.Command(os.Args[0], "-test.run=^TestReceiveEndsOnTheValueOrThirtySecondsBeforeTheDeadline$", "-test.count=1", "-test.timeout=31s")
	child.Env = append(os.Environ(), receiveChildEnv+"=1")
	out, err := child.CombinedOutput()
	const want = "testwait: TestReceiveEndsOnTheValueOrThirtySecondsBeforeTheDeadline: gave up waiting for a value nothing sends 30s before the test binary's deadline"
	exit, failed := err.(*exec.ExitError)
	switch {
	case !failed || exit.ExitCode() != 1:
		t.Fatalf("the child run = %v, want exit status 1; its output:\n%s", err, out)
	case strings.Contains(string(out), "panic: test timed out"):
		t.Fatalf("the child run ran into the binary's deadline, want the wait's own failure first; its output:\n%s", out)
	case strings.Count(string(out), want) != 2:
		t.Fatalf("the child run's output holds %q %d times, want twice (stderr at once, then the failure); its output:\n%s", want, strings.Count(string(out), want), out)
	}
}
