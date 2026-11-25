package supervise

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"
)

// begunCut is a write the claim-write bound cut after the database was handed it, as the store
// answers it: the deadline, under an error that answers WriteBegun (store's writeBegun).
type begunCut struct{ err error }

func (c begunCut) Error() string    { return c.err.Error() }
func (c begunCut) Unwrap() error    { return c.err }
func (begunCut) WriteBegun() bool   { return true }
func cutAfterBegin(err error) error { return begunCut{err} }

// cutStore is the store double whose next PutClaim calls are cut by the claim-write bound: each
// answers as the store wraps a write its bound ended (context.DeadlineExceeded), and records the
// call as countingStore does. begun says whether the cut came after the database was handed the
// write (the error answers WriteBegun), or before (a dial, a wait in the pool's queue, a BEGIN: no
// mark). Every other call is the store's.
type cutStore struct {
	*countingStore
	mu    sync.Mutex
	cuts  int
	begun bool
}

// cutNext makes the next n PutClaim calls answer as writes the bound cut short, after the
// database was handed them or before, as begun says.
func (s *cutStore) cutNext(n int, begun bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cuts, s.begun = n, begun
}

func (s *cutStore) PutClaim(ctx context.Context, c Claim) error {
	s.mu.Lock()
	cut, begun := s.cuts > 0, s.begun
	if cut {
		s.cuts--
	}
	s.mu.Unlock()
	if !cut {
		return s.countingStore.PutClaim(ctx, c)
	}
	s.record("PutClaim")
	err := fmt.Errorf("put claim %s: %w", c.Token, context.DeadlineExceeded)
	if begun {
		return cutAfterBegin(err)
	}
	return err
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

// One claim write the claim-write bound cut after the database was handed it (a database that
// answered that write past defaultClaimWrite) is made again in the same decision, so the decision
// lands: one slow write costs a launch no probe interval and a boot's release no orphan sweep
// (dispatch://INFRA-3904). A second such cut leaves the write owed, the decision failing with it,
// as every write the database did not take is (owes, repair). A cut before the database was handed
// the write — a dial, a wait in the pool's queue, a BEGIN, none of which the store marks
// WriteBegun — is not made again, nor is a cut the daemon's stop made, nor a broken connection
// (TestTheWriteATurnStartOwedIsMadeAtTheClaimsNextDecision).
func TestAClaimWriteTheBoundCutShortOnceIsMadeAgainInTheSameDecision(t *testing.T) {
	t.Run("one cut after the write began: the write is made again and the decision lands", func(t *testing.T) {
		h, cut := newCutHarness(t)
		h.reach(StateReady)
		attempted := cut.mark()
		cut.cutNext(1, true)

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

	t.Run("two cuts after the write began: the write is owed and the decision fails", func(t *testing.T) {
		h, cut := newCutHarness(t)
		h.reach(StateReady)
		attempted := cut.mark()
		cut.cutNext(2, true)

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

	t.Run("a cut before the database was handed the write is owed at once", func(t *testing.T) {
		h, cut := newCutHarness(t)
		h.reach(StateReady)
		attempted := cut.mark()
		cut.cutNext(1, false)

		if err := h.handle(StreamTurnStart{Claim: testToken}); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("the turn start whose write the bound cut before the database had it = %v, want the cut", err)
		}
		if asked := cut.writes()[attempted:]; !slices.Equal(asked, []string{"PutClaim"}) {
			t.Fatalf("the decision asked for %v, want the one claim write: no database was handed it", asked)
		}
		if h.m.owedWrite == nil {
			t.Fatal("the cut write left no write owed")
		}
	})

	t.Run("a cut while the daemon stops is not made again", func(t *testing.T) {
		h, cut := newCutHarness(t)
		h.reach(StateReady)
		attempted := cut.mark()
		cut.cutNext(1, true)
		h.shutdown()

		if err := h.handle(StreamTurnStart{Claim: testToken}); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("the turn start whose write the bound cut while the daemon stops = %v, want the cut", err)
		}
		if asked := cut.writes()[attempted:]; !slices.Equal(asked, []string{"PutClaim"}) {
			t.Fatalf("the decision asked for %v, want the one claim write: the daemon is going", asked)
		}
	})
}

// silentStore is the store double of a database that answers no claim write: each PutClaim waits
// out its bound and answers the deadline, after the database was handed the write or before it, as
// begun says. It counts its calls.
type silentStore struct {
	Store
	begun bool
	mu    sync.Mutex
	calls int
}

func (s *silentStore) PutClaim(ctx context.Context, c Claim) error {
	s.mu.Lock()
	s.calls++
	s.mu.Unlock()
	<-ctx.Done()
	err := fmt.Errorf("put claim %s: %w", c.Token, ctx.Err())
	if s.begun {
		return cutAfterBegin(err)
	}
	return err
}

// What a database that answers no claim write costs a decision, the time the decision holds the
// machine's lock: one claim-write bound for a write it was never handed (a dial, a queue's wait, a
// BEGIN), and two for a write it was handed, the one try again (claimWriteTries). The bound here is
// 200 ms, so the measured holds are about 200 ms and 400 ms; in production, with defaultClaimWrite,
// they are about 5 s and 10 s. The row reads the count of tries; the times are logged for the pull
// request, not judged.
func TestWhatADatabaseThatAnswersNoWriteCostsADecision(t *testing.T) {
	const bound = 200 * time.Millisecond
	for _, tc := range []struct {
		name  string
		begun bool
		tries int
	}{
		{name: "a write the database was never handed", begun: false, tries: 1},
		{name: "a write the database was handed", begun: true, tries: 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newBareHarness(t)
			silent := &silentStore{Store: h.store, begun: tc.begun}
			h.deps.Store = silent
			h.deps.Timeouts.ClaimWrite = bound
			h.deps.BoundWrite = nil
			c := queuedClaim()
			if err := h.store.PutClaim(h.ctx, c); err != nil {
				t.Fatal(err)
			}
			h.start(c)

			started := time.Now()
			err := h.handle(RequestSpawn{Claim: testToken})
			held := time.Since(started)
			silent.mu.Lock()
			calls := silent.calls
			silent.mu.Unlock()
			t.Logf("the decision held the machine's lock %s over %d claim write(s) under a %s bound: %v", held.Round(time.Millisecond), calls, bound, err)
			if calls != tc.tries {
				t.Fatalf("the decision made its claim write %d time(s), want %d", calls, tc.tries)
			}
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("the decision = %v, want the cut", err)
			}
		})
	}
}

// cutReleaseStore commits a release and then answers it as a write the claim-write bound cut after
// the database was handed it, as a commit whose answer the deadline cut off is, counting the calls.
type cutReleaseStore struct {
	Store
	mu    sync.Mutex
	calls int
}

func (s *cutReleaseStore) ReleaseHolds(ctx context.Context, c Claim, holds []Hold) ([]Hold, error) {
	s.mu.Lock()
	s.calls++
	s.mu.Unlock()
	if _, err := s.Store.ReleaseHolds(ctx, c, holds); err != nil {
		return nil, err
	}
	return nil, cutAfterBegin(fmt.Errorf("release the names of %s: %w", c.Token, context.DeadlineExceeded))
}

// A release is made once, whatever cut it (writeOnce): a second try after a cut whose first try
// freed the names would free none and answer an empty Freed as a success, so the names the first
// try freed would be told to nobody. The release answers the cut instead.
func TestAReleaseTheBoundCutIsAnsweredAsTheCutNotMadeAgain(t *testing.T) {
	h := newHarness(t)
	cut := &cutReleaseStore{Store: h.store}
	h.deps.Store = cut
	h.start(h.store.load(h.token))
	h.reach(StateReady)
	h.must(RequestRegister{Claim: h.token, Generation: h.generation(), Session: session, SessionFile: "/state/sessions/second.jsonl",
		CapabilityHash: []byte("capability"), PluginContract: h.deps.PluginContract})

	released, err := h.m.ReleaseNames(h.ctx)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("the release whose answer the bound cut = %+v, %v; want the cut", released, err)
	}
	if cut.calls != 1 {
		t.Fatalf("the release was made %d times, want once: a second try would answer the names freed as none", cut.calls)
	}
}
