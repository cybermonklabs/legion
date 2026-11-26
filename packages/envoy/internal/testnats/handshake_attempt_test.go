package testnats

import (
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
)

// A call that needs no connection lock answers within the attempt held when it began, and
// AnswersWithinAttempt passes it: the lock is taken once the call has answered, nats.go did not let
// it go, and no later attempt reached the hold. The client's connect timeout is what is left until
// the test's waits give up (left), the one HoldReconnect gives it again (OwnConnectTimeout), so no
// span of the attempt ends the row; the cleanup's Release ends the attempt.
func TestAnswersWithinAttemptPassesACallThatNeedsNoLock(t *testing.T) {
	hold := StartHandshakeHold(t, URL(t))
	conn := heldAttemptConn(t, hold, left(t))
	hold.HoldReconnect(t, conn)
	hold.AnswersWithinAttempt(t, conn, "a call that needs no lock", func() error { return nil })
}

// attemptChildEnv tells the re-executed test binary to run TestAnswersWithinAttemptChild, which
// skips in an ordinary run.
const attemptChildEnv = "TESTNATS_ATTEMPT_CHILD"

// attemptHeld is the line the child logs once its attempt is held (AwaitAttempt), before it measures.
const attemptHeld = "the child's handshake attempt is held"

// AnswersWithinAttempt fails a call that waits out the attempt held when it began. The child's call
// reads conn's status, which waits for nats.go's connection lock, so it answers only once the held
// attempt has ended at the client's connect timeout, in the second before the next attempt; the
// child must fail there by AnswersWithinAttempt's own words. A check that takes the lock to be held
// whenever a read of the status is slow passes the call when that read is descheduled, and so does
// one that reads the attempt count before the lock when the check is held up past the gap; the child
// then passes, which fails this row. The child runs in a binary of its own, so its failure is read
// rather than taken as this row's. Its -timeout ends where this row's waits give up
// (testwait.GiveUpAt), so the child's own waits give up testwait's margin before that and name
// what they waited for; the context that ends it is this row's shared wait's (testwait.Context),
// which kills a child still running at the give-up, and the row then fails on what it printed.
// Under -timeout 0 neither has an end.
func TestAnswersWithinAttemptFailsACallThatWaitsOutTheAttempt(t *testing.T) {
	childTimeout := "0"
	if end, bounded := testwait.GiveUpAt(t); bounded {
		childTimeout = time.Until(end).String()
	}
	child := exec.CommandContext(testwait.Context(t), os.Args[0], "-test.run=^TestAnswersWithinAttemptChild$", "-test.timeout="+childTimeout, "-test.v")
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
// lock. It holds the attempt with Hold and AwaitAttempt rather than HoldReconnect, which would give
// the client a connect timeout of the test's own (OwnConnectTimeout), so the client keeps its own,
// heldAttemptTimeout, and the held attempt ends at it. It runs only when the parent re-executes this
// binary.
func TestAnswersWithinAttemptChild(t *testing.T) {
	if os.Getenv(attemptChildEnv) == "" {
		t.Skip(attemptChildEnv + " is not set: this test is the child of TestAnswersWithinAttemptFailsACallThatWaitsOutTheAttempt")
	}
	hold := StartHandshakeHold(t, URL(t))
	conn := heldAttemptConn(t, hold, heldAttemptTimeout)
	before := hold.Attempts()
	hold.Hold()
	hold.AwaitAttempt(t, conn, before)
	t.Log(attemptHeld)
	hold.AnswersWithinAttempt(t, conn, "a status read that waits for the lock", func() error {
		conn.Status()
		return nil
	})
	t.Log("AnswersWithinAttempt passed a call that waited out the held attempt")
}
