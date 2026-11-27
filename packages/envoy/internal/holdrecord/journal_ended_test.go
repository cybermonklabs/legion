package holdrecord_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/holdrecord"
	"github.com/sjawhar/envoy/natsgrant"
)

// savedJournal is one version of a hold's done record as the table held it, for a database writer
// to put back (putJournalBack).
type savedJournal struct {
	value     string
	gen       int64
	writtenAt time.Time
	sig       string
}

func saveJournal(t *testing.T, r rig, holdID string) savedJournal {
	t.Helper()
	var j savedJournal
	if err := r.Pool.QueryRow(context.Background(), `select value::text, gen, written_at, sig from nats_hold_records where hold_id = $1 and kind = 'done'`, holdID).
		Scan(&j.value, &j.gen, &j.writtenAt, &j.sig); err != nil {
		t.Fatal(err)
	}
	return j
}

func putJournalBack(t *testing.T, r rig, holdID string, j savedJournal) {
	t.Helper()
	if _, err := r.Pool.Exec(context.Background(), `update nats_hold_records set value = $2::jsonb, gen = $3, written_at = $4, sig = $5 where hold_id = $1 and kind = 'done'`,
		holdID, j.value, j.gen, j.writtenAt, j.sig); err != nil {
		t.Fatal(err)
	}
}

