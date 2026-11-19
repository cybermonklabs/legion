package testwait

import (
	"context"
	"testing"
	"time"
)

// noDeadline is a testing.TB whose test has no deadline, as under `go test -timeout 0`.
type noDeadline struct{ testing.TB }

// A call a test only needs answered is given what the test's own deadline leaves, less Cleanup;
// with no test deadline it has none but its parent's, and its parent's end still ends it.
func TestAnsweredTakesTheTestsDeadlineLessCleanup(t *testing.T) {
	ctx, cancel := Answered(t, context.Background())
	defer cancel()
	got, has := ctx.Deadline()
	if deadline, set := t.Deadline(); set {
		if !has || !got.Equal(deadline.Add(-Cleanup)) {
			t.Errorf("Answered's deadline = %v (set %t), want the test's deadline %v less Cleanup", got, has, deadline)
		}
	} else if has {
		t.Errorf("with no test deadline Answered gave the call the deadline %v", got)
	}
	parent, stop := context.WithCancel(context.Background())
	none, cancelNone := Answered(noDeadline{t}, parent)
	defer cancelNone()
	if deadline, has := none.Deadline(); has {
		t.Errorf("a test with no deadline gave the call the deadline %v, want none", deadline)
	}
	stop()
	if none.Err() == nil {
		t.Error("the parent's end left the call's context live")
	}
}

// A wait for a container the test starts is given what the test's own deadline leaves, less
// Cleanup, and with no test deadline NoBound.
func TestStartupBoundTakesTheTestsDeadlineLessCleanup(t *testing.T) {
	before := time.Now()
	given := StartupBound(t)
	after := time.Now()
	if deadline, set := t.Deadline(); set {
		end := deadline.Add(-Cleanup)
		if given > end.Sub(before) || given < end.Sub(after) {
			t.Errorf("StartupBound = %s, want what the test's deadline leaves less Cleanup, %s to %s", given, end.Sub(after), end.Sub(before))
		}
	} else if given != NoBound {
		t.Errorf("with no test deadline StartupBound = %s, want NoBound", given)
	}
	if none := StartupBound(noDeadline{t}); none != NoBound {
		t.Errorf("a test with no deadline was given %s, want NoBound", none)
	}
}
