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

	for _, row := range []struct{ name, text, function string }{
		{"fails", ": the fact never came within the bound Bounded gave the wait", ".awaitWithItsOwnText("},
		{"context", ": the call under testwait.Context ended: context deadline exceeded", ".awaitUnderTheContext("},
	} {
		out, err := run(row.name)
		var exit *exec.ExitError
		if !errors.As(err, &exit) || exit.ExitCode() != 1 {
			t.Errorf("the %s child = %v, want exit status 1", row.name, err)
			continue
		}
		if !strings.Contains(out, "forcing fired: the wait's bound ends two seconds after the child began, and the fact never comes") {
			t.Errorf("the %s child logged no line that its forcing fired", row.name)
		}
		output := strings.Split(out, "\n")
		failure := slices.IndexFunc(output, func(line string) bool { return strings.HasSuffix(line, row.text) })
		header := slices.IndexFunc(output, func(line string) bool {
			return strings.Contains(line, "testwait: TestABoundedWaitChild: the goroutine stacks 1s before the bound the test's wait took from testwait ended: ")
		})
		end := slices.IndexFunc(output, func(line string) bool {
			return strings.Contains(line, "testwait: TestABoundedWaitChild: end of the goroutine stacks: ")
		})
		failed := slices.IndexFunc(output, func(line string) bool { return strings.HasPrefix(line, "--- FAIL: TestABoundedWaitChild (") })
		if failure < 0 || header < failure || end < header || failed < end {
			t.Errorf("the %s child's output holds no goroutine stacks after its wait's own failure (line %d) and before its --- FAIL line (line %d): their first line at %d, their last at %d", row.name, failure, failed, header, end)
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
// from awaitUnderTheContext, under a context from Context whose deadline is planted. Its "passes"
// row's fact is there at once, and it runs on past its bound's end before it passes. It runs only
// when the parent re-executes this binary.
func TestABoundedWaitChild(t *testing.T) {
	if os.Getenv(childEnv) == "" {
		t.Skip(childEnv + " is not set: this test is the child of TestAWaitOfItsOwnBoundedByTestwaitPrintsTheStacks")
	}
	spare, err := time.ParseDuration(os.Getenv(boundSpareEnv))
	if err != nil {
		t.Fatalf("the child reads the spare its parent names in %s: %v", boundSpareEnv, err)
	}
	deadline, _ := t.Deadline()
	holdBack := func() {
		started, release := make(chan struct{}), make(chan struct{})
		t.Cleanup(func() { close(release) })
		go holdsTheBoundedFactBack(started, release)
		<-started
		t.Log("forcing fired: the wait's bound ends two seconds after the child began, and the fact never comes")
	}
	switch row := os.Getenv(boundRowEnv); row {
	case "fails":
		holdBack()
		awaitWithItsOwnText(t, spare, make(chan struct{}))
	case "context":
		holdBack()
		awaitUnderTheContext(t, Context(plantedDeadline{t, time.Now().Add(margin + 2*time.Second)}))
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

// awaitUnderTheContext waits until ctx ends, and fails t with its own text then.
func awaitUnderTheContext(t *testing.T, ctx context.Context) {
	t.Helper()
	<-ctx.Done()
	t.Fatalf("the call under testwait.Context ended: %v", ctx.Err())
}

// holdsTheBoundedFactBack is the goroutine a failing row starts, which holds the fact back until
// the test ends. It closes started once it runs, inside this function.
func holdsTheBoundedFactBack(started chan<- struct{}, release <-chan struct{}) {
	close(started)
	<-release
}
