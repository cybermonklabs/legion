package consumernames

import (
	"context"
	"slices"
	"testing"
	"time"
)

// An abandoned candidate's until is written on the clock of the process that retired it, and read
// on the clock of whichever process drops it, another listener's sweep among them. A dropper allows
// the settle age (Config.SettleAge, 2 s) beside it for the skew between the two clocks: a name read
// absent is dropped only once its until and the settle age have passed. The deep review's probe:
// the reserver on the true time, its create bound 10 s; a retirer whose clock is 3 s ahead
// retires the candidate 0.1 s after the reservation, with until at true +13.1 s; at true +7.2 s
// the reserver's create can still be sent, and a dropper whose clock is 6 s ahead reads +13.2 s.
// It keeps the candidate, so when the create lands the next Cleanup deletes the consumer it made.
// Before, it dropped the candidate, and that consumer was named by no record: an orphan holding a
// slot of the stream. The controls are a dropper 3 s ahead, which keeps the candidate at both
// heads, and a dropper past the until and the settle age, which drops a candidate whose create
// never landed.
func TestAnAbandonedCandidateIsKeptForTheSettleAgePastItsUntil(t *testing.T) {
	const key = "ses-1"
	ctx := context.Background()
	setup := func(t *testing.T) (*harness, string, time.Time, func(owner string, ahead time.Duration) *Names) {
		t.Helper()
		h := newHarness()
		at := func(owner string, ahead time.Duration) *Names {
			n := h.names(owner)
			n.cfg.Now = func() time.Time { return h.clock.Now().Add(ahead) }
			return n
		}
		candidate, reservedAt := FreshName("session"), h.clock.Now()
		h.store.put(key, Record{Key: key, Start: 1, Op: &Op{Kind: OpCreate, Owner: "reserver", Candidate: candidate, CreateWithin: testCreateBound, ReservedAt: reservedAt}})
		h.clock.advance(100 * time.Millisecond)
		if _, err := at("retirer", 3*time.Second).Resolve(ctx, key); err != nil {
			t.Fatalf("the retirer's Resolve = %v", err)
		}
		if rec := h.store.record(t, key); rec.Op != nil || !slices.Contains(rec.Retired, candidate) || len(rec.Abandoned) != 1 {
			t.Fatalf("setup: the record after the retirer's Resolve = %+v; want %s retired and abandoned", rec, candidate)
		}
		return h, candidate, reservedAt, at
	}
	t.Run("a dropper 6 s ahead, while the create can still be sent", func(t *testing.T) {
		h, candidate, reservedAt, at := setup(t)
		h.clock.advance(7100 * time.Millisecond)
		if h.clock.Now().Sub(reservedAt) >= testCreateBound {
			t.Fatalf("setup: the reserver's create bound has passed at %s", h.clock.Now().Sub(reservedAt))
		}
		for _, ahead := range []time.Duration{3 * time.Second, 6 * time.Second} {
			if _, err := at("dropper", ahead).Cleanup(ctx, key); err != nil {
				t.Fatalf("the Cleanup of a dropper %s ahead = %v", ahead, err)
			}
			if rec := h.store.record(t, key); !slices.Contains(rec.Retired, candidate) {
				t.Fatalf("a dropper %s ahead dropped %s, whose create can still land: the record is %+v", ahead, candidate, rec)
			}
		}
		if _, err := h.server.create(key)(ctx, candidate, Start{Seq: 1}); err != nil {
			t.Fatal(err)
		}
		if _, err := at("dropper", 6*time.Second).Cleanup(ctx, key); err != nil {
			t.Fatalf("the Cleanup once the create landed = %v", err)
		}
		if rec := h.store.record(t, key); h.server.holds(candidate) || slices.Contains(rec.Retired, candidate) {
			t.Fatalf("after the create landed and a Cleanup, the server holds %s: %t, the record %+v; want it deleted and dropped", candidate, h.server.holds(candidate), rec)
		}
	})
	t.Run("control: a dropper past the until and the settle age", func(t *testing.T) {
		h, candidate, _, at := setup(t)
		h.clock.advance(15200 * time.Millisecond)
		if _, err := at("dropper", 0).Cleanup(ctx, key); err != nil {
			t.Fatal(err)
		}
		if rec := h.store.record(t, key); slices.Contains(rec.Retired, candidate) || len(rec.Abandoned) != 0 {
			t.Fatalf("the record after a Cleanup past the until and the settle age = %+v; want %s dropped", rec, candidate)
		}
	})
}
