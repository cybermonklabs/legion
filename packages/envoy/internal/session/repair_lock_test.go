package session

import (
	"context"
	"errors"
	"log/slog"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/clock/clocktest"
	"github.com/sjawhar/envoy/internal/testnats"
	"github.com/sjawhar/envoy/internal/testwait"
)

// rewatchesWaitingForTheLock counts the goroutines parked in a KnownSessions.Rewatch on the
// store's repairMu (ctxlock's LockContext), which is where a Rewatch waits while the retry's
// attempt holds the lock.
func rewatchesWaitingForTheLock() int {
	buf := make([]byte, 1<<20)
	for {
		n := runtime.Stack(buf, true)
		if n < len(buf) {
			buf = buf[:n]
			break
		}
		buf = make([]byte, 2*len(buf))
	}
	count := 0
	for _, stack := range strings.Split(string(buf), "\n\n") {
		if strings.Contains(stack, "ctxlock.(*Mutex).LockContext") && strings.Contains(stack, "session.(*KnownSessions).Rewatch") {
			count++
		}
	}
	return count
}

// The background retry holds repairMu through each of its attempts (retryRepair), and an attempt's
// request takes no context, so on a connection that dropped with it unanswered it waits out its
// own bound. A reconnect hook's Rewatch waits for that lock only while its run lasts (LockContext):
// a run that ends first returns at once, so it holds no other hook of the run behind it. That
// holds for a store whose watcher is open, whose move (repairOnce) answers the run's error, and for
// one whose bucket never opened, which only wakes the retry and arms it (armRepairWithin) and
// answers nil. The test holds repairMu as the retry's attempt would.
func TestTheHooksMoveDoesNotWaitOutTheRetrysAttempt(t *testing.T) {
	for _, tc := range []struct {
		name string
		// open is the store and the connection its Rewatch is handed.
		open func(t *testing.T) (*KnownSessions, *nats.Conn)
		// returns is what Rewatch must answer once ctx has ended.
		returns func(ctx context.Context) error
	}{
		{
			name: "a store whose watcher is open",
			open: func(t *testing.T) (*KnownSessions, *nats.Conn) {
				client := setupNATS(t)
				// The retry's waits are on a clock nothing moves, so no attempt of its own runs.
				known, err := OpenKnownSessions(client.Conn, WithKnownSessionReplicas(1), withKnownSessionTimers(clocktest.New(time.Now())))
				if err != nil {
					t.Fatalf("open the known-session store: %v", err)
				}
				t.Cleanup(known.StopWatch)
				if err := known.WaitForCacheReady(testwait.Context(t)); err != nil {
					t.Fatalf("the known-session cache did not become ready: %v", err)
				}
				if known.watch() == nil {
					t.Fatal("the store has no watcher; the case needs one open")
				}
				return known, client.Conn
			},
			returns: func(ctx context.Context) error { return ctx.Err() },
		},
		{
			name: "a store whose bucket never opened",
			open: func(t *testing.T) (*KnownSessions, *nats.Conn) {
				seed, public := testnats.User(t)
				grant := testnats.StartNkeyGranted(t, public, "nothing.this.store.uses")
				conn, err := testnats.ConnectSeed(t, grant.URL, seed)
				if err != nil {
					t.Fatalf("connect as the user whose grant does not carry the bucket: %v", err)
				}
				t.Cleanup(conn.Close)
				known, err := OpenKnownSessions(conn,
					WithKnownSessionReplicas(1),
					WithKnownSessionLogger(slog.New(slog.NewTextHandler(&lockedWriter{}, nil))),
					// The retry the open arms waits on a clock nothing moves, so only the Rewatch's
					// wake moves it.
					withKnownSessionTimers(clocktest.New(time.Now())))
				if err != nil {
					t.Fatalf("open the store over a bucket its user may not open: %v", err)
				}
				t.Cleanup(known.StopWatch)
				if known.watch() != nil {
					t.Fatal("the store opened a bucket its user may not open; the case needs none open")
				}
				return known, conn
			},
			returns: func(context.Context) error { return nil },
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			known, conn := tc.open(t)
			known.repairMu.Lock()
			var unlocked bool
			unlock := func() {
				if !unlocked {
					unlocked = true
					known.repairMu.Unlock()
				}
			}
			// Registered after the store's StopWatch, so it runs first.
			t.Cleanup(unlock)

			// The run's end is the test's to choose: once the Rewatch waits for the lock the retry's
			// attempt holds, the test ends the run, and the Rewatch must return while the lock is
			// still held, which it is until the test's cleanup.
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			waiting := rewatchesWaitingForTheLock()
			rewatched := make(chan error, 1)
			go func() { rewatched <- known.Rewatch(ctx, conn) }()
			testwait.Eventually(t, "the Rewatch to wait for the lock the retry's attempt holds", func() bool {
				return rewatchesWaitingForTheLock() > waiting
			})
			cancel()
			err := testwait.Receive(t, "the Rewatch to return once its run ended, with the retry's lock still held", rewatched)
			if want := tc.returns(ctx); !errors.Is(err, want) {
				t.Fatalf("Rewatch returned %v, want %v", err, want)
			}
		})
	}
}
