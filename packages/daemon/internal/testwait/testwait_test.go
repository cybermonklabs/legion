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
// value then; it gives up when its timer fires and no value came, reading its clock at that
// moment; and its give-up moment is the margin before the deadline: 30 s under a 10 minute
// -timeout, and a quarter of a 25 s one, 6.25 s, so the waits of a short -timeout still wait.
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

			// At the give-up moment, with no value, it gives up, reading that moment.
			c = newFakeClock(giveUp.Add(-time.Second))
			answers = startReceive(c, deadline, tc.timeout, make(chan int))
			armedFor(t, c, answers)
			c.now = giveUp
			c.fire <- giveUp
			if got := <-answers; got.ok || !got.gaveUp.Equal(giveUp) {
				t.Fatalf("the receive answered %+v, want it to give up at %s", got, giveUp)
			}
		})
	}
}

// receiveChildEnv marks the child run of TestReceiveFailsNamingWhenItGaveUp, which waits on a
// channel nothing sends to.
const receiveChildEnv = "TESTWAIT_RECEIVE_CHILD"

// Receive returns what the channel yields, the zero value of a closed one, and otherwise fails
// once its margin before the test binary's deadline has come, before the binary's own alarm. The
// child run waits under a -timeout of 4 s, whose margin is 1 s, and RequireGaveUp reads its
// failure.
func TestReceiveFailsNamingWhenItGaveUp(t *testing.T) {
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

	child := exec.Command(os.Args[0], "-test.run=^TestReceiveFailsNamingWhenItGaveUp$", "-test.count=1", "-test.timeout=4s")
	child.Env = append(os.Environ(), receiveChildEnv+"=1")
	out, err := child.CombinedOutput()
	exit, failed := err.(*exec.ExitError)
	switch {
	case !failed || exit.ExitCode() != 1:
		t.Fatalf("the child run = %v, want exit status 1; its output:\n%s", err, out)
	case strings.Contains(string(out), "panic: test timed out"):
		t.Fatalf("the child run ran into the binary's deadline, want the wait's own failure first; its output:\n%s", out)
	}
	RequireGaveUp(t, string(out), "a value nothing sends", 4*time.Second)
}
