package consumernames

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"
)

// A call that creates nothing (Resolve, Delete, Cleanup) settles a convert's open replace of the
// key's fixed name without leaving that name on the server with no operation naming it: with a
// saved start it deletes the fixed name and only then closes the replace, and a delete that fails
// is the call's error with the replace kept open, naming it; with no saved start it leaves the
// replace open for a call that creates. The record is the one t-7's convert leaves after its own
// delete of the fixed name failed: a replace s-ses-1 → session~C reserved an hour ago, whose
// candidate the server does not hold. The control is 6c607400b0's abandon, which closed the
// replace and left s-ses-1 held with no operation naming it.
func TestACallThatCreatesNothingDeletesAConvertsFixedNameBeforeItClosesTheReplace(t *testing.T) {
	const key, fixed, candidate = "ses-1", "s-ses-1", "session~C"
	calls := map[string]func(context.Context, *Names) error{
		"Resolve": func(ctx context.Context, n *Names) error { _, err := n.Resolve(ctx, key); return err },
		"Delete":  func(ctx context.Context, n *Names) error { _, err := n.Delete(ctx, key, nil); return err },
		"Cleanup": func(ctx context.Context, n *Names) error { _, err := n.Cleanup(ctx, key); return err },
	}
	for _, name := range []string{"Resolve", "Delete", "Cleanup"} {
		call := calls[name]
		for _, c := range []struct {
			name        string
			start       uint64
			failDelete  bool
			closed      bool
			fixedIsHeld bool
		}{
			{name: "with a saved start", start: 3, closed: true},
			{name: "with a saved start and a delete that fails", start: 3, failDelete: true, fixedIsHeld: true},
			{name: "with no saved start", start: 0, fixedIsHeld: true},
		} {
			t.Run(name+" "+c.name, func(t *testing.T) {
				ctx := context.Background()
				h := newHarness()
				if _, err := h.server.create(key)(ctx, fixed, Start{Seq: 3}); err != nil {
					t.Fatal(err)
				}
				h.store.put(key, Record{Key: key, Start: c.start, Op: &Op{Kind: OpReplace, Old: fixed, Candidate: candidate, Owner: "converter", ReservedAt: h.clock.Now().Add(-time.Hour)}})
				if c.failDelete {
					h.server.deleteFault = func(name string) error {
						if name == fixed {
							return errors.New("the server refused the delete")
						}
						return nil
					}
				}
				names := h.names("p1")
				names.cfg.Fixed = func(logical string) string { return "s-" + logical }
				err := call(ctx, names)
				record := h.store.record(t, key)
				switch {
				case c.failDelete && err == nil:
					t.Fatalf("%s over a failed delete of %s = nil; want the delete's error", name, fixed)
				case !c.failDelete && err != nil:
					t.Fatalf("%s = %v; want it served", name, err)
				case h.server.holds(fixed) != c.fixedIsHeld:
					t.Fatalf("after %s the server holds %s: %t; want %t", name, fixed, h.server.holds(fixed), c.fixedIsHeld)
				case c.closed && record.Op != nil:
					t.Fatalf("after %s the record is %+v; want the replace closed", name, record)
				case !c.closed && (record.Op == nil || record.Op.Old != fixed):
					t.Fatalf("after %s the record is %+v; want the replace open, naming %s", name, record, fixed)
				case slices.Contains(record.Retired, fixed):
					t.Fatalf("after %s the record retires the fixed name: %+v", name, record)
				}
			})
		}
	}
}
