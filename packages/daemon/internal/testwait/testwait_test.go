package testwait

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"
)

// noDeadline is a testing.TB whose test has no deadline, as under `go test -timeout 0`.
type noDeadline struct{ testing.TB }

// shortDeadline is a testing.TB whose test binary's deadline is at, and whose Fatalf records its
// message and returns, so a test can read what a helper refused.
type shortDeadline struct {
	testing.TB
	at      time.Time
	refused []string
}

func (s *shortDeadline) Deadline() (time.Time, bool) { return s.at, true }
func (s *shortDeadline) Helper()                     {}
func (s *shortDeadline) Fatalf(format string, args ...any) {
	s.refused = append(s.refused, fmt.Sprintf(format, args...))
}

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

// A test whose deadline leaves its call nothing after Cleanup is refused by name, the deadline, the
// reserve and the shortfall, and its call gets a context already ended, never a deadline in the
// past handed on as if it were time; a deadline that leaves time is not refused.
func TestAnsweredRefusesADeadlineThatLeavesTheCallNoTime(t *testing.T) {
	short := &shortDeadline{TB: t, at: time.Now().Add(Cleanup / 2)}
	ctx, cancel := Answered(short, context.Background())
	defer cancel()
	if len(short.refused) != 1 {
		t.Fatalf("Answered under a deadline %s away, inside the %s reserve, refused %d times (%q), want once", Cleanup/2, Cleanup, len(short.refused), short.refused)
	}
	for _, want := range []string{"testwait: the test binary's deadline, " + short.at.Format(time.RFC3339), Cleanup.String() + " testwait.Cleanup", " short; run the test with a -timeout"} {
		if !strings.Contains(short.refused[0], want) {
			t.Errorf("the refusal %q does not name %q", short.refused[0], want)
		}
	}
	if ctx.Err() == nil {
		t.Error("the refused call's context is live")
	}
	roomy := &shortDeadline{TB: t, at: time.Now().Add(Cleanup + time.Minute)}
	ctx, cancel = Answered(roomy, context.Background())
	defer cancel()
	if len(roomy.refused) != 0 || ctx.Err() != nil {
		t.Errorf("a deadline a minute past the reserve was refused (%q) or gave an ended context (%v)", roomy.refused, ctx.Err())
	}
}
