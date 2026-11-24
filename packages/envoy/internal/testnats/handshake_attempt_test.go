package testnats

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/testwait"
)

// heldAttemptConn connects to hold's URL with a client whose handshake attempts last timeout and
// come heldAttemptGap apart, as a bus client's last 5 s and come 1 s apart, and reconnects for as
// long as the test runs. The hold is released before the client closes, since nats.go's Close waits
// for the connection lock a held handshake holds.
func heldAttemptConn(t *testing.T, hold *HandshakeHold, timeout time.Duration) *natsgo.Conn {
	t.Helper()
	conn, err := natsgo.Connect(hold.URL(),
		natsgo.Timeout(timeout),
		natsgo.ReconnectWait(heldAttemptGap),
		natsgo.MaxReconnects(-1),
	)
	if err != nil {
		t.Fatalf("connect through the handshake hold: %v", err)
	}
	t.Cleanup(conn.Close)
	t.Cleanup(hold.Release)
	return conn
}

const (
	// heldAttemptTimeout is the child's connect timeout, how long each of its held attempts keeps
	// nats.go's connection lock: the child's measured call answers once an attempt ends.
	heldAttemptTimeout = 2 * time.Second
	// heldAttemptGap is the client's ReconnectWait, how long nats.go waits between two attempts with
	// its lock free.
	heldAttemptGap = time.Second
	// heldAttemptSpare is what the rows below leave of the test binary's deadline for their own
	// failure and cleanups (testwait.Remaining).
	heldAttemptSpare = 30 * time.Second
	// childReportSpare is how much sooner than the context that kills it the child's own -timeout
	// fires, so a child that hangs prints its goroutines and exits by its alarm, naming itself.
	childReportSpare = 10 * time.Second
)

// A call that needs no connection lock answers within the attempt held when it began, and
// AnswersWithinAttempt passes it: the lock is taken once the call has answered, nats.go did not let
// it go, and no later attempt reached the hold. The client's held attempt lasts until the test
// binary's deadline, less heldAttemptSpare, so no span of the attempt ends the row; the cleanup's
// Release ends the attempt.
func TestAnswersWithinAttemptPassesACallThatNeedsNoLock(t *testing.T) {
	hold := StartHandshakeHold(t, URL(t))
	conn := heldAttemptConn(t, hold, testwait.Remaining(t, heldAttemptSpare))
	hold.HoldReconnect(t, conn)
	hold.AnswersWithinAttempt(t, conn, "a call that needs no lock", func() error { return nil })
}

// attemptChildEnv tells the re-executed test binary to run TestAnswersWithinAttemptChild, which
// skips in an ordinary run.
const attemptChildEnv = "TESTNATS_ATTEMPT_CHILD"

// attemptHeld is the line the child logs once HoldReconnect has returned, before it measures.
const attemptHeld = "the child's handshake attempt is held"

// AnswersWithinAttempt fails a call that waits out the attempt held when it began. The child's call
// reads conn's status, which waits for nats.go's connection lock, so it answers only once the held
// attempt has ended at the client's connect timeout, in the second before the next attempt; the
// child must fail there by AnswersWithinAttempt's own words. A check that takes the lock to be held
// whenever a read of the status is slow passes the call when that read is descheduled, and so does
// one that reads the attempt count before the lock when the check is held up past the gap; the child
// then passes, which fails this row. The child runs in a binary of its own, so its failure is read
// rather than taken as this row's. The context that ends it fires when the test binary's deadline
// leaves heldAttemptSpare, and the child's own -timeout childReportSpare before that, so a child
// that hangs is ended by its alarm, with its goroutines printed, and this row's failure is still to
// come.
func TestAnswersWithinAttemptFailsACallThatWaitsOutTheAttempt(t *testing.T) {
	childTimeout := testwait.Remaining(t, heldAttemptSpare+childReportSpare)
	ctx, cancel := context.WithTimeout(t.Context(), testwait.Remaining(t, heldAttemptSpare))
	defer cancel()
	child := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestAnswersWithinAttemptChild$", "-test.timeout="+childTimeout.String(), "-test.v")
	child.Env = append(os.Environ(), attemptChildEnv+"=1")
	output, err := child.CombinedOutput()
	text := string(output)
	if err == nil {
		t.Fatalf("AnswersWithinAttempt passed a call that waited out the held attempt: the child passed:\n%s", text)
	}
	if strings.Contains(text, "panic:") || !strings.Contains(text, attemptHeld) || !strings.Contains(text, "--- FAIL: TestAnswersWithinAttemptChild (") {
		t.Fatalf("the child did not fail its measurement after its attempt was held (%v):\n%s", err, text)
	}
	for _, words := range []string{
		"a status read that waits for the lock answered only after", // the counts, or the lock let go
		"nats.go's connection lock was free once the calls had answered",
		// main's RequireLockHeld, for the overlay control at main (forced-attempt.sh); no tree with
		// this file's own helpers prints it.
		"its connection lock was not held when the calls answered",
	} {
		if strings.Contains(text, words) {
			return
		}
	}
	t.Fatalf("the child failed, but not by AnswersWithinAttempt's words:\n%s", text)
}

// TestAnswersWithinAttemptChild is the child of
// TestAnswersWithinAttemptFailsACallThatWaitsOutTheAttempt: it measures a call that waits for nats.go's
// lock. It runs only when the parent re-executes this binary.
func TestAnswersWithinAttemptChild(t *testing.T) {
	if os.Getenv(attemptChildEnv) == "" {
		t.Skip(attemptChildEnv + " is not set: this test is the child of TestAnswersWithinAttemptFailsACallThatWaitsOutTheAttempt")
	}
	hold := StartHandshakeHold(t, URL(t))
	conn := heldAttemptConn(t, hold, heldAttemptTimeout)
	hold.HoldReconnect(t, conn)
	t.Log(attemptHeld)
	hold.AnswersWithinAttempt(t, conn, "a status read that waits for the lock", func() error {
		conn.Status()
		return nil
	})
	t.Log("AnswersWithinAttempt passed a call that waited out the held attempt")
}
