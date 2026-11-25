package testwait

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"slices"
	"strings"
	"testing"
	"time"
)

// boundRowEnv names, to TestABoundedWaitChild, which of its rows to run.
const boundRowEnv = "TESTWAIT_BOUND_ROW"

// A wait with a failure text of its own, whose bound is Bounded's clamp to the test binary's
// deadline less a spare, shows a reader where it waited: after the test fails with the wait's own
// text, the test's output carries the goroutine stacks read just before that bound ended, the
// waiting goroutine's first, naming the function that waited, then the goroutine the test started
// that holds the fact back. A test that passes prints none, though its bound's end came while it
// still ran. Each child runs under the -timeout this test's deadline leaves (childTimeout), and its
// wait asks for all but two seconds of it as spare, so its bound ends two seconds after the child
// began its tests, whenever the machine runs it. This row's file uses only what the package had
// before the bounds were watched, so the same row run at a commit without them fails by name. It
// logs each child's whole output.
func TestAWaitOfItsOwnBoundedByTestwaitPrintsTheStacks(t *testing.T) {
	timeout := childTimeout(t)
	spare := timeout - 2*time.Second
	run := func(row string) (string, error) {
		child := exec.Command(os.Args[0], "-test.run=^TestABoundedWaitChild$", "-test.timeout="+timeout.String(), "-test.v")
		child.Env = append(os.Environ(), childEnv+"=1", spareEnv+"="+spare.String(), boundRowEnv+"="+row)
		var stdout, stderr bytes.Buffer
		child.Stdout, child.Stderr = &stdout, &stderr
		err := child.Run()
		t.Logf("the %s child's output (%v):\n%s\nits stderr:\n%s", row, err, &stdout, &stderr)
		requireNoRace(t, stdout.String(), stderr.String())
		if strings.Contains(stdout.String()+stderr.String(), "panic: test timed out") {
			t.Fatalf("the %s child ran into the binary's deadline", row)
		}
		return stdout.String(), err
	}

	out, err := run("fails")
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 1 {
		t.Fatalf("the failing child = %v, want exit status 1", err)
	}
	if !strings.Contains(out, "forcing fired: the wait's bound ends two seconds after the child began, and the fact never comes") {
		t.Error("the failing child logged no line that its forcing fired")
	}
	output := strings.Split(out, "\n")
	failure := slices.IndexFunc(output, func(line string) bool {
		return strings.HasSuffix(line, ": the fact never came within the bound Bounded gave the wait")
	})
	header := slices.IndexFunc(output, func(line string) bool {
		return strings.Contains(line, "testwait: TestABoundedWaitChild: the goroutine stacks 1s before the bound the test's wait took from testwait ended: ")
	})
	end := slices.IndexFunc(output, func(line string) bool {
		return strings.Contains(line, "testwait: TestABoundedWaitChild: end of the goroutine stacks: ")
	})
	failed := slices.IndexFunc(output, func(line string) bool { return strings.HasPrefix(line, "--- FAIL: TestABoundedWaitChild (") })
	if failure < 0 || header < failure || end < header || failed < end {
		t.Fatalf("the failing child's output holds no goroutine stacks after its wait's own failure (line %d) and before its --- FAIL line (line %d): their first line at %d, their last at %d", failure, failed, header, end)
	}
	stacks := output[header:end]
	waiting, _, _ := strings.Cut(strings.Join(stacks[1:], "\n"), "\n    \n")
	for _, frame := range []struct{ in, function, why string }{
		{waiting, ".awaitWithItsOwnText(", "the waiting goroutine's stack, first, names the function that waited"},
		{strings.Join(stacks, "\n"), ".holdsTheBoundedFactBack(", "the stacks name the goroutine the test started that holds the fact back"},
	} {
		if !strings.Contains(frame.in, frame.function) {
			t.Errorf("%s: no frame %s in:\n%s", frame.why, frame.function, frame.in)
		}
	}

	out, err = run("passes")
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
// the bound Bounded gives to end, then fails with its own text. Its "passes" row's fact is there
// at once, and it runs on past its bound's end before it passes. It runs only when the parent
// re-executes this binary.
func TestABoundedWaitChild(t *testing.T) {
	if os.Getenv(childEnv) == "" {
		t.Skip(childEnv + " is not set: this test is the child of TestAWaitOfItsOwnBoundedByTestwaitPrintsTheStacks")
	}
	spare, err := time.ParseDuration(os.Getenv(spareEnv))
	if err != nil {
		t.Fatalf("the child reads the spare its parent names in %s: %v", spareEnv, err)
	}
	deadline, _ := t.Deadline()
	switch row := os.Getenv(boundRowEnv); row {
	case "fails":
		started, release := make(chan struct{}), make(chan struct{})
		t.Cleanup(func() { close(release) })
		go holdsTheBoundedFactBack(started, release)
		<-started
		t.Log("forcing fired: the wait's bound ends two seconds after the child began, and the fact never comes")
		awaitWithItsOwnText(t, spare, make(chan struct{}))
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

// holdsTheBoundedFactBack is the goroutine the failing row starts, which holds the fact back until
// the test ends. It closes started once it runs, inside this function.
func holdsTheBoundedFactBack(started chan<- struct{}, release <-chan struct{}) {
	close(started)
	<-release
}
