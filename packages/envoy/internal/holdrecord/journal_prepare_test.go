package holdrecord_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/sjawhar/envoy/internal/holdrecord"
	"github.com/sjawhar/envoy/natsgrant"
)

// prepareAbortRows are TestTheHoldJournal's rows of a hold whose row was lost in prepare and
// restored released (ReleasedPrepareAbort): hold abort is admitted on that row while the journal
// shows prepare, its cohort delete is admitted there, and its done entry ends the journal. A
// released row whose journal shows anything else, or no journal, deletes nothing.
func prepareAbortRows(t *testing.T) {
	ctx := context.Background()
	cohort := json.RawMessage(`{"raised":["a"]}`)

	// restoredReleased deletes the row and restores it released with holdID, at the gen the
	// route names, as the operator restores a row lost in prepare from a new process, which has
	// seen no row (RestoreRow's seen gen).
	restoredReleased := func(t *testing.T, r rig, holdID string) {
		t.Helper()
		if _, err := r.Pool.Exec(ctx, `delete from nats_hold`); err != nil {
			t.Fatal(err)
		}
		operator := r.client(t)
		var mismatch *holdrecord.RestoreGenError
		if err := operator.Bootstrap(ctx); !errors.As(err, &mismatch) {
			t.Fatalf("Bootstrap with the row lost answered %v, want a RestoreGenError", err)
		}
		if _, err := operator.RestoreRow(ctx, mismatch.RestoreGen, natsgrant.OperatorPhaseReleased, holdID, "restored after a delete in prepare"); err != nil {
			t.Fatal(err)
		}
	}
	cohorts := func(t *testing.T, r rig, holdID string) int {
		t.Helper()
		var n int
		if err := r.Pool.QueryRow(ctx, `select count(*) from nats_hold_records where hold_id = $1 and kind = 'cohort'`, holdID).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}

	t.Run("after a row lost in prepare is restored released, hold abort deletes the cohort and ends the journal", func(t *testing.T) {
		// Fails while the released row admits nothing: Admit refuses ErrNotHeld and the cohort
		// delete 409 hold_mismatch, so the cohort is orphaned and the journal never ends.
		r := newRig(t)
		c := r.client(t)
		const holdID = "h-prepare-abort"
		current := row(t, c)
		if _, err := c.MoveRow(ctx, current.Gen, natsgrant.OperatorPhasePrepare, holdID, "prepare"); err != nil {
			t.Fatal(err)
		}
		if err := c.StartJournal(ctx, holdID, holdrecord.WorkflowCutover, nil); err != nil {
			t.Fatal(err)
		}
		if _, err := c.Admit(ctx, holdID, cmd(holdrecord.CommandSet), nil); err != nil {
			t.Fatal(err)
		}
		if _, err := c.PutRecord(ctx, holdID, "cohort", "cohort", 0, cohort); err != nil {
			t.Fatal(err)
		}
		restoredReleased(t, r, holdID)
		if _, err := c.Admit(ctx, holdID, cmd(holdrecord.CommandAbort), nil); err != nil {
			t.Errorf("Admit(hold abort) on the released row answered %v, want admitted", err)
		}
		if err := c.DeleteCohort(ctx, holdID); err != nil {
			t.Errorf("DeleteCohort on the released row answered %v, want nil", err)
		}
		if err := c.AppendDone(ctx, holdID, holdrecord.CommandAbort, nil); err != nil {
			t.Fatalf("AppendDone(hold abort) answered %v", err)
		}
		if n := cohorts(t, r, holdID); n != 0 {
			t.Errorf("after the abort the table holds %d cohort records, want 0", n)
		}
		d, _ := journal(t, c, holdID)
		if done, ok := d.Latest(holdrecord.CommandAbort, holdrecord.StateDone); !ok || !d.Complete {
			t.Errorf("the journal's abort done entry is %+v, %v, and complete %v; want a done abort that ends it", done, ok, d.Complete)
		}
		if _, err := c.Admit(ctx, holdID, cmd(holdrecord.CommandAbort), nil); !errors.Is(err, holdrecord.ErrNotHeld) {
			t.Errorf("a second abort after the journal ended answered %v, want ErrNotHeld", err)
		}
	})

	t.Run("a released row whose journal shows a done hold set deletes no cohort", func(t *testing.T) {
		// A hold that ran and was released keeps its cohort record; its row is released with its
		// hold id. Fails while the route deletes on any released row of the hold.
		r := newRig(t)
		c := r.client(t)
		const holdID = "h-released-done"
		hold(t, c, holdID, holdrecord.WorkflowBucketsCreate, nil)
		if _, err := c.PutRecord(ctx, holdID, "cohort", "cohort", 0, cohort); err != nil {
			t.Fatal(err)
		}
		run(t, c, holdID, cmd("buckets create"), nil, nil)
		release(t, c, holdID)
		var refusal *holdrecord.RouteError
		if err := c.DeleteCohort(ctx, holdID); !errors.As(err, &refusal) || refusal.Code != "hold_mismatch" {
			t.Errorf("DeleteCohort on the released row of a hold with a done hold set answered %v, want 409 hold_mismatch", err)
		}
		if n := cohorts(t, r, holdID); n != 1 {
			t.Errorf("after the refused delete the table holds %d cohort records, want 1", n)
		}
	})

	t.Run("a released row with no journal for its hold: hold abort deletes the cohort", func(t *testing.T) {
		// The row was lost in prepare before the journal was written (ReleasedNoJournalAbort): the
		// abort has no journal to admit it, and deletes the cohort alone. Fails while the route
		// refuses a released row with no done record: the cohort stays for good.
		r := newRig(t)
		c := r.client(t)
		const holdID = "h-released-nojournal"
		current := row(t, c)
		if _, err := c.MoveRow(ctx, current.Gen, natsgrant.OperatorPhasePrepare, holdID, "prepare"); err != nil {
			t.Fatal(err)
		}
		if _, err := c.PutRecord(ctx, holdID, "cohort", "cohort", 0, cohort); err != nil {
			t.Fatal(err)
		}
		restoredReleased(t, r, holdID)
		if err := c.DeleteCohort(ctx, holdID); err != nil {
			t.Errorf("DeleteCohort on the released row of a hold with no journal answered %v, want nil", err)
		}
		if n := cohorts(t, r, holdID); n != 0 {
			t.Errorf("after the delete the table holds %d cohort records, want 0", n)
		}
	})

	t.Run("a released row whose journal a writer deleted, on the replica that wrote it, deletes no cohort", func(t *testing.T) {
		// The broker wrote the hold's done record, so its record floor saw it; a database writer
		// deletes it, and the hold reads as one with no journal. Fails while the cohort-delete route
		// reads a missing journal without the record floor: the no-journal abort deletes the cohort.
		r := newRig(t)
		c := r.client(t)
		const holdID = "h-journal-gone"
		current := row(t, c)
		if _, err := c.MoveRow(ctx, current.Gen, natsgrant.OperatorPhasePrepare, holdID, "prepare"); err != nil {
			t.Fatal(err)
		}
		if err := c.StartJournal(ctx, holdID, holdrecord.WorkflowCutover, nil); err != nil {
			t.Fatal(err)
		}
		if _, err := c.PutRecord(ctx, holdID, "cohort", "cohort", 0, cohort); err != nil {
			t.Fatal(err)
		}
		restoredReleased(t, r, holdID)
		if _, err := r.Pool.Exec(ctx, `delete from nats_hold_records where hold_id = $1 and kind = 'done'`, holdID); err != nil {
			t.Fatal(err)
		}
		var refusal *holdrecord.RouteError
		if err := c.DeleteCohort(ctx, holdID); !errors.Is(err, holdrecord.ErrUnknown) || !errors.As(err, &refusal) || refusal.Code != "below_floor" {
			t.Errorf("DeleteCohort with the hold's journal deleted outside the routes answered %v, want 409 below_floor, read as ErrUnknown", err)
		}
		if n := cohorts(t, r, holdID); n != 1 {
			t.Errorf("after the refused delete the table holds %d cohort records, want 1", n)
		}
	})

	t.Run("a released row whose done record does not verify deletes no cohort", func(t *testing.T) {
		// A database writer changed the journal's signature. Fails while the route reads a journal
		// that does not verify as no journal: it deletes the cohort.
		r := newRig(t)
		c := r.client(t)
		const holdID = "h-released-forged"
		current := row(t, c)
		if _, err := c.MoveRow(ctx, current.Gen, natsgrant.OperatorPhasePrepare, holdID, "prepare"); err != nil {
			t.Fatal(err)
		}
		if err := c.StartJournal(ctx, holdID, holdrecord.WorkflowCutover, nil); err != nil {
			t.Fatal(err)
		}
		if _, err := c.PutRecord(ctx, holdID, "cohort", "cohort", 0, cohort); err != nil {
			t.Fatal(err)
		}
		restoredReleased(t, r, holdID)
		if _, err := r.Pool.Exec(ctx, `update nats_hold_records set sig = 'forged' where hold_id = $1 and kind = 'done'`, holdID); err != nil {
			t.Fatal(err)
		}
		var refusal *holdrecord.RouteError
		if err := c.DeleteCohort(ctx, holdID); !errors.As(err, &refusal) || refusal.Code != "hold_mismatch" {
			t.Errorf("DeleteCohort on the released row of a hold whose done record does not verify answered %v, want 409 hold_mismatch", err)
		}
		if n := cohorts(t, r, holdID); n != 1 {
			t.Errorf("after the refused delete the table holds %d cohort records, want 1", n)
		}
	})

	t.Run("after a prepare entry for hold A, a released restore naming hold B, or no hold, is refused 409 and stores nothing", func(t *testing.T) {
		// The Legion reviewer's N5: a released row under another hold id leaves hold A's cohort
		// and journal where no command reaches them (Admit and the cohort delete need A's id on the
		// row, and A cannot be prepared again). Fails while the route stores released after a
		// prepare entry under any hold id: the row lands released at the restore gen. A's own id
		// still restores, and abort then ends A.
		r := newRig(t)
		c := r.client(t)
		const holdA = "h-prepare-a"
		current := row(t, c)
		if _, err := c.MoveRow(ctx, current.Gen, natsgrant.OperatorPhasePrepare, holdA, "prepare"); err != nil {
			t.Fatal(err)
		}
		if err := c.StartJournal(ctx, holdA, holdrecord.WorkflowCutover, nil); err != nil {
			t.Fatal(err)
		}
		if _, err := c.Admit(ctx, holdA, cmd(holdrecord.CommandSet), nil); err != nil {
			t.Fatal(err)
		}
		if _, err := c.PutRecord(ctx, holdA, "cohort", "cohort", 0, cohort); err != nil {
			t.Fatal(err)
		}
		if _, err := r.Pool.Exec(ctx, `delete from nats_hold`); err != nil {
			t.Fatal(err)
		}
		operator := r.client(t) // a new process, which has seen no row
		var mismatch *holdrecord.RestoreGenError
		if err := operator.Bootstrap(ctx); !errors.As(err, &mismatch) {
			t.Fatalf("Bootstrap with the row lost answered %v, want a RestoreGenError", err)
		}
		for _, other := range []string{"h-prepare-b", ""} {
			got, err := operator.RestoreRow(ctx, mismatch.RestoreGen, natsgrant.OperatorPhaseReleased, other, "released under another hold")
			var refusal *holdrecord.RouteError
			if !errors.As(err, &refusal) || refusal.Status != 409 || refusal.Code != "hold_mismatch" {
				t.Errorf("RestoreRow of released with hold id %q after hold A's prepare entry answered %+v, %v; want 409 hold_mismatch", other, got, err)
			} else if refusal.HoldID != holdA {
				// F6: the hold id to name is in the refusal's typed field, which the client exposes;
				// it fails while the route carries it only in the message text.
				t.Errorf("the 409 hold_mismatch carries hold id %q, want %q, the prepared hold's", refusal.HoldID, holdA)
			}
			var n int
			if err := r.Pool.QueryRow(ctx, `select count(*) from nats_hold`).Scan(&n); err != nil || n != 0 {
				t.Fatalf("after the restore with hold id %q the table holds %d rows (%v), want none", other, n, err)
			}
		}
		if _, err := operator.RestoreRow(ctx, mismatch.RestoreGen, natsgrant.OperatorPhaseReleased, holdA, "released under hold A"); err != nil {
			t.Fatalf("RestoreRow of released with hold A's id answered %v, want it stored", err)
		}
		if _, err := c.Admit(ctx, holdA, cmd(holdrecord.CommandAbort), nil); err != nil {
			t.Fatalf("Admit(hold abort) on hold A's released row answered %v", err)
		}
		if err := c.DeleteCohort(ctx, holdA); err != nil {
			t.Fatalf("DeleteCohort on hold A's released row answered %v", err)
		}
		if err := c.AppendDone(ctx, holdA, holdrecord.CommandAbort, nil); err != nil {
			t.Fatalf("AppendDone(hold abort) answered %v", err)
		}
		if n := cohorts(t, r, holdA); n != 0 {
			t.Errorf("after hold A's abort the table holds %d cohort records, want 0", n)
		}
	})
}
