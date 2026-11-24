package consumernames

import (
	"context"
	"errors"
	"testing"
	"time"
)

// A replace reserved with no start (Replace at start 0, Dispatch's take-over) has destroyed
// nothing, so a call that settles it without creating closes it with the record as it was before
// the reservation: the saved start stands (plan §2's Step 0 row "replace with no start yet").
// Here the record proved start 7; Replace(sel, 0) reserves and its process stops before it reads
// the predecessor's floor (Config.Reserved fails). A Cleanup then settles the open replace: the
// operation closes, the candidate is retired, and the start is still 7, so once the current is gone
// the next Ensure creates at 7. The control is a replace that a call which creates settles: its
// start is read from the predecessor's floor, as before. Before, the reservation wrote start 0 and
// the Cleanup closed the replace at 0, and the next Ensure answered ErrNoSafeStart though the record
// had proved 7.
func TestAReplaceReservedWithNoStartKeepsTheSavedStart(t *testing.T) {
	const key = "ses-1"
	ctx := context.Background()
	reserveAndStop := func(t *testing.T, h *harness) {
		t.Helper()
		if _, err := h.server.create(key)(ctx, "session~A", Start{Seq: 7}); err != nil {
			t.Fatal(err)
		}
		h.server.setFloor("session~A", 9)
		h.store.put(key, Record{Key: key, Current: "session~A", Start: 7})
		stopping := h.names("p1")
		stopped := errors.New("the process stopped after the reservation")
		stopping.cfg.Reserved = func(context.Context, string, Op) error { return stopped }
		sel, err := stopping.Resolve(ctx, key)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := stopping.Replace(ctx, sel, 0, h.server.create(key)); !errors.Is(err, stopped) {
			t.Fatalf("Replace at start 0 with its process stopping after the reservation = %v", err)
		}
		if rec := h.store.record(t, key); rec.Op == nil || rec.Op.Kind != OpReplace {
			t.Fatalf("the record after the stopped Replace = %+v, want the replace open", rec)
		}
	}
	t.Run("settled by a call that creates nothing", func(t *testing.T) {
		h := newHarness()
		reserveAndStop(t, h)
		if _, err := h.names("p2").Cleanup(ctx, key); err != nil {
			t.Fatalf("Cleanup over the open replace = %v", err)
		}
		rec := h.store.record(t, key)
		if rec.Op != nil || rec.Current != "session~A" || rec.Start != 7 {
			t.Fatalf("the record after the Cleanup = %+v; want the replace closed, session~A current, start 7", rec)
		}
		if err := h.server.Delete(ctx, "session~A"); err != nil {
			t.Fatal(err)
		}
		sel, _, err := h.names("p3").Ensure(ctx, key, CreateRequest{Create: h.server.create(key), First: first(1)})
		if err != nil {
			t.Fatalf("Ensure once the current is gone = %v; want a create at the saved start 7", err)
		}
		if got := h.server.startOf(sel.Record.Current); got != (Start{Seq: 7}) {
			t.Fatalf("the consumer Ensure created starts at %+v, want 7", got)
		}
	})
	t.Run("control: settled by a call that creates", func(t *testing.T) {
		h := newHarness()
		reserveAndStop(t, h)
		h.clock.advance(time.Minute)
		sel, _, err := h.names("p2").Ensure(ctx, key, CreateRequest{Create: h.server.create(key), First: first(1)})
		if err != nil {
			t.Fatalf("Ensure over the open replace = %v", err)
		}
		if sel.Record.Current == "session~A" || h.server.startOf(sel.Record.Current) != (Start{Seq: 10}) {
			t.Fatalf("Ensure over the open replace selected %s at %+v; want a fresh candidate at the predecessor's floor + 1, 10",
				sel.Record.Current, h.server.startOf(sel.Record.Current))
		}
	})
}
