package session

import (
	"context"
	"errors"
	"log/slog"
	"testing"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/testnats"
	"github.com/sjawhar/envoy/testnkeys"
)

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
				known, err := OpenKnownSessions(client.Conn, WithKnownSessionReplicas(1), withKnownSessionRetry(time.Hour, time.Hour))
				if err != nil {
					t.Fatalf("open the known-session store: %v", err)
				}
				t.Cleanup(known.StopWatch)
				if err := known.WaitForCacheReady(context.Background()); err != nil {
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
				seed, public := testnkeys.User(t)
				grant := testnats.StartNkeyGranted(t, public, "nothing.this.store.uses")
				conn, err := testnats.ConnectSeed(t, grant.URL, seed)
				if err != nil {
					t.Fatalf("connect as the user whose grant does not carry the bucket: %v", err)
				}
				t.Cleanup(conn.Close)
				known, err := OpenKnownSessions(conn,
					WithKnownSessionReplicas(1),
					WithKnownSessionLogger(slog.New(slog.NewTextHandler(&lockedWriter{}, nil))),
					// The retry the open arms waits an hour, so only the Rewatch's wake moves it.
					withKnownSessionRetry(time.Hour, time.Hour))
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

			ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
			defer cancel()
			started := time.Now()
			rewatched := make(chan error, 1)
			go func() { rewatched <- known.Rewatch(ctx, conn) }()
			select {
			case err := <-rewatched:
				elapsed := time.Since(started)
				if want := tc.returns(ctx); !errors.Is(err, want) {
					t.Fatalf("Rewatch returned %v after %s, want %v", err, elapsed, want)
				}
				t.Logf("Rewatch returned %v %s after it began, with the retry's lock held", err, elapsed)
			case <-time.After(time.Second + 100*time.Millisecond):
				t.Fatal("Rewatch did not return within 1s of its run's end: it waited for the lock the retry's attempt holds")
			}
		})
	}
}
