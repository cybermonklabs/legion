package testwait

import (
	"bytes"
	"os"
	"os/exec"
	"testing"
	"time"
)

// childWaitDelay is how long RunChild's wait for a child it killed gives a process the child
// started, which still holds the child's output, before it closes that output itself and returns
// what was read. It bounds nothing a child does while it runs.
const childWaitDelay = 10 * time.Second

// SetUpDone is the step a child test binary takes once its setup has reported done, in a child its
// parent runs to read how a wait of the child gives up (RunChild). From the call until t ends, the
// waits of this binary read a clock the child moves (stepClock) that stands at the moment they give
// up (GiveUpAt): the next wait asks its question once and gives up, and a wait that waits on its
// timer, such as Receive, takes a value already there and otherwise gives up once it is blocked on
// that timer. A fixture's own spans pass on that clock too (Within), in their moments' order, and
// one that would end past the test binary's deadline, the margin past the give-up, ends the binary
// as go test's alarm would, so what a child holds of the margin it holds in clock time. The setup
// before the call ran on the wall clock under the shared wait's own bound, however long a loaded
// machine took over it, so the give-up the child then reports comes at a step of its own, and
// follows from what its waits did, not from how long the machine took. The child's -timeout is then
// a hang guard only. A child with no deadline (-timeout 0) has no moment for its waits to give up
// at, and SetUpDone fails t.
func SetUpDone(t testing.TB) {
	t.Helper()
	end, bounded := GiveUpAt(t)
	if !bounded {
		t.Fatalf("testwait: %s: SetUpDone has no moment to give up at: the child has no deadline, and a child its parent runs (RunChild) takes the hang guard its parent gives it as its -timeout", t.Name())
	}
	useStepClock(t, end).setDeadline(end.Add(margin))
}

// RunChild runs this test binary again, once, as a child of t: with env added to the environment it
// inherits, -test.run run, -test.v, and -test.timeout guard. It returns what the child wrote to
// stdout and to stderr, and the error the child ended with, for t to judge. A child meant to give
// up takes its give-up at a step of its own once its setup has reported done (SetUpDone), so guard
// is a hang guard only: what a child whose wait never gives up runs into, never what decides the
// verdict. A caller sizes it from the row's measured span, and says so. A guard of 0 runs the child
// with no deadline, as -timeout 0 does. The child runs on t's Context: a child still running where
// t's waits give up, such as one stuck after its tests, where its own -timeout no longer watches it,
// is killed, and RunChild fails t naming it, with what it wrote.
func RunChild(t testing.TB, guard time.Duration, run string, env ...string) (stdout, stderr string, err error) {
	t.Helper()
	ctx := Context(t)
	child := exec.CommandContext(ctx, os.Args[0], "-test.run="+run, "-test.v", "-test.timeout="+guard.String())
	child.Env = append(os.Environ(), env...)
	child.WaitDelay = childWaitDelay
	var out, errOut bytes.Buffer
	child.Stdout, child.Stderr = &out, &errOut
	err = child.Run()
	if ctx.Err() != nil {
		t.Fatalf("testwait: %s: the child running %s under -timeout %s was still running where this test's waits give up, so it was killed (%v):\n%s\nstderr:\n%s",
			t.Name(), run, guard, err, &out, &errOut)
	}
	return out.String(), errOut.String(), err
}
