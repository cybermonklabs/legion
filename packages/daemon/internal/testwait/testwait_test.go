package testwait

import (
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// fakeClock is a clock whose time the test sets and whose one timer the test fires: After reports
// the span it was asked for on armed, and the channel it returns receives only what the test
// sends on fire.
type fakeClock struct {
	now   time.Time
	armed chan time.Duration
	fire  chan time.Time
}

func newFakeClock(now time.Time) *fakeClock {
	return &fakeClock{now: now, armed: make(chan time.Duration, 1), fire: make(chan time.Time)}
}

func (c *fakeClock) Now() time.Time { return c.now }

func (c *fakeClock) After(d time.Duration) <-chan time.Time {
	c.armed <- d
	return c.fire
}

// received is what one receive answered.
type received struct {
	value  int
	gaveUp time.Time
	ok     bool
}

// startReceive runs receive on c for deadline and timeout over ch, and returns where its answer
// arrives.
func startReceive(c *fakeClock, deadline time.Time, timeout time.Duration, ch <-chan int) <-chan received {
	answers := make(chan received, 1)
	go func() {
		value, gaveUp, ok := receive(c, deadline, true, timeout, ch)
		answers <- received{value, gaveUp, ok}
	}()
	return answers
}

// armedFor is the span the receive on c armed its timer for; a receive that answered first,
// having armed none, fails the test.
func armedFor(t *testing.T, c *fakeClock, answers <-chan received) time.Duration {
	t.Helper()
	select {
	case d := <-c.armed:
		return d
	case got := <-answers:
		t.Fatalf("the receive answered %+v before it armed a timer", got)
		return 0
	}
}

// On its own clock, a receive is still waiting a second before its give-up moment and takes a
// value then; it gives up when its timer fires and no value came, reading its clock as it gives up
// (here a second past the give-up moment, as a timer a loaded machine fires late reads), not the
// moment it computed; and its give-up moment is the margin before the deadline: 30 s under a 10
// minute -timeout, and a quarter of a 25 s one, 6.25 s, so the waits of a short -timeout still wait.
func TestReceiveGivesUpExactlyTheMarginBeforeTheDeadline(t *testing.T) {
	deadline := time.Date(2026, 11, 20, 16, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		timeout, margin time.Duration
	}{
		{10 * time.Minute, 30 * time.Second},
		{25 * time.Second, 6250 * time.Millisecond},
	} {
		t.Run(tc.timeout.String(), func(t *testing.T) {
			if got := marginOf(tc.timeout); got != tc.margin {
				t.Fatalf("the margin of a %s -timeout is %s, want %s", tc.timeout, got, tc.margin)
			}
			giveUp := deadline.Add(-tc.margin)

			// A second before the give-up moment the wait is still on: it takes the value sent then.
			c := newFakeClock(giveUp.Add(-time.Second))
			ch := make(chan int)
			answers := startReceive(c, deadline, tc.timeout, ch)
			if d := armedFor(t, c, answers); d != time.Second {
				t.Fatalf("a receive a second before its give-up moment armed its timer for %s, want 1s", d)
			}
			ch <- 7
			if got := <-answers; !got.ok || got.value != 7 {
				t.Fatalf("the receive answered %+v, want the 7 sent while it waited", got)
			}

			// Its timer fires late, a second past the give-up moment, with no value: it gives up,
			// reading its clock then.
			c = newFakeClock(giveUp.Add(-time.Second))
			answers = startReceive(c, deadline, tc.timeout, make(chan int))
			armedFor(t, c, answers)
			late := giveUp.Add(time.Second)
			c.now = late
			c.fire <- late
			if got := <-answers; got.ok || !got.gaveUp.Equal(late) {
				t.Fatalf("the receive answered %+v, want it to give up reading its clock, %s", got, late)
			}
		})
	}
}

// receiveChildEnv marks the child run of TestReceiveFailsNamingWhenItGaveUp, which waits on a
// channel nothing sends to.
const receiveChildEnv = "TESTWAIT_RECEIVE_CHILD"

// Receive returns what the channel yields, the zero value of a closed one, and otherwise fails
// once its margin before the test binary's deadline has come, before the binary's own alarm. The
// child plants a deadline (deadlineOf) and waits on a stepped clock (waitClock), so its wait gives
// up at once, at the margin before that deadline as its clock reads it, and RequireGaveUp reads
// its failure. The child runs under what is left of this binary's own deadline, so its alarm is
// no end a child starved after its give-up reaches before it fails: a child that ran into its
// alarm had a wait that did not give up.
func TestReceiveFailsNamingWhenItGaveUp(t *testing.T) {
	if os.Getenv(receiveChildEnv) == "1" {
		c := &steppedClock{now: time.Now()}
		onClock(t, c)
		at := c.Now().Add(marginOf(timeoutFlag()) + 5*time.Second)
		deadlineOf = func(*testing.T) (time.Time, bool) { return at, true }
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

	timeout := Remaining(t)
	child := exec.Command(os.Args[0], "-test.run=^TestReceiveFailsNamingWhenItGaveUp$", "-test.count=1", "-test.timeout="+timeout.String())
	child.Env = append(os.Environ(), receiveChildEnv+"=1")
	out, err := child.CombinedOutput()
	exit, failed := err.(*exec.ExitError)
	switch {
	case !failed || exit.ExitCode() != 1:
		t.Fatalf("the child run = %v, want exit status 1; its output:\n%s", err, out)
	case strings.Contains(string(out), "panic: test timed out"):
		t.Fatalf("the child run ran into the binary's deadline, want the wait's own failure first; its output:\n%s", out)
	}
	RequireGaveUp(t, string(out), "a value nothing sends", timeout)
}
