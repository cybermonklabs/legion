package testpg

import (
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/testwait"
)

// noDeadline is a testing.TB whose test has no deadline, as under `go test -timeout 0`.
type noDeadline struct{ testing.TB }

// The wait for a started container's Postgres has no bound of its own: it is given what the test's
// deadline leaves less the test's cleanup, not testcontainers' default 60 s, which a loaded host
// can pass while Postgres still runs its first initdb; and with no deadline it has no bound.
func TestTheReadyWaitTakesWhatTheTestsDeadlineLeaves(t *testing.T) {
	deadline, set := t.Deadline()
	before := time.Now()
	strategy := readyWait(t)
	after := time.Now()
	given := strategy.Timeout()
	if given == nil {
		t.Fatal("the ready wait has no startup timeout, so testcontainers gives it its own 60 s")
	}
	if !set {
		if *given != testwait.NoBound {
			t.Errorf("with no test deadline the ready wait was given %s, want no bound", *given)
		}
	} else if end := deadline.Add(-testwait.Cleanup); *given > end.Sub(before) || *given < end.Sub(after) {
		t.Errorf("the ready wait was given %s, want what the test's deadline left less its cleanup, %s to %s", *given, end.Sub(after), end.Sub(before))
	}
	if none := readyWait(noDeadline{t}).Timeout(); none == nil || *none != testwait.NoBound {
		t.Errorf("a test with no deadline gave the ready wait %v, want no bound", none)
	}
}
