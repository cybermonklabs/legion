package supervise

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"testing"
)

// cutStore is the store double whose next PutClaim calls are cut by the claim-write bound before
// the database answers: each answers as the store wraps a write its bound ended
// (context.DeadlineExceeded), and records the call as countingStore does. Every other call is the
// store's.
type cutStore struct {
	*countingStore
	mu   sync.Mutex
	cuts int
}

// cutNext makes the next n PutClaim calls answer as writes the bound cut short.
func (s *cutStore) cutNext(n int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cuts = n
}

func (s *cutStore) PutClaim(ctx context.Context, c Claim) error {
	s.mu.Lock()
	cut := s.cuts > 0
	if cut {
		s.cuts--
	}
	s.mu.Unlock()
	if cut {
		s.record("PutClaim")
		return fmt.Errorf("put claim %s: %w", c.Token, context.DeadlineExceeded)
	}
	return s.countingStore.PutClaim(ctx, c)
}

// newCutHarness is newCountedHarness over a cutStore.
func newCutHarness(t *testing.T) (*harness, *cutStore) {
	t.Helper()
	h := newBareHarness(t)
	cut := &cutStore{countingStore: &countingStore{Store: h.store}}
	h.deps.Store = cut
	c := queuedClaim()
	if err := h.store.PutClaim(h.ctx, c); err != nil {
		t.Fatal(err)
	}
	h.start(c)
	return h, cut
}

// One claim write the claim-write bound cut short (a database that answered that write past
// defaultClaimWrite) is made again in the same decision, so the decision lands: one slow write
// costs a launch no probe interval and a boot's release no orphan sweep (dispatch://INFRA-3904).
// A second cut in a row leaves the write owed, the decision failing with it, as every write the
// database did not take is (owes, repair). A cut the daemon's stop made is not made again, and a
// broken connection is not either (TestTheWriteATurnStartOwedIsMadeAtTheClaimsNextDecision).
func TestAClaimWriteTheBoundCutShortOnceIsMadeAgainInTheSameDecision(t *testing.T) {
	t.Run("one cut: the write is made again and the decision lands", func(t *testing.T) {
		h, cut := newCutHarness(t)
		h.reach(StateReady)
		attempted := cut.mark()
		cut.cutNext(1)

		if err := h.handle(StreamTurnStart{Claim: testToken}); err != nil {
			t.Fatalf("the turn start whose first write the bound cut = %v, want it to land", err)
		}
		if asked := cut.writes()[attempted:]; !slices.Equal(asked, []string{"PutClaim", "PutClaim"}) {
			t.Fatalf("the decision asked for %v, want the claim write and the one write again", asked)
		}
		h.wantState(StateWorking)
		if stored := h.store.load(testToken); stored.State != StateWorking {
			t.Fatalf("the stored claim reads %s, want working: the write made again landed", stored.State)
		}
		if h.m.owedWrite != nil {
			t.Fatal("a write that landed in its own decision left a write owed")
		}
		if owed := h.records.of(owedWriteLine); len(owed) != 0 {
			t.Fatalf("the write was logged owed %d times, want none: it landed", len(owed))
		}
		if again := h.records.of(claimWriteAgainLine); len(again) != 1 {
			t.Fatalf("the cut write was logged made again %d times, want once", len(again))
		}
	})

	t.Run("two cuts: the write is owed and the decision fails", func(t *testing.T) {
		h, cut := newCutHarness(t)
		h.reach(StateReady)
		attempted := cut.mark()
		cut.cutNext(2)

		if err := h.handle(StreamTurnStart{Claim: testToken}); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("the turn start whose two writes the bound cut = %v, want the cut", err)
		}
		if asked := cut.writes()[attempted:]; !slices.Equal(asked, []string{"PutClaim", "PutClaim"}) {
			t.Fatalf("the decision asked for %v, want the claim write and the one write again, no third", asked)
		}
		if h.m.owedWrite == nil {
			t.Fatal("two cut writes left no write owed")
		}
		if stored := h.store.load(testToken); stored.State != StateReady {
			t.Fatalf("the stored claim reads %s, want ready: no write landed", stored.State)
		}
	})

	t.Run("a cut while the daemon stops is not made again", func(t *testing.T) {
		h, cut := newCutHarness(t)
		h.reach(StateReady)
		attempted := cut.mark()
		cut.cutNext(1)
		h.shutdown()

		if err := h.handle(StreamTurnStart{Claim: testToken}); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("the turn start whose write the bound cut while the daemon stops = %v, want the cut", err)
		}
		if asked := cut.writes()[attempted:]; !slices.Equal(asked, []string{"PutClaim"}) {
			t.Fatalf("the decision asked for %v, want the one claim write: the daemon is going", asked)
		}
	})
}
