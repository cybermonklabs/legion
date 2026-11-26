package testwait

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"slices"
	"strings"
	"testing"
	"time"
)

// boundRowEnv names, to TestABoundedWaitChild, which of its rows to run, and boundSpareEnv the
// spare its Bounded wait asks for.
const (
	boundRowEnv   = "TESTWAIT_BOUND_ROW"
	boundSpareEnv = "TESTWAIT_BOUND_SPARE"
)

// A wait with a failure text of its own, whose bound is Bounded's clamp to the test binary's
// deadline less a spare, or the deadline of a context from Context, shows a reader where it
// waited: after the test fails with the wait's own text, the test's output carries the goroutine
// stacks read just before that bound ended, the waiting goroutine's first, naming the function
// that waited, then the goroutine the test started that holds the fact back. A test that passes
// prints none, though its bound's end came while it still ran. Each child runs under what is left
// of this binary's deadline (Remaining); the Bounded wait asks for all but two seconds of it as
// spare, and the context's deadline is planted two seconds off (plantedDeadline), so each bound
// ends two seconds after the child began its tests, whenever the machine runs it. This row's file
// uses only what the package had before the bounds were watched, so the same row run at a commit
// without them fails by name. It logs each child's whole output.
func TestAWaitOfItsOwnBoundedByTestwaitPrintsTheStacks(t *testing.T) {
	timeout := Remaining(t)
	run := func(row string) (string, error) {
		child := exec.Command(os.Args[0], "-test.run=^TestABoundedWaitChild$", "-test.timeout="+timeout.String(), "-test.v")
		child.Env = append(os.Environ(), childEnv+"=1", boundSpareEnv+"="+(timeout-2*time.Second).String(), boundRowEnv+"="+row)
		var stdout, stderr bytes.Buffer
		child.Stdout, child.Stderr = &stdout, &stderr
		err := child.Run()
		t.Logf("the %s child's output (%v):\n%s\nits stderr:\n%s", row, err, &stdout, &stderr)
		if strings.Contains(stdout.String()+stderr.String(), "panic: test timed out") {
			t.Fatalf("the %s child ran into the binary's deadline", row)
		}
		return stdout.String(), err
	}

	for _, row := range []struct{ name, forcing, text, bound, function string }{
		{"fails", "the wait's bound ends two seconds after the child began", ": the fact never came within the bound Bounded gave the wait",
			"testwait.Bounded gave at bound_dump_test.go:", ".awaitWithItsOwnText("},
		{"context", "the wait's bound ends two seconds after the child began", ": the call under testwait.Context ended: context deadline exceeded",
			"testwait.Context gave at bound_dump_test.go:", ".awaitUnderTheContext("},
		{"span", "the wait's own span of 10s ends before the deadline", ": the fact never came within the wait's own 10s",
			"the bound of 10s testwait.Bounded gave at bound_dump_test.go:", ".awaitWithItsOwnSpan("},
		{"two", "two spans of 200ms end in a test that has already failed", ": the fact never came within the second span",
			"the bound of 200ms testwait.Bounded gave at bound_dump_test.go:", ".awaitTheSecondSpan("},
	} {
		out, err := run(row.name)
		var exit *exec.ExitError
		if !errors.As(err, &exit) || exit.ExitCode() != 1 {
			t.Errorf("the %s child = %v, want exit status 1", row.name, err)
			continue
		}
		if !strings.Contains(out, "forcing fired: "+row.forcing+", and the fact never comes") {
			t.Errorf("the %s child logged no line that its forcing fired", row.name)
		}
		if n := strings.Count(out, "the goroutine stacks read at "); n != 1 {
			t.Errorf("the %s child printed %d dumps of the goroutine stacks, want one, its latest reading", row.name, n)
		}
		output := strings.Split(out, "\n")
		index := func(holds func(string) bool) int { return slices.IndexFunc(output, holds) }
		failure := index(func(line string) bool { return strings.HasSuffix(line, row.text) })
		header := index(func(line string) bool {
			return strings.Contains(line, "testwait: TestABoundedWaitChild: the goroutine stacks read at ") && strings.Contains(line, row.bound)
		})
		end := index(func(line string) bool {
			return strings.Contains(line, "testwait: TestABoundedWaitChild: end of the goroutine stacks: ")
		})
		ended := index(func(line string) bool {
			return strings.Contains(line, "testwait: TestABoundedWaitChild: the test ended failed at ")
		})
		failed := index(func(line string) bool { return strings.HasPrefix(line, "--- FAIL: TestABoundedWaitChild (") })
		if failure < 0 || header < failure || end < header || ended < end || failed < ended {
			t.Errorf("the %s child's output holds no goroutine stacks naming %q after its wait's own failure (line %d) and before its --- FAIL line (line %d): their first line at %d, their last at %d, the test's end at %d",
				row.name, row.bound, failure, failed, header, end, ended)
			continue
		}
		stacks := output[header:end]
		waiting, _, _ := strings.Cut(strings.Join(stacks[1:], "\n"), "\n    \n")
		if !strings.Contains(waiting, row.function) {
			t.Errorf("the %s child's first stack, the waiting goroutine's, names no %s:\n%s", row.name, row.function, waiting)
		}
		if !strings.Contains(strings.Join(stacks, "\n"), ".holdsTheBoundedFactBack(") {
			t.Errorf("the %s child's stacks name no goroutine the test started that holds the fact back:\n%s", row.name, strings.Join(stacks, "\n"))
		}
	}

	out, err := run("passes")
	if err != nil || !strings.Contains(out, "--- PASS: TestABoundedWaitChild (") {
		t.Fatalf("the passing child = %v, want a pass", err)
	}
	if !strings.Contains(out, "the bound's end came while the test still ran") {
		t.Error("the passing child logged no line that its bound's end came while it ran")
	}
	if strings.Contains(out, "the goroutine stacks") {
		t.Error("the passing child printed goroutine stacks, though it passed")
	}
}

