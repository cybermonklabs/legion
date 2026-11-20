package testwait

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"strings"
	"testing"
)

// childEnv tells the re-executed test binary to run the children below, which skip in an ordinary
// run. One gives up on purpose under a short -timeout and the other reads the deadline under
// -timeout 0, and the parent names each flag, so each runs in a binary of its own.
const childEnv = "TESTWAIT_CHILD"

// childTimeout is the -timeout the parent gives the child that gives up: its Receive gives up 5 s
// into the run, the margin before its deadline.
const childTimeout = "35s"

// Receive returns a value already sent and one a sender sends later, and returns on a close with
// the zero value.
func TestReceiveReturnsASentValueAndAClose(t *testing.T) {
	ready := make(chan int, 1)
	ready <- 7
	if got := Receive(t, "a value already sent", ready); got != 7 {
		t.Fatalf("Receive returned %d, want the 7 sent", got)
	}
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

// Context's deadline is the test binary's deadline less the margin, where Receive gives up, and
// the context ends with the test that took it.
func TestContextEndsWhereReceiveGivesUpAndWithTheTest(t *testing.T) {
	var taken context.Context
	t.Run("taker", func(t *testing.T) {
		taken = Context(t)
		binary, bounded := t.Deadline()
		deadline, has := taken.Deadline()
		if has != bounded || has && !deadline.Equal(binary.Add(-margin)) {
			t.Fatalf("Context's deadline is %v (%t), want %s before the binary's %v (%t)", deadline, has, margin, binary, bounded)
		}
		if err := taken.Err(); err != nil {
			t.Fatalf("Context ended before its test did: %v", err)
		}
	})
	if err := taken.Err(); !errors.Is(err, context.Canceled) {
		t.Fatalf("Context outlived the test that took it: its error is %v, want context.Canceled", err)
	}
}

// Under a short -timeout Receive gives up the margin before the deadline, before Go's alarm, and
// fails its test naming what it waited for. A Receive that starts once the margin has passed asks
// once: it takes a value already sent, and fails at once on a channel that holds none.
func TestReceiveGivesUpTheMarginBeforeTheDeadline(t *testing.T) {
	child := exec.Command(os.Args[0], "-test.run=^TestReceiveGivesUpChild$", "-test.timeout="+childTimeout, "-test.v")
	child.Env = append(os.Environ(), childEnv+"=1")
	output, err := child.CombinedOutput()
	if err == nil {
		t.Fatalf("the child was meant to fail; it passed:\n%s", output)
	}
	if strings.Contains(string(output), "panic: test timed out") {
		t.Fatalf("the child's Receive ran into the binary's deadline rather than giving up before it:\n%s", output)
	}
	for _, want := range []string{
		"timed out waiting for a send no one makes\n",
		"--- FAIL: TestReceiveGivesUpChild/receive (",
		"--- PASS: TestReceiveGivesUpChild/a_value_past_the_margin (",
		"timed out waiting for a later send no one makes\n",
		"--- FAIL: TestReceiveGivesUpChild/no_value_past_the_margin (",
	} {
		if !strings.Contains(string(output), want) {
			t.Errorf("the child's output holds no %q:\n%s", want, output)
		}
	}
}

// Under -timeout 0 there is no deadline: Receive has no timer to give up on, and Context has no
// deadline.
func TestUnderNoTimeoutReceiveAndContextHaveNoEnd(t *testing.T) {
	child := exec.Command(os.Args[0], "-test.run=^TestNoDeadlineChild$", "-test.timeout=0", "-test.v")
	child.Env = append(os.Environ(), childEnv+"=1")
	output, err := child.CombinedOutput()
	if err != nil || !strings.Contains(string(output), "--- PASS: TestNoDeadlineChild (") {
		t.Fatalf("under -timeout 0 the child did not pass: %v\n%s", err, output)
	}
}

// TestReceiveGivesUpChild is the child of TestReceiveGivesUpTheMarginBeforeTheDeadline, run under
// -timeout 35s: its first Receive gives up 5 s in, and the later ones start past the margin. It
// runs only when the parent re-executes this binary.
func TestReceiveGivesUpChild(t *testing.T) {
	if os.Getenv(childEnv) == "" {
		t.Skip(childEnv + " is not set: this test is the child of TestReceiveGivesUpTheMarginBeforeTheDeadline")
	}
	t.Run("receive", func(t *testing.T) {
		Receive(t, "a send no one makes", make(chan int))
	})
	t.Run("a value past the margin", func(t *testing.T) {
		ready := make(chan int, 1)
		ready <- 7
		if got := Receive(t, "a value already sent", ready); got != 7 {
			t.Fatalf("Receive past the margin returned %d, want the 7 sent", got)
		}
	})
	t.Run("no value past the margin", func(t *testing.T) {
		Receive(t, "a later send no one makes", make(chan int))
	})
}

// TestNoDeadlineChild is the child of TestUnderNoTimeoutReceiveAndContextHaveNoEnd, run under
// -timeout 0. It runs only when the parent re-executes this binary.
func TestNoDeadlineChild(t *testing.T) {
	if os.Getenv(childEnv) == "" {
		t.Skip(childEnv + " is not set: this test is the child of TestUnderNoTimeoutReceiveAndContextHaveNoEnd")
	}
	if end, ok := giveUpAt(t); ok {
		t.Errorf("under -timeout 0 Receive gives up at %v, want never", end)
	}
	expired, stop := expiry(t)
	stop()
	if expired != nil {
		t.Errorf("under -timeout 0 Receive has a timer to give up on, want none")
	}
	if deadline, ok := Context(t).Deadline(); ok {
		t.Errorf("under -timeout 0 Context has the deadline %v, want none", deadline)
	}
}
