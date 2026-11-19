package ctxlock_test

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"

	"github.com/sjawhar/envoy/internal/ctxlock"
)

// TestLockContextTakesAFreeLockAndWaitsForAHeldOneOnlyWhileItsContextLives: LockContext takes a
// free lock even on an ended context, as sync.Mutex's Lock would; a held lock on an ended context
// answers that context's error and leaves the caller holding nothing; and a caller waiting on a
// live context gets the lock once its holder unlocks it. Each case runs in a synctest bubble, so
// the waiter is waiting (synctest.Wait) before the holder lets go.
func TestLockContextTakesAFreeLockAndWaitsForAHeldOneOnlyWhileItsContextLives(t *testing.T) {
	cases := []struct {
		name    string
		held    bool // another holder has m locked when LockContext is called
		ended   bool // LockContext's context has ended before the call
		release bool // the holder unlocks m once LockContext waits
		want    error
	}{
		{name: "a free lock is taken on an ended context", ended: true},
		{name: "a held lock on an ended context answers its error", held: true, ended: true, want: context.Canceled},
		{name: "a waiter gets the lock when its holder unlocks it", held: true, release: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				var m ctxlock.Mutex
				if c.held {
					m.Lock()
				}
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				if c.ended {
					cancel()
				}
				answered := make(chan error, 1)
				go func() { answered <- m.LockContext(ctx) }()
				synctest.Wait()
				if c.release {
					m.Unlock()
					synctest.Wait()
				}
				select {
				case err := <-answered:
					if !errors.Is(err, c.want) {
						t.Fatalf("LockContext = %v; want %v", err, c.want)
					}
				default:
					t.Fatal("LockContext is still waiting")
				}

				// Whether the caller holds m: once a holder still holding m lets go, nobody else can,
				// and a lock on an ended context succeeds only on a free m.
				if c.held && !c.release {
					m.Unlock()
				}
				done, end := context.WithCancel(context.Background())
				end()
				holds := m.LockContext(done) != nil
				if !holds {
					m.Unlock()
				}
				if wantHolds := c.want == nil; holds != wantHolds {
					t.Fatalf("after LockContext answered %v, the caller holds m: %v; want %v", c.want, holds, wantHolds)
				}
			})
		})
	}
}
