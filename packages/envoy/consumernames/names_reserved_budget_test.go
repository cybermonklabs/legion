package consumernames

import (
	"context"
	"slices"
	"testing"
	"time"
)

// A create's bound runs from its reservation, not from when the create is sent, so the create of a
// candidate ends no later than the candidate's until, whoever retires it. Here the first process's
// Config.Reserved hook runs while two other processes act on the key: the second's Cleanup settles
// the open create once it is the settle age old (Step 0 abandons the candidate, until = then + its
// create bound), and the third's Cleanup, past that until and its own settle age, reads the
// candidate absent and drops it. The hook then returns. The first process sends no create of the
// candidate, whose bound has passed since its reservation, and goes on to a fresh one; the server
// never holds a consumer no record names. Before, the create was bounded from its own start: the
// candidate landed after its until, and no record named it, an orphan holding a slot of the stream.
func TestACreatesBoundRunsFromItsReservation(t *testing.T) {
	const key = "ses-1"
	ctx := context.Background()
	h := newHarness()
	var candidate string
	p1 := h.names("p1")
	p1.cfg.Reserved = func(_ context.Context, _ string, op Op) error {
		if candidate != "" {
			return nil
		}
		candidate = op.Candidate
		h.clock.advance(3 * time.Second)
		if _, err := h.names("p2").Cleanup(ctx, key); err != nil {
			t.Errorf("the second process's Cleanup over the open create = %v", err)
		}
		h.clock.advance(testCreateBound + testSettleAge + time.Second)
		if _, err := h.names("p3").Cleanup(ctx, key); err != nil {
			t.Errorf("the third process's Cleanup past the candidate's until = %v", err)
		}
		if rec := h.store.record(t, key); slices.Contains(rec.Retired, candidate) {
			t.Errorf("setup: after the third Cleanup the record still retires %s: %+v", candidate, rec)
		}
		return nil
	}
	sel, _, err := p1.Ensure(ctx, key, CreateRequest{Create: h.server.create(key), First: first(1)})
	if err != nil {
		t.Fatalf("the first process's Ensure = %v", err)
	}
	if candidate == "" || sel.Record.Current == candidate {
		t.Fatalf("setup: the Ensure selected %s, the hook saw candidate %q; want a fresh current", sel.Record.Current, candidate)
	}
	rec := h.store.record(t, key)
	if h.server.holds(candidate) && rec.Current != candidate && !slices.Contains(rec.Retired, candidate) {
		t.Fatalf("the server holds %s, which its create made after the candidate's until, and the record %+v names it nowhere: an orphan", candidate, rec)
	}
	// Control: a reservation whose hook returns at once creates its candidate within the bound.
	h2 := newHarness()
	quick := h2.names("p1")
	if sel, _, err := quick.Ensure(ctx, key, CreateRequest{Create: h2.server.create(key), First: first(1)}); err != nil || !h2.server.holds(sel.Record.Current) {
		t.Fatalf("control: an Ensure with no hook = %+v, %v; want its current created", sel.Record, err)
	}
}
