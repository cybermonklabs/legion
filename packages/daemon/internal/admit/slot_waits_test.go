package admit

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// The admission pass writes when a slot holder began to wait on another issue while it keeps its
// slot (record.Slot.WaitsSince), the time the slots reading dates the holder state `issue` by: the
// pass's clock at the pass whose judgement first finds the holder waiting, kept by every later
// pass while the wait stands, and cleared by the pass that finds it ended. The rule here keeps the
// slot through a link wait, which is what makes a holder that waits on a link a holder; under the
// daemon's own rule that wait gives the slot back. A holder that waits on nothing is marked
// nothing.
func TestAHolderThatKeepsItsSlotThroughAWaitIsMarkedWhenTheWaitBegan(t *testing.T) {
	pool := migratedPool(t)
	rule := ruleWith("main with a link wait that keeps the slot", func(rule *record.SlotRule) { rule.LinkWaitGivesBack = false })
	admission := ruledAdmission(t, 2, rule, silent())
	seedSlottedInPhase(t, pool, acmeProject, "ACME-1", "a", phase.Implementing, nil)
	seedSlottedInPhase(t, pool, acmeProject, "ACME-2", "b", phase.Implementing, nil)
	seedMirrorIn(t, pool, acmeProject, "ACME-1", "in_progress", "ACME-9")
	seedMirrorIn(t, pool, acmeProject, "ACME-9", "in_progress")
	seedMirrorIn(t, pool, acmeProject, "ACME-2", "in_progress")

	waitsSince := func() map[string]*time.Time {
		t.Helper()
		marks := map[string]*time.Time{}
		for _, slot := range slots(t, pool) {
			marks[slot.Issue] = slot.WaitsSince
		}
		return marks
	}
	began := fixedNow
	tickThrough(t, pool, admission, "tick-1")
	if marks := waitsSince(); len(marks) != 2 || marks["ACME-1"] == nil || !marks["ACME-1"].Equal(began) || marks["ACME-2"] != nil {
		t.Fatalf("after the first pass the slots' waits read %v, want ACME-1 waiting since %s and ACME-2 waiting on nothing", marks, began)
	}

	admission.now = func() time.Time { return fixedNow.Add(time.Hour) }
	tickThrough(t, pool, admission, "tick-2")
	if marks := waitsSince(); marks["ACME-1"] == nil || !marks["ACME-1"].Equal(began) {
		t.Fatalf("a later pass left ACME-1's wait %v, want the moment it began, %s", marks["ACME-1"], began)
	}

	inTx(t, pool, func(tx pgx.Tx) {
		if err := record.NewStore().PutDispatchIssue(context.Background(), tx, record.DispatchReading{Key: "ACME-9",
			Project: acmeProject, Seq: 2, Status: record.Carry("done")}); err != nil {
			t.Fatalf("finish ACME-9 on the mirror: %v", err)
		}
	})
	tickThrough(t, pool, admission, "tick-3")
	if marks := waitsSince(); len(marks) != 2 || marks["ACME-1"] != nil {
		t.Fatalf("after its blocker finished the slots' waits read %v, want ACME-1 waiting on nothing", marks)
	}
}
