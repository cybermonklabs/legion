package testwait

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// A wait that stands in for a product's bound gives up margin before the binary's deadline, never
// sooner than the bound after it began, and never later than the deadline.
func TestABoundEndLeavesTheMarginButNeverLessThanTheBound(t *testing.T) {
	now := time.Date(2026, 11, 22, 16, 0, 0, 0, time.UTC)
	const bound = 4 * time.Second
	for _, tc := range []struct {
		left, want time.Duration
	}{
		{10 * time.Minute, 10*time.Minute - margin},
		{margin + bound + time.Second, bound + time.Second},
		{margin + bound, bound},
		{20 * time.Second, bound},
		{bound, bound},
		{3 * time.Second, 3 * time.Second},
	} {
		if got := BoundEndAt(now.Add(tc.left), now, bound).Sub(now); got != tc.want {
			t.Errorf("with %s left and a bound of %s the wait ends %s from now, want %s", tc.left, bound, got, tc.want)
		}
	}
}

// WithBoundEnd arms BoundEndAt of the test's own deadline, read when it is called; under
// -timeout 0 it arms nothing.
func TestWithBoundEndArmsTheBoundEndOfTheTestsDeadline(t *testing.T) {
	const bound = 4 * time.Second
	before := time.Now()
	ctx, cancel := WithBoundEnd(context.Background(), t, bound)
	after := time.Now()
	defer cancel()
	got, armed := ctx.Deadline()
	deadline, bounded := t.Deadline()
	if !bounded {
		if armed {
			t.Fatalf("under -timeout 0 the context ends at %v, want no end", got)
		}
		return
	}
	if lo, hi := BoundEndAt(deadline, before, bound), BoundEndAt(deadline, after, bound); !armed || got.Before(lo) || got.After(hi) {
		t.Fatalf("the context ends at %v (armed %t), want BoundEndAt of the test's deadline %v, between %v and %v", got, armed, deadline, lo, hi)
	}
}

// BoundArm, given a product's bound, arms what WithBoundEnd arms for it: BoundEndAt of the test's
// deadline and that bound, read when the wait begins. The bound is longer than any test's deadline
// is away, so its floor decides the end (the deadline itself), and an arm that dropped the bound
// would end the wait margin sooner.
func TestBoundArmArmsTheBoundEndOfTheBoundItIsGiven(t *testing.T) {
	const bound = 1000 * time.Hour
	before := time.Now()
	ctx, cancel := BoundArm(t)(context.Background(), bound)
	after := time.Now()
	defer cancel()
	got, armed := ctx.Deadline()
	deadline, bounded := t.Deadline()
	if !bounded {
		if armed {
			t.Fatalf("under -timeout 0 the arm ends at %v, want no end", got)
		}
		return
	}
	if lo, hi := BoundEndAt(deadline, before, bound), BoundEndAt(deadline, after, bound); !armed || got.Before(lo) || got.After(hi) {
		t.Fatalf("the arm ends at %v (armed %t), want BoundEndAt of the test's deadline %v and %s, between %v and %v", got, armed, deadline, bound, lo, hi)
	}
}

// UntilBoundEnd returns the reading that held.
func TestUntilBoundEndReturnsTheReadingThatHeld(t *testing.T) {
	asks := 0
	if got := UntilBoundEnd(t, time.Second, "the third reading", func() (int, bool) { asks++; return asks, asks == 3 }); got != 3 {
		t.Fatalf("UntilBoundEnd returned %d, want the third reading", got)
	}
}

// boundChildTimeout is the -timeout of the child that gives up: its wait's bound end is 2 s in,
// 30 s before the deadline, so the give-up comes before Go's alarm.
const boundChildTimeout = "32s"

// UntilBoundEnd gives up at its end with one line on stderr and the same text as the test's
// failure, naming what it waited for and the last reading.
func TestUntilBoundEndGivesUpWithItsLineOnStderrAndTheSameFailure(t *testing.T) {
	child := exec.Command(os.Args[0], "-test.run=^TestTheBoundEndGivesUpChild$", "-test.timeout="+boundChildTimeout, "-test.v")
	child.Env = append(os.Environ(), childEnv+"=1")
	var stdout, stderr bytes.Buffer
	child.Stdout, child.Stderr = &stdout, &stderr
	if err := child.Run(); err == nil {
		t.Fatalf("the child was meant to fail; it passed:\n%s\nstderr:\n%s", &stdout, &stderr)
	}
	if strings.Contains(stdout.String()+stderr.String(), "panic: test timed out") {
		t.Fatalf("the child's wait ran into the binary's deadline rather than giving up before it:\n%s\nstderr:\n%s", &stdout, &stderr)
	}
	prefix := "testwait: TestTheBoundEndGivesUpChild: gave up waiting for a reading that never holds at "
	suffix := `, the end of its bound; last read: a later\nreading`
	var line string
	for _, written := range strings.Split(stderr.String(), "\n") {
		if strings.HasPrefix(written, prefix) && strings.HasSuffix(written, suffix) {
			line = written
		}
	}
	if line == "" {
		t.Fatalf("the child wrote no stderr line %q…%q; its stderr:\n%s", prefix, suffix, &stderr)
	}
	if !strings.Contains(stdout.String(), line+"\n") {
		t.Errorf("the child failed no test with %q; its output:\n%s", line, &stdout)
	}
}

// TestTheBoundEndGivesUpChild is the child of
// TestUntilBoundEndGivesUpWithItsLineOnStderrAndTheSameFailure, run under -timeout 32s with a
// bound of 1 s: its wait gives up 2 s in. It runs only when the parent re-executes this binary.
func TestTheBoundEndGivesUpChild(t *testing.T) {
	if os.Getenv(childEnv) == "" {
		t.Skip(childEnv + " is not set: this test is the child of TestUntilBoundEndGivesUpWithItsLineOnStderrAndTheSameFailure")
	}
	asks := 0
	UntilBoundEnd(t, time.Second, "a reading that never holds", func() (string, bool) {
		asks++
		if asks == 1 {
			return "the first reading", false
		}
		return "a later\nreading", false
	})
}
