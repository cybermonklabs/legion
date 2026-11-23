package session

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/testnats"
)

// openUnopenedStore is a known-session store over a connection through proxy whose user may not
// open the bucket, so the store's background retry runs (armRepair) and attempts every
// retryFirst. ctx bounds the store's first attempt (OpenKnownSessions), and extra adds to the
// store's options.
func openUnopenedStore(t *testing.T, ctx context.Context, extra ...KnownSessionsOption) (*KnownSessions, *testnats.Proxy, *nats.Conn) {
	t.Helper()
	seed, public := testnats.User(t)
	grant := testnats.StartNkeyGranted(t, public, "nothing.this.store.uses")
	proxy := testnats.StartProxy(t, grant.URL)
	t.Cleanup(proxy.Restore)
	conn, err := testnats.ConnectSeed(t, proxy.URL(), seed)
	if err != nil {
		t.Fatalf("connect through the proxy as the user whose grant does not carry the bucket: %v", err)
	}
	t.Cleanup(conn.Close)
	options := []KnownSessionsOption{
		WithKnownSessionReplicas(1),
		WithKnownSessionLogger(slog.New(slog.NewTextHandler(&lockedWriter{}, nil))),
		withKnownSessionRetry(10*time.Millisecond, 10*time.Millisecond),
	}
	options = append(options, extra...)
	known, err := OpenKnownSessions(ctx, conn, options...)
	if err != nil {
		t.Fatalf("open the store over a bucket its user may not open: %v", err)
	}
	t.Cleanup(known.StopWatch)
	if !repairRunning(known) {
		t.Fatal("the store's background retry is not running; the row needs it")
	}
	return known, proxy, conn
}

// The background retry's attempt lets repairMu go once its window ends, though its request for
// the bucket was never answered: the lookup is bounded by the attempt's context (bus.Bounded), so
// a reconnect hook's move and the next attempt are not held behind it.
//
// The row reads no clock to decide. The test ends each attempt's window itself
// (withKnownSessionAttemptContext), and the retry's lookups wait an hour
// (withKnownSessionOpenWait), past the test's end, with the proxy holding every byte (Freeze) on
// a connection whose user the server drops the lookup for: nothing but the window ends an attempt.
// Each attempt asks for its window while it holds repairMu, and the retry is one goroutine that
// takes repairMu again for its next attempt, so attempt n+1 asking for its window is attempt n
// having let the lock go. The row ends three windows and counts three such releases. A row that
// fails ends at the test's deadline. The store's first attempt is made on a context already ended,
// so it sends nothing and arms the retry at once; with the lookup unbounded (the second control)
// it waits its own knownBucketOpenWait first.
//
// The controls are the attempt run on the store's context rather than its window, and the lookup
// unbounded by the attempt's context: either way attempt 1 holds the lock after its window ends,
// and the row fails at the test's deadline, or, where that deadline is past nats.go's close of the
// frozen connection, when attempt 2 comes only after that close.
func TestTheRetrysAttemptLetsItsLockGoAtItsWindow(t *testing.T) {
	windows := make(chan context.CancelFunc)
	ended, endOpen := context.WithCancel(t.Context())
	endOpen()
	_, proxy, conn := openUnopenedStore(t, ended,
		withKnownSessionOpenWait(time.Hour),
		withKnownSessionAttemptContext(func(ctx context.Context) (context.Context, context.CancelFunc) {
			attempt, end := context.WithCancel(ctx)
			select {
			case windows <- end:
			case <-ctx.Done():
			}
			return attempt, end
		}))
	proxy.Freeze()
	// next is attempt n asking for its window. An attempt the connection's close ended (nats.go
	// closes a connection whose pings go unanswered, about 6 min into the freeze) says nothing of
	// the window, so attempt n must come while the connection is still open.
	next := func(n int) context.CancelFunc {
		t.Helper()
		select {
		case end := <-windows:
			if conn.IsClosed() {
				t.Fatalf("attempt %d asked for its window after the connection closed: the close, not the window, ended attempt %d", n, n-1)
			}
			return end
		case <-time.After(testnats.Remaining(t)):
			if n == 1 {
				t.Fatal("the retry's first attempt never asked for its window")
			}
			t.Fatalf("attempt %d never asked for its window by the test's deadline: attempt %d still held repairMu after the test ended its window, though its lookup was never answered", n, n-1)
			return nil
		}
	}
	end := next(1)
	for n := 1; n <= 3; n++ {
		end()
		end = next(n + 1)
	}
	end()
}

// An arm of the background repair made while the retry's attempt holds repairMu returns at once:
// arming reads and writes only the retry's state (repairStateMu), never the attempt's lock, so the
// open of a bucket that failed and a move's failure arm or ask again without waiting for an attempt
// a handshake holds. An arm made while the retry runs asks it for one more attempt (rearm). The row
// holds repairMu as the retry's attempt would.
//
// The control is armRepair taking repairMu: it waits for the lock, and the row fails.
func TestAnArmWhileTheRetrysAttemptHoldsItsLockReturnsAtOnce(t *testing.T) {
	known, _, _ := openUnopenedStore(t, t.Context())
	known.repairMu.Lock()
	unlocked := false
	t.Cleanup(func() {
		if !unlocked {
			known.repairMu.Unlock()
		}
	})
	armed := make(chan struct{})
	go func() {
		known.armRepair()
		close(armed)
	}()
	select {
	case <-armed:
	case <-time.After(testnats.Remaining(t)):
		known.repairMu.Unlock()
		unlocked = true
		t.Fatal("armRepair had not returned by the test's deadline, called while repairMu was held: it waited for the retry's attempt")
	}
	known.repairStateMu.Lock()
	asked := known.rearm
	known.repairStateMu.Unlock()
	if !asked {
		t.Fatal("an arm made while the retry ran did not ask it for another attempt")
	}
}
