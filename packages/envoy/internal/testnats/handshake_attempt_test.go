package testnats

import (
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"
)

// heldAttemptConn connects to hold's URL with a client whose handshake attempts last
// heldAttemptTimeout and come heldAttemptGap apart, as a bus client's do at 5 s and 1 s, and
// reconnects for as long as the test runs. The hold is released before the client closes, since
// nats.go's Close waits for the connection lock a held handshake holds.
func heldAttemptConn(t *testing.T, hold *HandshakeHold) *natsgo.Conn {
	t.Helper()
	conn, err := natsgo.Connect(hold.URL(),
		natsgo.Timeout(heldAttemptTimeout),
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
	// heldAttemptTimeout is the client's connect timeout, how long each held attempt keeps nats.go's
	// connection lock.
	heldAttemptTimeout = 2 * time.Second
	// heldAttemptGap is the client's ReconnectWait, how long nats.go waits between two attempts with
	// its lock free.
	heldAttemptGap = time.Second
)

// A call that needs no connection lock answers within the attempt held when it began, and
// AnswersWithinAttempt passes it: no later attempt reached the hold, nats.go did not let its lock go,
// and the lock is taken once the call has answered.
func TestAnswersWithinAttemptPassesACallThatNeedsNoLock(t *testing.T) {
	hold := StartHandshakeHold(t, URL(t))
	conn := heldAttemptConn(t, hold)
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
// whenever a read of the status is slow passes the call when that read is descheduled, and the child
// then passes, which fails this row. The child runs in a binary of its own, so its failure is read
// rather than taken as this row's.
func TestAnswersWithinAttemptFailsACallThatWaitsOutTheAttempt(t *testing.T) {
	child := exec.Command(os.Args[0], "-test.run=^TestAnswersWithinAttemptChild$", "-test.timeout=2m", "-test.v")
	child.Env = append(os.Environ(), attemptChildEnv+"=1")
	output, err := child.CombinedOutput()
	text := string(output)
	if err == nil {
		t.Fatalf("AnswersWithinAttempt passed a call that waited out the held attempt: the child passed:\n%s", text)
	}
	if strings.Contains(text, "panic:") || !strings.Contains(text, attemptHeld) || !strings.Contains(text, "--- FAIL: TestAnswersWithinAttemptChild (") {
		t.Fatalf("the child did not fail its measurement after its attempt was held:\n%s", text)
	}
	for _, words := range []string{
		"a status read that waits for the lock answered only after", // the attempt count, or the lock let go
		"its connection lock was not held when the calls answered",  // a check of the lock alone
		"nats.go's connection lock was free once the calls had answered",
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
	conn := heldAttemptConn(t, hold)
	hold.HoldReconnect(t, conn)
	t.Log(attemptHeld)
	hold.AnswersWithinAttempt(t, conn, "a status read that waits for the lock", func() error {
		conn.Status()
		return nil
	})
	t.Log("AnswersWithinAttempt passed a call that waited out the held attempt")
}
