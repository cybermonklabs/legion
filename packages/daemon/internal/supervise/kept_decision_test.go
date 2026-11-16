package supervise

import (
	"bytes"
	"slices"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/phase"
)

// keptDecision is what a start row names of a decision a person made while nobody in the root's
// tree could hear it, as the row recorded it (record.KeptDecisions, encoded JSON). The machine
// carries it as the bytes the row wrote (Delivery.Kept).
const keptDecision = `{"decisions":[{"ref":"dispatch://LEGION-208/ask/ask-1","at":"2026-11-01T09:00:00Z",` +
	`"issue":"LEGION-208","decision":"ask-answered","actor":"user:alice"}]}`

// A person's recorded decision is claimed by the start row that names it and told by the
// confirmation of the delivery that carries it (the claim store's putDelivery, the outbox's
// settleKept), so the claim must be given that row's task exactly once: a second queueing would
// tell the decision a second time, and nothing after the telling re-places the row.
//
// What holds it to once is the record of the rows this claim has already queued a task from
// (Claim.DeliveredRows, written in the transaction that writes the delivery), which is the
// duplicate guard of a delivery that carries no entry — a catch-up and a root architect's start,
// which hand no phase entry. A row runs again whenever a failure follows its delivery
// (record.Store.RetryOutbox leaves it unleased), and the claim declines it by that record alone.
func TestAKeptDecisionOfAStartRowReachesItsClaimOnce(t *testing.T) {
	h := newHarness(t)
	h.reach(StateReady)
	start := RequestDeliver{Claim: testToken, Task: "a person decided: dispatch://LEGION-208/ask/ask-1",
		ID: "outbox:40", Row: 40, Phase: phase.Implementing, Generation: 7, Kept: []byte(keptDecision)}

	h.must(start)

	if p := h.pending(); !bytes.Equal(p.Kept, []byte(keptDecision)) {
		t.Fatalf("the queued delivery names %q of the tree's kept decisions, want the %q the row recorded: "+
			"its confirmation is what tells them", p.Kept, keptDecision)
	}
	if rows := h.claim().DeliveredRows; !slices.Equal(rows, []int64{40}) {
		t.Fatalf("the claim records rows %v as queued, want the one row 40 it was given a task from", rows)
	}
	queued := h.store.deliveryLog()
	prompts := len(h.prompts())

	// The row runs again, as it does after any failure that follows its delivery. The claim was
	// given it, so nothing is queued and nothing is written: the decision is named once.
	h.must(start)

	if log := h.store.deliveryLog(); !slices.Equal(log, queued) {
		t.Fatalf("the row run again wrote %v, want the %v the first run left: a second delivery of a row the claim queued "+
			"tells the decisions it names a second time", log, queued)
	}
	h.wantPrompts(prompts)

	// The turn takes the task. The write that records the confirmation carries what the row
	// named, which is the write that tells it.
	h.must(StreamTurnStart{Claim: testToken})

	stored, ok := h.store.delivery(testToken)
	if !ok || stored.ConfirmedAt.IsZero() || !bytes.Equal(stored.Kept, []byte(keptDecision)) {
		t.Fatalf("the stored delivery after the turn started = %+v (held %t), want it confirmed and still naming %q",
			stored, ok, keptDecision)
	}

	h.must(StreamTurnEnd{Claim: testToken})

	if d, ok := h.store.delivery(testToken); ok {
		t.Fatalf("the delivery %+v stands after the turn ended, want it retired", d)
	}
	if log := h.store.deliveryLog(); len(log) == 0 || log[len(log)-1] != "retire outbox:40" {
		t.Fatalf("the delivery writes = %v, want the retirement of outbox:40 last", log)
	}

	// The row runs once more after the telling. The claim still records it, so the decision the
	// architect has been told is not told again.
	h.must(start)

	if p := h.claim().Pending; p != nil {
		t.Fatalf("the row run after the telling queued %+v, want nothing: the claim was given row 40 already", p)
	}
	if _, ok := h.store.delivery(testToken); ok {
		t.Error("the row run after the telling wrote a delivery over the retired one")
	}
}