// endedHoldRows are TestTheHoldJournal's rows of a released restore beside the journals of holds
// the verified log shows ended. Every hold's journal, an ended one's included, is judged first
// against this replica's record floor, its signature and the log (journalBindingRows): an older
// version, or one that does not verify, is unknown (409 below_floor). Then a hold's journal holds
// no restore once it shows a done hold release or hold abort, or once the log shows a released
// entry naming it, at any gen, while the journal shows hold release's or hold abort's intent as
// that command's latest entry and no other intent pending (strictEndRows); a later hold's prepare
// shows nothing (forkRows). A hold_mismatch names only a hold a verified row or log entry names.
func endedHoldRows(t *testing.T) {
	ctx := context.Background()
	stored := func(t *testing.T, r rig) (int64, string) {
		t.Helper()
		var gen int64
		var phase string
		if err := r.Pool.QueryRow(ctx, `select gen, phase from nats_hold`).Scan(&gen, &phase); err != nil && !errors.Is(err, pgx.ErrNoRows) {
			t.Fatal(err)
		}
		return gen, phase
	}
	restoreGen := func(t *testing.T, c *holdrecord.Client) uint64 {
		t.Helper()
		var mismatch *holdrecord.RestoreGenError
		if err := c.Bootstrap(ctx); !errors.As(err, &mismatch) {
			t.Fatalf("Bootstrap with the row lost answered %v, want a RestoreGenError", err)
		}
		return mismatch.RestoreGen
	}
	// cutRelease runs a hold to its release's row move, without the release's done entry.
	cutRelease := func(t *testing.T, c *holdrecord.Client, holdID string) holdrecord.Row {
		t.Helper()
		held := hold(t, c, holdID, holdrecord.WorkflowCutover, nil)
		if _, err := c.Admit(ctx, holdID, cmd(holdrecord.CommandRelease), nil); err != nil {
			t.Fatal(err)
		}
		released, err := c.MoveRow(ctx, held.Gen, natsgrant.OperatorPhaseReleased, holdID, "release")
		if err != nil {
			t.Fatal(err)
		}
		return released
	}
	restoresReleased := func(t *testing.T, r rig, what string) {
		t.Helper()
		fresh := r.client(t)
		gen := restoreGen(t, fresh)
		if got, err := fresh.RestoreRow(ctx, gen, natsgrant.OperatorPhaseReleased, "", "the lost row"); err != nil || got.Gen != gen {
			t.Errorf("%s: RestoreRow of released at gen %d answered %+v, %v; want it stored", what, gen, got, err)
		}
	}

	t.Run("a release cut before its done entry, then a later hold, then a lost row: the released restore stores", func(t *testing.T) {
		// Claude's round-7 reproducer: the cut hold's journal shows hold set done and no end, but
		// the log shows its released entry. Fails while only the log's latest entry exempts a
		// hold: every released restore is refused.
		r := newRig(t)
		c := r.client(t)
		cutRelease(t, c, "h-cut-release")
		hold(t, c, "h-next", holdrecord.WorkflowBucketsCreate, nil)
		run(t, c, "h-next", cmd("buckets create"), nil, nil)
		release(t, c, "h-next")
		if _, err := r.Pool.Exec(ctx, `delete from nats_hold`); err != nil {
			t.Fatal(err)
		}
		r.Restart()
		restoresReleased(t, r, "after the cut release and the later hold")
	})

	t.Run("a release cut before its done entry, its released entry deleted, then a later hold: refused, naming the hand repair", func(t *testing.T) {
		// A later hold's prepare says nothing about the cut hold (N-E: gen order and signing order
		// disagree across histories), so with its released entry deleted, which takes a database
		// writer, the cut hold's journal holds every released restore, and no held restore of it
		// is admitted either: the refusal names the hold and the hand repair. Fails while the
		// later prepare ends the cut hold (released is stored), or while the refusal advises the
		// held restore the route refuses.
		r := newRig(t)
		c := r.client(t)
		released := cutRelease(t, c, "h-cut-unlogged")
		hold(t, c, "h-later", holdrecord.WorkflowBucketsCreate, nil)
		run(t, c, "h-later", cmd("buckets create"), nil, nil)
		release(t, c, "h-later")
		if _, err := r.Pool.Exec(ctx, `delete from nats_hold_log where gen = $1`, int64(released.Gen)); err != nil {
			t.Fatal(err)
		}
		if _, err := r.Pool.Exec(ctx, `delete from nats_hold`); err != nil {
			t.Fatal(err)
		}
		r.Restart()
		fresh := r.client(t)
		got := restoreRefusal(t, fresh, restoreGen(t, fresh), natsgrant.OperatorPhaseReleased, "")
		if !refusedWith(got, http.StatusBadRequest, "bad_transition", "hold h-cut-unlogged's journal", handRepair) || strings.Contains(got.Message, "restore the row held") {
			t.Errorf("the released restore answered %s; want 400 bad_transition naming h-cut-unlogged's journal and the hand repair", refusalText(got))
		}
		if gen, phase := stored(t, r); gen != 0 {
			t.Errorf("after the refused restore the table holds %s at gen %d, want no row", phase, gen)
		}
	})

	t.Run("an earlier hold's journal from mid-hold put back after a restart: refused, naming the older journal", func(t *testing.T) {
		// TN's S8: hold A's journal from while it was held, put back after A and B ended. After a
		// restart the route cannot tell it from an idle live hold whose released entry another
		// history put back (TN deep's R10-1), so a released entry ends a hold only while its
		// journal shows that end admitted, and this is a writer's residual infra/README.md lists.
		// Fails while a released entry ends any hold with no intent pending, as at 886868daac:
		// released is stored.
		r := newRig(t)
		c := r.client(t)
		hold(t, c, "h-a", holdrecord.WorkflowStreamsUpdate, nil)
		mid := saveJournal(t, r, "h-a")
		release(t, c, "h-a")
		hold(t, c, "h-b", holdrecord.WorkflowStreamsUpdate, nil)
		release(t, c, "h-b")
		putJournalBack(t, r, "h-a", mid)
		if _, err := r.Pool.Exec(ctx, `delete from nats_hold`); err != nil {
			t.Fatal(err)
		}
		r.Restart()
		fresh := r.client(t)
		got := restoreRefusal(t, fresh, restoreGen(t, fresh), natsgrant.OperatorPhaseReleased, "")
		if !refusedWith(got, http.StatusBadRequest, "bad_transition", "hold h-a's journal", "neither hold release's nor hold abort's intent",
			"an older version of this hold's journal put back", handRepair) {
			t.Errorf("the released restore answered %s; want 400 bad_transition naming h-a's journal, that it shows no end admitted, the older journal and the hand repair", refusalText(got))
		}
		if gen, phase := stored(t, r); gen != 0 {
			t.Errorf("the table holds %s at gen %d, want no row", phase, gen)
		}
	})

	t.Run("an older version of an ended hold's journal on the replica that saw the newer one is unknown: 409 below_floor", func(t *testing.T) {
		// Claude's swap, with no restart: the record floor saw A's newer journal. Every journal is
		// judged against the floor and its signature before any log entry ends its hold, so the
		// older version is refused, a database writer's residual infra/README.md lists. Fails
		// while the log's released entry skips A's journal first, as at e481b26500: released is
		// stored.
		r := newRig(t)
		c := r.client(t)
		hold(t, c, "h-swap-a", holdrecord.WorkflowCutover, nil)
		mid := saveJournal(t, r, "h-swap-a")
		release(t, c, "h-swap-a")
		hold(t, c, "h-swap-b", holdrecord.WorkflowCutover, nil)
		release(t, c, "h-swap-b")
		putJournalBack(t, r, "h-swap-a", mid)
		if _, err := r.Pool.Exec(ctx, `delete from nats_hold`); err != nil {
			t.Fatal(err)
		}
		fresh := r.client(t)
		got := restoreRefusal(t, fresh, restoreGen(t, fresh), natsgrant.OperatorPhaseReleased, "")
		if !refusedWith(got, http.StatusConflict, "below_floor", `hold "h-swap-a"'s done record`, handRepair) {
			t.Errorf("the released restore answered %s; want 409 below_floor naming h-swap-a's done record and the hand repair", refusalText(got))
		}
		if gen, phase := stored(t, r); gen != 0 {
			t.Errorf("after the refused restore the table holds %s at gen %d, want no row", phase, gen)
		}
	})

	t.Run("an older version of a held hold's journal is unknown: 409 below_floor", func(t *testing.T) {
		// The log shows the hold held, not ended, so its journal is judged: against the record
		// floor, which saw a newer version. Fails while an older version stands as the hold's
		// record: 400 bad_transition.
		r := newRig(t)
		c := r.client(t)
		hold(t, c, "h-older", holdrecord.WorkflowCutover, nil)
		older := saveJournal(t, r, "h-older")
		if _, err := c.Admit(ctx, "h-older", cmd("create-bucket"), nil); err != nil {
			t.Fatal(err)
		}
		putJournalBack(t, r, "h-older", older)
		if _, err := r.Pool.Exec(ctx, `delete from nats_hold`); err != nil {
			t.Fatal(err)
		}
		fresh := r.client(t)
		got, err := fresh.RestoreRow(ctx, restoreGen(t, fresh), natsgrant.OperatorPhaseReleased, "", "released over an older journal")
		var refusal *holdrecord.RouteError
		if !errors.Is(err, holdrecord.ErrUnknown) || !errors.As(err, &refusal) || refusal.Code != "below_floor" {
			t.Errorf("RestoreRow of released answered %+v, %v; want 409 below_floor, read as ErrUnknown", got, err)
		}
		if gen, phase := stored(t, r); gen != 0 {
			t.Errorf("after the refused restore the table holds %s at gen %d, want no row", phase, gen)
		}
	})

	t.Run("a journal of a hold the log never names that does not verify is unknown: 409 below_floor", func(t *testing.T) {
		// A writer adds a done record under a hold id of its own, unsigned. Fails while a journal
		// other than the named hold's is skipped when it does not verify: released is stored.
		r := newRig(t)
		c := r.client(t)
		hold(t, c, "h-real", holdrecord.WorkflowBucketsCreate, nil)
		run(t, c, "h-real", cmd("buckets create"), nil, nil)
		release(t, c, "h-real")
		if _, err := r.Pool.Exec(ctx, `insert into nats_hold_records (hold_id, kind, key, value, gen, written_at, sig)
			select 'h-ghost', kind, key, value, gen, written_at, 'forged' from nats_hold_records where hold_id = 'h-real' and kind = 'done'`); err != nil {
			t.Fatal(err)
		}
		if _, err := r.Pool.Exec(ctx, `delete from nats_hold`); err != nil {
			t.Fatal(err)
		}
		fresh := r.client(t)
		got, err := fresh.RestoreRow(ctx, restoreGen(t, fresh), natsgrant.OperatorPhaseReleased, "", "released beside a forged journal")
		var refusal *holdrecord.RouteError
		if !errors.Is(err, holdrecord.ErrUnknown) || !errors.As(err, &refusal) || refusal.Code != "below_floor" {
			t.Errorf("RestoreRow of released answered %+v, %v; want 409 below_floor, read as ErrUnknown", got, err)
		}
		if gen, phase := stored(t, r); gen != 0 {
			t.Errorf("after the refused restore the table holds %s at gen %d, want no row", phase, gen)
		}
	})

	t.Run("a completed hold's journal put back from prepare does not admit the cohort delete: 409 below_floor", func(t *testing.T) {
		// P3-3: the older version shows prepare (hold set's intent alone), which the released row's
		// cohort delete admits. Fails while the route reads that version as it is: the completed
		// hold's cohort is deleted.
		r := newRig(t)
		c := r.client(t)
		const holdID = "h-cohort-older"
		current := row(t, c)
		prepared, err := c.MoveRow(ctx, current.Gen, natsgrant.OperatorPhasePrepare, holdID, "prepare")
		if err != nil {
			t.Fatal(err)
		}
		if err := c.StartJournal(ctx, holdID, holdrecord.WorkflowBucketsCreate, nil); err != nil {
			t.Fatal(err)
		}
		if _, err := c.Admit(ctx, holdID, cmd(holdrecord.CommandSet), nil); err != nil {
			t.Fatal(err)
		}
		inPrepare := saveJournal(t, r, holdID)
		if _, err := c.MoveRow(ctx, prepared.Gen, natsgrant.OperatorPhaseHeld, holdID, "held"); err != nil {
			t.Fatal(err)
		}
		if err := c.AppendDone(ctx, holdID, holdrecord.CommandSet, nil); err != nil {
			t.Fatal(err)
		}
		if _, err := c.PutRecord(ctx, holdID, "cohort", "cohort", 0, json.RawMessage(`{"raised":["a"]}`)); err != nil {
			t.Fatal(err)
		}
		run(t, c, holdID, cmd("buckets create"), nil, nil)
		release(t, c, holdID)
		putJournalBack(t, r, holdID, inPrepare)
		var refusal *holdrecord.RouteError
		if err := c.DeleteCohort(ctx, holdID); !errors.Is(err, holdrecord.ErrUnknown) || !errors.As(err, &refusal) || refusal.Code != "below_floor" {
			t.Errorf("DeleteCohort over the journal put back from prepare answered %v, want 409 below_floor, read as ErrUnknown", err)
		}
		var n int
		if err := r.Pool.QueryRow(ctx, `select count(*) from nats_hold_records where hold_id = $1 and kind = 'cohort'`, holdID).Scan(&n); err != nil || n != 1 {
			t.Errorf("after the refused delete the table holds %d cohort records (%v), want 1", n, err)
		}
	})

	t.Run("a hold_mismatch against a row that does not verify names no hold", func(t *testing.T) {
		// TN's S12: a writer puts an unsigned row naming a hold of its choice. Fails while the
		// refusal's hold id comes from that row: "chosen".
		r := newRig(t)
		c := r.client(t)
		current := row(t, c)
		prepared, err := c.MoveRow(ctx, current.Gen, natsgrant.OperatorPhasePrepare, "y", "prepare")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := r.Pool.Exec(ctx, `update nats_hold set hold_id = 'chosen', sig = 'AAAA'`); err != nil {
			t.Fatal(err)
		}
		_, err = c.MoveRow(ctx, prepared.Gen, natsgrant.OperatorPhaseHeld, "y", "held")
		var refusal *holdrecord.RouteError
		if !errors.As(err, &refusal) || refusal.HoldID != "" {
			t.Errorf("MoveRow against the unsigned row answered %v (hold id %q), want a refusal naming no hold", err, refusalHoldID(refusal))
		}
	})
}

// refusalHoldID is the refusal's hold id, "" for none.
func refusalHoldID(r *holdrecord.RouteError) string {
	if r == nil {
		return ""
	}
	return r.HoldID
}
