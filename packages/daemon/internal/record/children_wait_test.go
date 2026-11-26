package record

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/phase"
)

// A parent's wait on its open children round-trips whole: the held start it keeps in
// awaiting_children (its phase, task, reason, when the wait began and the children that held it),
// and the mark that outlives the phase, when the last wait began. Leaving the phase takes the held
// start with it and keeps the outliving mark; a plain issue carries neither.
func TestStoreRoundTripsAParentsChildrenWait(t *testing.T) {
	ctx := context.Background()
	st := migratedStore(t)
	records := NewStore()
	since := time.Date(2026, 11, 14, 10, 0, 0, 0, time.UTC)
	waitedAt := time.Date(2026, 11, 14, 10, 0, 5, 0, time.UTC)

	waiting := issueFixture("LEGION-220")
	waiting.Phase = phase.AwaitingChildren
	waiting.ChildrenWait = &ChildrenWait{Start: phase.Implementing, Task: "merge the base and finish",
		Reason: "a child of it is open", Since: since, Children: []string{"LEGION-230", "LEGION-231"}}
	waiting.ChildrenWaitedAt = &waitedAt

	inTx(t, st, func(tx pgx.Tx) {
		must(t, records.PutIssue(ctx, tx, waiting))
		got, err := records.Issue(ctx, tx, waiting.Key)
		must(t, err)
		if got.Phase != phase.AwaitingChildren {
			t.Fatalf("phase = %s, want awaiting_children", got.Phase)
		}
		if got.ChildrenWait == nil {
			t.Fatalf("the held start reads back nil, want it kept with the phase")
		}
		w := got.ChildrenWait
		if w.Start != phase.Implementing || w.Task != waiting.ChildrenWait.Task ||
			w.Reason != waiting.ChildrenWait.Reason || !w.Since.Equal(since) || !slices.Equal(w.Children, waiting.ChildrenWait.Children) {
			t.Fatalf("held start = %+v, want %+v", w, waiting.ChildrenWait)
		}
		if got.ChildrenWaitedAt == nil || !got.ChildrenWaitedAt.Equal(waitedAt) {
			t.Fatalf("children waited at = %v, want %s", got.ChildrenWaitedAt, waitedAt)
		}
	})

	// A parent that returned from the wait keeps the outliving mark and carries no held start: the
	// return clears the held-start columns, the children among them.
	returned := waiting
	returned.Phase = phase.Implementing
	returned.ChildrenWait = nil
	inTx(t, st, func(tx pgx.Tx) {
		must(t, records.PutIssue(ctx, tx, returned))
		got, err := records.Issue(ctx, tx, returned.Key)
		must(t, err)
		if got.ChildrenWait != nil {
			t.Fatalf("held start after the return = %+v, want nil", got.ChildrenWait)
		}
		if got.ChildrenWaitedAt == nil || !got.ChildrenWaitedAt.Equal(waitedAt) {
			t.Fatalf("after the return children_waited_at = %v, want it to outlive the phase", got.ChildrenWaitedAt)
		}
	})

	plain := issueFixture("LEGION-221")
	inTx(t, st, func(tx pgx.Tx) {
		must(t, records.PutIssue(ctx, tx, plain))
		got, err := records.Issue(ctx, tx, plain.Key)
		must(t, err)
		if got.ChildrenWait != nil || got.ChildrenWaitedAt != nil {
			t.Fatalf("a plain issue = %+v, want no held start and no wait mark", got)
		}
	})
}

// A parent in awaiting_children holds none of the cap's slots, reads as not at work, and carries
// its run. The three reads are what admit's releaseSlots, blockers.waitsOf and the engine's close
// read of the phase, and the one thing that must be true of each is stated here.
func TestAwaitingChildrenHoldsNoSlotIsNotAtWorkAndCarriesItsRun(t *testing.T) {
	waiting := Issue{Key: "LEGION-231", Tree: "LEGION-230", Project: "LEGION", Phase: phase.AwaitingChildren,
		Status: "in_progress", Parent: ParentOf("LEGION-230")}
	if MainRule.PhaseKeepsSlot(phase.AwaitingChildren) {
		t.Errorf("PhaseKeepsSlot(awaiting_children) = true, want false: a parent waiting on its children holds no slot")
	}
	if KeepsSlot(waiting) {
		t.Errorf("KeepsSlot = true, want false: the phase keeps none of the cap's slots")
	}
	if AtWork(waiting, TreeGate{}) {
		t.Errorf("AtWork(awaiting_children) = true, want false: no worker of it has work in the phase")
	}
	if !waiting.CarriesRun() {
		t.Errorf("CarriesRun = false, want true: a parent in awaiting_children started a phase")
	}
}
