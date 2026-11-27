package supervise

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"testing"
)

// begunCut is a write the claim-write bound cut after the database was handed it, as the store
// answers it: the deadline, under the BegunWriteError the store's begunError is.
type begunCut struct{ err error }

var _ BegunWriteError = begunCut{}

func (c begunCut) Error() string  { return c.err.Error() }
func (c begunCut) Unwrap() error  { return c.err }
func (begunCut) WriteBegun() bool { return true }

// The two cuts a claim write can meet, as the store answers them: one the bound made after the
// database answered the write's BEGIN (a BegunWriteError), and one it made before, in a dial, a
// queue's wait or the BEGIN itself (no mark).
var (
	cutAfterTheBegin  error = begunCut{fmt.Errorf("put claim: %w", context.DeadlineExceeded)}
	cutBeforeTheBegin       = fmt.Errorf("put claim: %w", context.DeadlineExceeded)
)

// One claim write the claim-write bound cut after the database was handed it (a database that
// answered that write past defaultClaimWrite) is made again in the same decision, so the decision
// lands: one slow write costs a launch no probe interval and a boot's release no orphan sweep
// (dispatch://INFRA-3904). A second such cut leaves the write owed, the decision failing with it,
// as every write the database did not take is (owes, repair). A cut before the database was handed
// the write — a dial, a wait in the pool's queue, a BEGIN, none of which the store marks — is not
// made again, nor is a cut the daemon's stop made, nor a broken connection
// (TestTheWriteATurnStartOwedIsMadeAtTheClaimsNextDecision). The release that is the exception to
// the retry is TestAReleaseWhoseAnswerIsLostLeavesNoFreedNameCached's.
func TestAClaimWriteTheBoundCutShortOnceIsMadeAgainInTheSameDecision(t *testing.T) {
	t.Run("one cut after the write began: the write is made again and the decision lands", func(t *testing.T) {
		h, counted := newCountedHarness(t)
		h.reach(StateReady)
		attempted := counted.mark()
		counted.failNext(1, cutAfterTheBegin)

		if err := h.handle(StreamTurnStart{Claim: testToken}); err != nil {
			t.Fatalf("the turn start whose first write the bound cut = %v, want it to land", err)
		}
		if asked := counted.writes()[attempted:]; !slices.Equal(asked, []string{"PutClaim", "PutClaim"}) {
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
		h, counted := newCountedHarness(t)
		h.reach(StateReady)
		attempted := counted.mark()
		counted.failNext(2, cutAfterTheBegin)

		if err := h.handle(StreamTurnStart{Claim: testToken}); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("the turn start whose two writes the bound cut = %v, want the cut", err)
		}
		if asked := counted.writes()[attempted:]; !slices.Equal(asked, []string{"PutClaim", "PutClaim"}) {
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
		h, counted := newCountedHarness(t)
		h.reach(StateReady)
		attempted := counted.mark()
		counted.failNext(1, cutBeforeTheBegin)

		if err := h.handle(StreamTurnStart{Claim: testToken}); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("the turn start whose write the bound cut before the database had it = %v, want the cut", err)
		}
		if asked := counted.writes()[attempted:]; !slices.Equal(asked, []string{"PutClaim"}) {
			t.Fatalf("the decision asked for %v, want the one claim write: no database was handed it", asked)
		}
		if h.m.owedWrite == nil {
			t.Fatal("the cut write left no write owed")
		}
	})

	t.Run("a cut while the daemon stops is not made again", func(t *testing.T) {
		h, counted := newCountedHarness(t)
		h.reach(StateReady)
		attempted := counted.mark()
		counted.failNext(1, cutAfterTheBegin)
		h.shutdown()

		if err := h.handle(StreamTurnStart{Claim: testToken}); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("the turn start whose write the bound cut while the daemon stops = %v, want the cut", err)
		}
		if asked := counted.writes()[attempted:]; !slices.Equal(asked, []string{"PutClaim"}) {
			t.Fatalf("the decision asked for %v, want the one claim write: the daemon is going", asked)
		}
	})
}
