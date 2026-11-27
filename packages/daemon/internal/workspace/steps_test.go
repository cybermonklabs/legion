package workspace

import (
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

// readClock is a testClock that also moves on by perRead at each read of it. A Steps that read it
// more than once at one boundary, one read to end a step and another to start the next, would give
// the instants between the two reads to no step, and its steps would then add up to less than its
// total.
type readClock struct {
	*testClock
	perRead time.Duration
}

func (c *readClock) Now() time.Time {
	now := c.testClock.Now()
	c.advance(c.perRead)
	return now
}

// loggedStep is one line a Steps logs, read: a step that took or failed after its duration, or the
// total, each in tenths of a second.
var loggedStep = regexp.MustCompile(`^(?:step ([a-z]+(?:-[a-z]+)*) (took|failed after) ([0-9]+)\.([0-9])s|steps took ([0-9]+)\.([0-9])s)$`)

// tenthsOf is the duration whose whole seconds and tenth a logged line printed, in tenths.
func tenthsOf(t *testing.T, whole, tenth string) int {
	t.Helper()
	w, err := strconv.Atoi(whole)
	if err != nil {
		t.Fatal(err)
	}
	f, err := strconv.Atoi(tenth)
	if err != nil {
		t.Fatal(err)
	}
	return 10*w + f
}

// stepSums is the sum of the steps' durations lines logs, and the total its last line logs, -1
// when it logs none, both in tenths of a second; it fails the test on a line no Steps logs.
func stepSums(t *testing.T, lines []string) (steps, total int) {
	t.Helper()
	total = -1
	for _, line := range lines {
		match := loggedStep.FindStringSubmatch(line)
		switch {
		case match == nil:
			t.Fatalf("the steps logged %q, which is no step's line", line)
		case match[1] != "":
			steps += tenthsOf(t, match[3], match[4])
		default:
			total = tenthsOf(t, match[5], match[6])
		}
	}
	return steps, total
}

// A provision's step clock gives each instant of its run to exactly one step: each boundary reads
// the clock once, and that instant ends one step and opens the next. On a clock that also moves on
// at each read, the steps' durations add up to the total End logs exactly, and those of a run that
// failed add up to its whole span; Fail names the step that was open. A nil Steps, the tmux
// runtime's, logs nothing, and a step name that is not lower-case words joined by hyphens panics,
// naming it.
func TestProvisionStepsGiveEachInstantToOneStep(t *testing.T) {
	t.Parallel()
	const perRead = 100 * time.Millisecond

	t.Run("a run that ends", func(t *testing.T) {
		t.Parallel()
		clock := &readClock{testClock: newTestClock(), perRead: perRead}
		var lines []string
		steps := NewSteps(clock, func(line string) { lines = append(lines, line) })
		clock.advance(1200 * time.Millisecond)
		steps.Begin("clone-stage")
		clock.advance(300 * time.Millisecond)
		steps.Begin("github-delta")
		clock.advance(4500 * time.Millisecond)
		steps.End()
		want := []string{"step inputs took 1.3s", "step clone-stage took 0.4s", "step github-delta took 4.6s", "steps took 6.3s"}
		if !slices.Equal(lines, want) {
			t.Fatalf("the steps logged %q, want %q", lines, want)
		}
		if sum, total := stepSums(t, lines); sum != total {
			t.Fatalf("the steps took %d tenths of a second together and the run %d, want the same", sum, total)
		}
		steps.Begin("checkout")
		steps.Fail()
		steps.End()
		if len(lines) != len(want) {
			t.Fatalf("the ended steps logged %q after their total, want nothing", lines[len(want):])
		}
	})

	t.Run("a run that fails", func(t *testing.T) {
		t.Parallel()
		clock := &readClock{testClock: newTestClock(), perRead: perRead}
		var lines []string
		started := clock.Now()
		steps := NewSteps(clock, func(line string) { lines = append(lines, line) })
		clock.advance(700 * time.Millisecond)
		steps.Begin("cache-wait")
		clock.advance(2 * time.Second)
		steps.Begin("github-delta")
		clock.advance(900 * time.Millisecond)
		steps.Fail()
		// The clock's reads: started, then NewSteps', each Begin's and Fail's; the span from
		// NewSteps' read to Fail's is the run's.
		run := clock.now.Sub(started) - 2*perRead
		want := []string{"step inputs took 0.8s", "step cache-wait took 2.1s", "step github-delta failed after 1.0s"}
		if !slices.Equal(lines, want) {
			t.Fatalf("the steps logged %q, want %q", lines, want)
		}
		sum, total := stepSums(t, lines)
		if total != -1 || time.Duration(sum)*100*time.Millisecond != run {
			t.Fatalf("the failed steps took %d tenths of a second together and logged the total %d, want their run's %s and no total", sum, total, run)
		}
		steps.End()
		if len(lines) != len(want) {
			t.Fatalf("the failed steps logged %q after their failure, want nothing", lines[len(want):])
		}
	})

	t.Run("a nil Steps", func(t *testing.T) {
		t.Parallel()
		var steps *Steps
		steps.Begin("checkout")
		steps.Fail()
		steps.End()
	})

	t.Run("a name that is no step's", func(t *testing.T) {
		t.Parallel()
		for _, name := range []string{"", "Checkout", "check out", "jj_init", "-checkout", "checkout-", "jj--init", "pack2"} {
			for _, steps := range []*Steps{nil, NewSteps(newTestClock(), func(string) {})} {
				said := func() (said any) {
					defer func() { said = recover() }()
					steps.Begin(name)
					return nil
				}()
				if said == nil || !strings.Contains(fmt.Sprint(said), strconv.Quote(name)) {
					t.Errorf("Begin(%q) on %v panicked with %v, want a panic naming it", name, steps, said)
				}
			}
		}
	})
}