// TestABoundedWaitChild is the child of TestAWaitOfItsOwnBoundedByTestwaitPrintsTheStacks. Its
// "fails" row starts a goroutine that holds the fact back and waits, from awaitWithItsOwnText, for
// the bound Bounded gives to end, then fails with its own text; its "context" row does the same
// from awaitUnderTheContext, under a context from Context whose deadline is planted; its "span"
// row does the same from awaitWithItsOwnSpan, with a span of 10 s that ends before the deadline.
// Its "passes" row's fact is there at once, and it runs on past its bound's end before it passes.
// It runs only when the parent re-executes this binary.
func TestABoundedWaitChild(t *testing.T) {
	if os.Getenv(childEnv) == "" {
		t.Skip(childEnv + " is not set: this test is the child of TestAWaitOfItsOwnBoundedByTestwaitPrintsTheStacks")
	}
	spare, err := time.ParseDuration(os.Getenv(boundSpareEnv))
	if err != nil {
		t.Fatalf("the child reads the spare its parent names in %s: %v", boundSpareEnv, err)
	}
	deadline, _ := t.Deadline()
	holdBack := func(forcing string) {
		started, release := make(chan struct{}), make(chan struct{})
		t.Cleanup(func() { close(release) })
		go holdsTheBoundedFactBack(started, release)
		<-started
		t.Log("forcing fired: " + forcing + ", and the fact never comes")
	}
	switch row := os.Getenv(boundRowEnv); row {
	case "fails":
		holdBack("the wait's bound ends two seconds after the child began")
		awaitWithItsOwnText(t, spare, make(chan struct{}))
	case "context":
		holdBack("the wait's bound ends two seconds after the child began")
		awaitUnderTheContext(t, Context(plantedDeadline{t, time.Now().Add(margin + 2*time.Second)}))
	case "span":
		holdBack("the wait's own span of 10s ends before the deadline")
		awaitWithItsOwnSpan(t, make(chan struct{}))
	case "two":
		holdBack("two spans of 200ms end in a test that has already failed")
		t.Error("an early failure, so each span's end is read")
		awaitTheFirstSpan(t)
		awaitTheSecondSpan(t)
	case "passes":
		came := make(chan struct{}, 1)
		came <- struct{}{}
		awaitWithItsOwnText(t, spare, came)
		time.Sleep(time.Until(deadline.Add(-spare)) + 200*time.Millisecond)
		t.Log("the bound's end came while the test still ran")
	default:
		t.Fatalf("no row %q", row)
	}
}

// awaitWithItsOwnText waits for came until the bound Bounded gives with spare, and fails t with its
// own text when that bound ends first.
func awaitWithItsOwnText(t *testing.T, spare time.Duration, came <-chan struct{}) {
	t.Helper()
	select {
	case <-came:
	case <-time.After(Bounded(t, 1<<60, spare)):
		t.Fatal("the fact never came within the bound Bounded gave the wait")
	}
}

// awaitWithItsOwnSpan waits for came for a span of its own, 10 s, which Bounded clamps to the
// deadline less a second only when that is sooner, and fails t with its own text when the span
// ends first.
func awaitWithItsOwnSpan(t *testing.T, came <-chan struct{}) {
	t.Helper()
	select {
	case <-came:
	case <-time.After(Bounded(t, 10*time.Second, time.Second)):
		t.Fatal("the fact never came within the wait's own 10s")
	}
}

// awaitUnderTheContext waits until ctx ends, and fails t with its own text then.
func awaitUnderTheContext(t *testing.T, ctx context.Context) {
	t.Helper()
	<-ctx.Done()
	t.Fatalf("the call under testwait.Context ended: %v", ctx.Err())
}

// awaitTheFirstSpan waits for a fact that never comes until its span of 200 ms ends, and goes on.
func awaitTheFirstSpan(t *testing.T) {
	t.Helper()
	<-time.After(Bounded(t, 200*time.Millisecond, time.Second))
	t.Log("the first span ended")
}

// awaitTheSecondSpan waits for a fact that never comes until its span of 200 ms ends, and fails t
// with its own text then.
func awaitTheSecondSpan(t *testing.T) {
	t.Helper()
	<-time.After(Bounded(t, 200*time.Millisecond, time.Second))
	t.Fatal("the fact never came within the second span")
}

// holdsTheBoundedFactBack is the goroutine a failing row starts, which holds the fact back until
// the test ends. It closes started once it runs, inside this function.
func holdsTheBoundedFactBack(started chan<- struct{}, release <-chan struct{}) {
	close(started)
	<-release
}
