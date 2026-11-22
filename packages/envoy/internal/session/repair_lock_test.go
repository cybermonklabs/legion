package session

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/testnats"
	"github.com/sjawhar/envoy/internal/testwait"
)

// The background retry holds repairMu through each of its attempts (retryRepair), and an attempt's
// request takes no context, so on a connection that dropped with it unanswered it waits out its
// own bound. A reconnect hook's Rewatch waits for that lock only while its run lasts (LockContext):
// a run that ends first returns at once, so it holds no other hook of the run behind it. That
// holds for a store whose watcher is open, whose move (repairOnce) answers the run's error, and for
// one whose bucket never opened, which only wakes the retry and arms it (armRepairWithin) and
// answers nil. The test holds repairMu as the retry's attempt would.
//
// Each case runs twice. In the first the run is over before Rewatch is called: its context is
// cancelled, so the move's answer is context.Canceled, which nothing but that run's end gives. A
// wait for the lock bounded some other way, by a request's own timeout (bus.RequestTimeout) for
// one, would answer context.DeadlineExceeded instead. In the second the run ends while Rewatch
// waits for the lock: the row cancels the run once Rewatch has asked its context for Done, which
// it does only when it finds the lock held and waits for it (ctxlock.Mutex.LockContext), so a
// Rewatch that checked the run once and then waited on the lock alone (Lock) never asks, and the
// row ends at the test binary's deadline by name. The retry's lock is held until the row's
// cleanup, after Rewatch has answered.
func TestTheHooksMoveDoesNotWaitOutTheRetrysAttempt(t *testing.T) {
	for _, tc := range []struct {
		name string
		// open is the store and the connection its Rewatch is handed.
		open func(t *testing.T) (*KnownSessions, *nats.Conn)
		// returns is what Rewatch must answer once its run is over.
		returns error
	}{
		{
			name: "a store whose watcher is open",
			open: func(t *testing.T) (*KnownSessions, *nats.Conn) {
				client := setupNATS(t)
				known, err := OpenKnownSessions(t.Context(), client.Conn, WithKnownSessionReplicas(1), withKnownSessionRetry(time.Hour, time.Hour))
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
			returns: context.Canceled,
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
				known, err := OpenKnownSessions(t.Context(), conn,
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
			returns: nil,
		},
	} {
		for _, ending := range []struct {
			name string
			// whileWaiting ends the run once Rewatch waits for the lock, rather than before the call.
			whileWaiting bool
		}{
			{name: "the run over before the call"},
			{name: "the run ending while Rewatch waits for the lock", whileWaiting: true},
		} {
			t.Run(tc.name+"/"+ending.name, func(t *testing.T) {
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

				base, end := context.WithCancel(context.Background())
				defer end()
				run := &askedContext{Context: base, asked: make(chan struct{})}
				if !ending.whileWaiting {
					end()
				}
				started := time.Now()
				rewatched := make(chan error, 1)
				go func() { rewatched <- known.Rewatch(run, conn) }()
				if ending.whileWaiting {
					testwait.Receive(t, "Rewatch to wait for the retry's lock under its run's context", run.asked)
					end()
				}
				// The test holds repairMu until its cleanup, so a Rewatch that waited for the lock the
				// retry's attempt holds never answers: its answer, however late a loaded machine gives
				// it, is the claim (testwait.Receive).
				err := testwait.Receive(t, "Rewatch to return with the retry's lock held", rewatched)
				if !errors.Is(err, tc.returns) {
					t.Fatalf("Rewatch returned %v after %s, want %v", err, time.Since(started), tc.returns)
				}
				t.Logf("Rewatch returned %v %s after it began, with the retry's lock held", err, time.Since(started))
			})
		}
	}
}

// askedContext is a context that tells the test, by closing asked, when its Done channel is first
// asked for.
type askedContext struct {
	context.Context
	asked chan struct{}
	once  sync.Once
}

func (c *askedContext) Done() <-chan struct{} {
	c.once.Do(func() { close(c.asked) })
	return c.Context.Done()
}
