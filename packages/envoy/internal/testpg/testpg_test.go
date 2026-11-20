package testpg

import (
	"testing"
	"time"
)

// noDeadline is a testing.TB whose test has no deadline, as under `go test -timeout 0`.
type noDeadline struct{ testing.TB }

// The wait for a started container's Postgres has no bound of its own: it is given what the test's
// deadline leaves, not testcontainers' default 60 s, which a loaded host can pass while Postgres
// still runs its first initdb; and with no deadline it has no bound.
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
		if *given != noBound {
			t.Errorf("with no test deadline the ready wait was given %s, want no bound (%s)", *given, noBound)
		}
	} else if *given > deadline.Sub(before) || *given < deadline.Sub(after) {
		t.Errorf("the ready wait was given %s, want what the test's deadline left, %s to %s", *given, deadline.Sub(after), deadline.Sub(before))
	}
	if none := readyWait(noDeadline{t}).Timeout(); none == nil || *none != noBound {
		t.Errorf("a test with no deadline gave the ready wait %v, want no bound (%s)", none, noBound)
	}
}
