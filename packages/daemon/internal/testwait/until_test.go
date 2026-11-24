package testwait

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"slices"
	"strings"
	"testing"
	"time"
)

// childEnv tells the re-executed test binary to run the helpers below, which skip in an ordinary
// run. One gives up on purpose and the other reads the deadline under -timeout 0, and the parent
// names each flag, so each runs in a binary of its own.
const childEnv = "TESTWAIT_CHILD"

// A wait whose condition holds returns from its first question: it asks once and never sleeps
// at the pace first.
func TestAWaitThatHoldsReturnsAtOnce(t *testing.T) {
	asks := 0
	Eventually(t, "a condition that holds", func() bool { asks++; return true })
	if asks != 1 {
		t.Fatalf("Eventually asked a condition that holds %d times, want 1", asks)
	}
	asks = 0
	if got := Until(t, "a reading that holds", func() (string, bool) { asks++; return "the reading", true }); got != "the reading" {
		t.Fatalf("Until returned %q, want the reading that held", got)
	}
	if asks != 1 {
		t.Fatalf("Until asked a reading that holds %d times, want 1", asks)
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

// Context's deadline is the moment a wait gives up, and the context ends with the test that took it.
func TestContextEndsWhereAWaitGivesUpAndWithTheTest(t *testing.T) {
	var taken context.Context
	t.Run("taker", func(t *testing.T) {
		taken = Context(t)
		end, bounded := giveUpAt(t)
		deadline, has := taken.Deadline()
		if has != bounded || !deadline.Equal(end) {
			t.Fatalf("Context's deadline is %v (%t), want the moment a wait gives up, %v (%t)", deadline, has, end, bounded)
		}
		if err := taken.Err(); err != nil {
			t.Fatalf("Context ended before its test did: %v", err)
		}
	})
	if err := taken.Err(); !errors.Is(err, context.Canceled) {
		t.Fatalf("Context outlived the test that took it: its error is %v, want context.Canceled", err)
	}
}

// A wait gives up the margin before the test binary's deadline. A *testing.T names that deadline;
// a testing.TB without Deadline reads it from the -timeout flag and the process's start, which is
// no later than testing's own. That the wait then holds to that moment, with no cap of its own, is
// TestEventuallyHasNoCapOfItsOwn's.
func TestAWaitGivesUpTheMarginBeforeTheDeadline(t *testing.T) {
	deadline, bounded := t.Deadline()
	end, ok := giveUpAt(t)
	if ok != bounded || ok && !end.Equal(deadline.Add(-margin)) {
		t.Fatalf("a wait in a test whose deadline is %v (%t) gives up at %v (%t), want %s before it", deadline, bounded, end, ok, margin)
	}
	fromFlag, ok := giveUpAt(noDeadline{t})
	if ok != bounded || ok && fromFlag.After(end) {
		t.Fatalf("a wait in a testing.TB without Deadline gives up at %v (%t), want no later than %v (%t)", fromFlag, ok, end, bounded)
	}
}

// Eventually has no cap of its own: on a clock that moves only as the wait waits, a condition that
// holds 90 s in, past the minute Eventually once gave up at, is waited for under a deadline ten
// minutes off. The wait asks once per pace, 9,001 times, and returns at the 90 s.
func TestEventuallyHasNoCapOfItsOwn(t *testing.T) {
	c := &steppedClock{now: time.Now()}
	onClock(t, c)
	start := c.Now()
	asks := 0
	Eventually(plantedDeadline{t, start.Add(10 * time.Minute)}, "a condition that holds 90 s in", func() bool {
		asks++
		return c.Now().Sub(start) >= 90*time.Second
	})
	if waited := c.Now().Sub(start); waited != 90*time.Second || asks != 9001 {
		t.Fatalf("Eventually returned %s into its wait after %d asks, want 90s and 9001", waited, asks)
	}
}

// A wait that gives up writes its line to stderr and fails the test with the same text, which
// carries the wait's last reading where it has one, the reading it read last however many it
// read. A wait that starts once the margin has passed, a wait in a cleanup included, asks once and
// fails, and its line says the moment had passed before it began, so the first give-up line in
// time order, the one without that note, names the cause. The child plants each wait's deadline
// (plantedDeadline) and runs under what is left of this binary's own deadline, so its alarm is no
// end a slow child reaches before its waits give up: a child that ran into its alarm had a wait
// that did not give up.
func TestAWaitGivesUpWithItsLineOnStderrAndTheSameFailure(t *testing.T) {
	child := exec.Command(os.Args[0], "-test.run=^TestTheWaitsGiveUpChild$", "-test.timeout="+Remaining(t).String(), "-test.v")
	child.Env = append(os.Environ(), childEnv+"=1")
	var stdout, stderr bytes.Buffer
	child.Stdout, child.Stderr = &stdout, &stderr
	if err := child.Run(); err == nil {
		t.Fatalf("the child was meant to fail; it passed:\n%s\nstderr:\n%s", &stdout, &stderr)
	}
	if strings.Contains(stdout.String(), "panic: test timed out") || strings.Contains(stderr.String(), "panic: test timed out") {
		t.Fatalf("the child's waits ran into its alarm rather than giving up before it:\n%s\nstderr:\n%s", &stdout, &stderr)
	}
	var reads int
	if _, after, found := strings.Cut(stdout.String(), "the reading was asked "); !found {
		t.Fatalf("the child logged no count of the reading's asks; its output:\n%s", &stdout)
	} else if _, err := fmt.Sscanf(after, "%d", &reads); err != nil || reads != 6 {
		t.Fatalf("the child's count of the reading's asks %.20q (%v), want 6: one a pace over the stepped clock's 50 ms, and one at its end", after, err)
	}
	name := "TestTheWaitsGiveUpChild/"
	ending := " 30s before the test binary's deadline"
	spent := ending + ", a moment that had passed before this wait began: the first give-up line in time order is the cause"
	want := []string{
		"testwait: " + name + "until: gave up waiting for a reading that never holds" + ending + fmt.Sprintf(`; last read: reading\n%d`, reads),
		"testwait: " + name + "eventually: gave up waiting for a condition that never holds" + spent,
		"testwait: " + name + "cleanup: gave up waiting for a condition a cleanup waits for" + spent,
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
	if !strings.Contains(stdout.String(), "the condition was asked 1 time(s)") {
		t.Errorf("a wait that began after the margin did not ask exactly once; the child's output:\n%s", &stdout)
	}
	if first, later := slices.Index(written, want[0]), slices.Index(written, want[1]); first < 0 || later < first {
		t.Errorf("the give-up with no note (line %d of stderr) does not come before the one that began past the margin (line %d); its stderr:\n%s", first, later, &stderr)
	}
}

// Under -timeout 0 there is no deadline: a wait has no moment to give up at, asks until its
// condition holds, and Context has no deadline.
func TestUnderNoTimeoutAWaitHasNoEnd(t *testing.T) {
	child := exec.Command(os.Args[0], "-test.run=^TestNoDeadlineChild$", "-test.timeout=0", "-test.v")
	child.Env = append(os.Environ(), childEnv+"=1")
	output, err := child.CombinedOutput()
	if err != nil || !strings.Contains(string(output), "--- PASS: TestNoDeadlineChild (") {
		t.Fatalf("under -timeout 0 the child did not pass: %v\n%s", err, output)
	}
}

// plantedDeadline is a test whose deadline is at, not the test binary's: a wait in it gives up
// the margin before at, and fails the test it embeds.
type plantedDeadline struct {
	*testing.T
	at time.Time
}

func (p plantedDeadline) Deadline() (time.Time, bool) { return p.at, true }

// steppedClock is a clock that moves only when a wait waits on it: After(d) moves it d on and
// answers at once.
type steppedClock struct{ now time.Time }

func (c *steppedClock) Now() time.Time { return c.now }

func (c *steppedClock) After(d time.Duration) <-chan time.Time {
	c.now = c.now.Add(d)
	fired := make(chan time.Time, 1)
	fired <- c.now
	return fired
}

// onClock makes c the clock poll and asked read (waitClock) until t ends.
func onClock(t *testing.T, c clock) {
	t.Cleanup(func() { waitClock = realClock{} })
	waitClock = c
}

// TestTheWaitsGiveUpChild is the child of TestAWaitGivesUpWithItsLineOnStderrAndTheSameFailure.
// Its until wait runs on a stepped clock and gives up 50 ms of that clock after it begins, having
// asked its reading six times; each later wait begins past its margin. It runs only when the
// parent re-executes this binary.
func TestTheWaitsGiveUpChild(t *testing.T) {
	if os.Getenv(childEnv) == "" {
		t.Skip(childEnv + " is not set: this test is the child of TestAWaitGivesUpWithItsLineOnStderrAndTheSameFailure")
	}
	t.Run("until", func(t *testing.T) {
		c := &steppedClock{now: time.Now()}
		onClock(t, c)
		asks := 0
		t.Cleanup(func() { t.Logf("the reading was asked %d time(s)", asks) })
		Until(plantedDeadline{t, c.Now().Add(margin + 50*time.Millisecond)}, "a reading that never holds", func() (string, bool) {
			asks++
			return fmt.Sprintf("reading\n%d", asks), false
		})
	})
	t.Run("eventually", func(t *testing.T) {
		asks := 0
		t.Cleanup(func() { t.Logf("the condition was asked %d time(s)", asks) })
		Eventually(plantedDeadline{t, time.Now()}, "a condition that never holds", func() bool { asks++; return false })
	})
	t.Run("cleanup", func(t *testing.T) {
		t.Cleanup(func() {
			Eventually(plantedDeadline{t, time.Now()}, "a condition a cleanup waits for", func() bool { return false })
		})
	})
}

// TestNoDeadlineChild is the child of TestUnderNoTimeoutAWaitHasNoEnd, run under -timeout 0. It
// runs only when the parent re-executes this binary.
func TestNoDeadlineChild(t *testing.T) {
	if os.Getenv(childEnv) == "" {
		t.Skip(childEnv + " is not set: this test is the child of TestUnderNoTimeoutAWaitHasNoEnd")
	}
	if end, ok := giveUpAt(t); ok {
		t.Errorf("under -timeout 0 a wait gives up at %v, want never", end)
	}
	if end, ok := giveUpAt(noDeadline{t}); ok {
		t.Errorf("under -timeout 0 a wait in a testing.TB without Deadline gives up at %v, want never", end)
	}
	asks := 0
	Eventually(t, "the third answer", func() bool { asks++; return asks == 3 })
	if asks != 3 {
		t.Errorf("under -timeout 0 Eventually asked its condition %d times, want 3", asks)
	}
	if deadline, ok := Context(t).Deadline(); ok {
		t.Errorf("under -timeout 0 Context has the deadline %v, want none", deadline)
	}
}

// noDeadline is a testing.TB with no Deadline method, as a benchmark's is.
type noDeadline struct{ testing.TB }
