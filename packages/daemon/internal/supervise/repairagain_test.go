package supervise

import (
	"log/slog"
	"slices"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/runtime"
)

// A repair that fails again costs the decision nothing. It keeps the mark, says so once, and the
// decision goes on to its own row with the event behind it applied: the repair returns nothing,
// precisely so that its failure cannot reach supervisor.decisionFailed in internal/daemon, which
// logs an error and drops the event. An event dropped for a write the database did not take is
// the fault this change exists to end, so a database still away must cost no more than the write
// it is still refusing.
func TestWhenTheRepairFailsAgainTheDecisionStillGoesOn(t *testing.T) {
	h, counted := owedTurnStart(t)
	written := len(h.records.of(owedWriteLine))
	attempted := counted.mark()

	// The store is still refusing claim writes. A tool call's own write is the step's narrow one
	// (PutClaimStep), which the outage is not on, so this decision has work it can finish.
	h.stepped("bash", "toolu_turn1_0", toolArgs, false)

	if h.m.owedWrite == nil {
		t.Fatal("the repair that failed again dropped the mark; the write would be lost with it")
	}
	lines := h.records.of(owedWriteLine)[written:]
	if len(lines) != 1 {
		t.Fatalf("the failed repair wrote %d lines, want the one an operator counts: %v", len(lines), lines)
	}
	if lines[0].level != slog.LevelWarn {
		t.Fatalf("the owed line is %s, want WARN", lines[0].level)
	}
	for _, key := range []string{"store", "error"} {
		if _, ok := lines[0].attr(key); !ok {
			t.Fatalf("the owed line carries no %s; a reader follows the write by it", key)
		}
	}
	if asked := counted.writes()[attempted:]; !slices.Equal(asked, []string{"PutClaim"}) {
		t.Fatalf("the decision asked for %v, want the repair's refused claim write alone: the step's own write is no claim write", asked)
	}
	if step := h.store.step(testToken); step.CallID != "toolu_turn1_0" {
		t.Fatalf("the stored step is %+v, want the call the step carried: the event was applied, not dropped", step)
	}
}

// A turn start that confirms a task writes the claim and the delivery in one transaction, so a
// failure leaves both unwritten and both owed. The repair makes that one write again — not the
// claim alone, which would leave a working claim whose delivery is unconfirmed, and not two
// writes, which would leave the row in between — and it makes the report the confirming decision
// owed. Before this change that half of the turn start had its delivery written again by settle
// and never made its report, so the daemon was never told the woken role took its work.
func TestAConfirmOwedWriteIsMadeWholeWithItsReport(t *testing.T) {
	h, counted := newCountedHarness(t)
	reports := watchReports(h)
	h.reach(StateReady)
	h.must(RequestDeliver{Claim: testToken, Task: "the task"})
	delivery := h.pending().ID
	h.store.fail("PutClaim", brokenConnection())

	if err := h.handle(StreamTurnStart{Claim: testToken}); err == nil {
		t.Fatal("the confirmation whose write failed returned no error")
	}

	h.wantState(StateWorking)
	if h.m.owedWrite == nil {
		t.Fatal("the confirmation's failed write left no mark")
	}
	if owed := h.records.of(owedWriteLine); len(owed) != 1 {
		t.Fatalf("the owed write is logged %d times, want once", len(owed))
	} else if store, ok := owed[0].attr("store"); !ok || store.String() != "PutClaimAndDelivery" {
		t.Fatalf("the owed line names the store method %v, want the transaction that writes both rows", store)
	}
	if seen := states(reports()); !slices.Equal(seen, []ClaimState{StateReady}) {
		t.Fatalf("the daemon was told %v; the confirmation's working is owed until its write lands", seen)
	}

	h.store.fail("PutClaim", nil)
	attempted := counted.mark()
	h.observe(runtime.Alive)

	if asked := counted.writes()[attempted:]; !slices.Equal(asked, []string{"PutClaimAndDelivery"}) {
		t.Fatalf("the repairing decision asked for %v, want the one transaction that writes both rows once", asked)
	}
	stored := h.store.load(testToken)
	if stored.State != StateWorking {
		t.Fatalf("the repaired row reads %s, want working", stored.State)
	}
	if stored.Pending == nil || stored.Pending.ID != delivery || stored.Pending.ConfirmedAt.IsZero() {
		t.Fatalf("the stored delivery is %+v, want %s written confirmed beside the working claim", stored.Pending, delivery)
	}
	if seen := states(reports()); !slices.Equal(seen, []ClaimState{StateReady, StateWorking}) {
		t.Fatalf("the daemon was told %v, want the confirmation's owed working made when its write landed", seen)
	}
}
