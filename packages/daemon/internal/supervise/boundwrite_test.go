package supervise

import (
	"context"
	"sync"
	"testing"
	"time"
)

// boundWriteKey marks a context Deps.BoundWrite made, so a store can tell it from one the
// machine bounded itself.
type boundWriteKey struct{}

// boundSeeingStore records, for each claim write a decision makes, whether its context came from
// Deps.BoundWrite and how long its deadline had left when the write began.
type boundSeeingStore struct {
	Store
	mu   sync.Mutex
	from []bool
	left []time.Duration
}

func (s *boundSeeingStore) PutClaim(ctx context.Context, c Claim) error {
	_, from := ctx.Value(boundWriteKey{}).(bool)
	left := time.Duration(-1)
	if deadline, ok := ctx.Deadline(); ok {
		left = time.Until(deadline)
	}
	s.mu.Lock()
	s.from, s.left = append(s.from, from), append(s.left, left)
	s.mu.Unlock()
	return s.Store.PutClaim(ctx, c)
}

// A decision's claim write takes its bound from Deps.BoundWrite, with both of the machine's
// bounds: the wait in the store's pool queue (defaultClaimAcquire) and the write once the pool has
// handed it a connection (Timeouts.ClaimWrite). The daemon's BoundWrite (store.BoundAfterAcquire)
// counts the write from its connection, so a wave of claim writes on a pool of a few connections no
// longer cuts the last of them for the queue ahead (defaultClaimWrite). With no BoundWrite the
// machine bounds the whole write by the claim-write bound, as it always did. Here a spawn writes
// the claim launching. Every other bounded call of a decision goes through the same helper
// (Machine.bounded), the one place either bound is read.
func TestADecisionsClaimWriteTakesItsBoundFromDepsBoundWrite(t *testing.T) {
	const acquire, write = defaultClaimAcquire, 3 * time.Second
	spawned := func(t *testing.T, bound func(context.Context, time.Duration, time.Duration) (context.Context, context.CancelFunc)) *boundSeeingStore {
		t.Helper()
		h := newBareHarness(t)
		c := queuedClaim()
		h.token = c.Token
		if err := h.store.PutClaim(h.ctx, c); err != nil {
			t.Fatal(err)
		}
		h.deps.Timeouts.ClaimWrite = write
		h.deps.BoundWrite = bound
		seeing := &boundSeeingStore{Store: h.store}
		h.deps.Store = seeing
		h.start(c)
		h.launch()
		seeing.mu.Lock()
		defer seeing.mu.Unlock()
		if len(seeing.from) == 0 {
			t.Fatal("the spawn wrote no claim, so this proves nothing")
		}
		return seeing
	}

	t.Run("BoundWrite set: every write's context is its, with both bounds", func(t *testing.T) {
		var mu sync.Mutex
		var asked [][2]time.Duration
		seeing := spawned(t, func(ctx context.Context, a, w time.Duration) (context.Context, context.CancelFunc) {
			mu.Lock()
			asked = append(asked, [2]time.Duration{a, w})
			mu.Unlock()
			return context.WithCancel(context.WithValue(ctx, boundWriteKey{}, true))
		})
		for i, from := range seeing.from {
			if !from {
				t.Fatalf("claim write %d of the spawn ran on a context BoundWrite did not make", i+1)
			}
		}
		mu.Lock()
		defer mu.Unlock()
		for _, got := range asked {
			if got != [2]time.Duration{acquire, write} {
				t.Fatalf("BoundWrite was asked for acquire %s and write %s, want the machine's %s and %s", got[0], got[1], acquire, write)
			}
		}
	})

	t.Run("BoundWrite unset: the claim-write bound over the whole write", func(t *testing.T) {
		seeing := spawned(t, nil)
		for i, left := range seeing.left {
			if seeing.from[i] || left <= 0 || left > write {
				t.Fatalf("claim write %d of the spawn had %s left on its deadline (BoundWrite's: %v), want at most the %s claim-write bound", i+1, left, seeing.from[i], write)
			}
		}
	})
}
