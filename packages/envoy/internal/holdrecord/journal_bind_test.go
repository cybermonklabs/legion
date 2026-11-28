package holdrecord_test

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/sjawhar/envoy/internal/bus/signedrecord"
	"github.com/sjawhar/envoy/internal/holdrecord"
	"github.com/sjawhar/envoy/natsgrant"
)

// journalBindingRows are TestTheHoldJournal's rows of a journal bound to its history: each entry
// names the hold row it was written under (Operation.Row, the row Admit read for an intent, the row
// read when a done or stop entry is written), and the restore reads a journal as unknown (409
// below_floor, naming the hold, the entry and the hand repair) when an entry's row is not in the
// verified log, or when an entry names no row. Another history's journal of the live hold, written
// over this one's at a higher gen without the log entries it names, then frees no client; put back
// together with them, it binds and stores (residualRows' FORK-WHOLE, which B5-JOURNAL controls).
func journalBindingRows(t *testing.T) {
	ctx := context.Background()
	exec := func(t *testing.T, r rig, sql string, args ...any) {
		t.Helper()
		if _, err := r.Pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	liveHold := func(t *testing.T, c *holdrecord.Client) holdrecord.Row {
		t.Helper()
		held := hold(t, c, "h-live", holdrecord.WorkflowBucketsCreate, nil)
		if _, err := c.Admit(ctx, "h-live", cmd("buckets create"), nil); err != nil {
			t.Fatal(err)
		}
		return held
	}

	t.Run("B5-JOURNAL: another history's journal of the live hold, at a higher gen, with no restart: refused, naming the row its log does not hold", func(t *testing.T) {
		// Claude's round-10 probe B5-JOURNAL, as a genuine fork: a point-in-time restore taken
		// while h-live was held with buckets create's intent pending. History 1 ran on the second
		// replica: create done, then hold release, the row moved to released at gen 4. Its journal
		// was saved, the tables restored to the restore point, and the first replica, which never
		// restarted and never saw history 1, serves the restore. Both histories share h-live's held
		// entry, so a journal bound to that entry alone would pass; history 1's release done entry
		// was written under its released row at gen 4, which this log does not hold. Fails with no
		// binding check, as at 886868daac: released is stored, and the first replica reads it.
		r := newRig(t)
		liveHold(t, r.client(t))
		tables := []string{"nats_hold", "nats_hold_log", "nats_hold_records", "nats_hold_record_deletes", "nats_cutover"}
		for _, table := range tables {
			exec(t, r, `create table snap_`+table+` as select * from `+table)
		}
		r.UseReplica(true)
		first := r.client(t)
		if err := first.AppendDone(ctx, "h-live", "buckets create", nil); err != nil {
			t.Fatal(err)
		}
		release(t, first, "h-live")
		history1 := saveJournal(t, r, "h-live")
		r.UseReplica(false)
		for _, table := range tables {
			exec(t, r, `delete from `+table)
			exec(t, r, `insert into `+table+` select * from snap_`+table)
		}
		putJournalBack(t, r, "h-live", history1)
		loseRow(t, r)
		fresh := r.client(t)
		got := restoreRefusal(t, fresh, lostRowGen(t, fresh), natsgrant.OperatorPhaseReleased, "")
		if !refusedWith(got, http.StatusConflict, "below_floor", `hold "h-live"'s journal entry`, "hold release done", "the hold row at gen 4", "which the verified log does not hold", handRepair) {
			t.Errorf("the released restore answered %s; want 409 below_floor naming h-live's hold release done entry, its row at gen 4 and the hand repair", refusalText(got))
		}
		if gen, phase := rowStored(t, r); gen != 0 {
			p, known := r.HoldView(false).State(ctx)
			t.Errorf("the table holds %s at gen %d while h-live is held in this history; the replica that never restarted reads %q, known %v", phase, gen, p, known)
		}
	})

	t.Run("a journal entry that names no hold row it was written under is unknown", func(t *testing.T) {
		// Claude's round-10 probe B5-JOURNAL as written: h-live's journal with entries appended
		// that name no row (create done, then hold release), signed by the operator at a higher
		// gen. Every entry the operator's client writes names its row, and this PR writes the
		// first journals, so an entry that names none is no journal of the routes. Fails with no
		// binding check, as at 886868daac: released is stored on the replica that never
		// restarted.
		r := newRig(t)
		c := r.client(t)
		liveHold(t, c)
		d, gen := journal(t, c, "h-live")
		d.Ops = append(d.Ops,
			holdrecord.Operation{Command: "buckets create", State: holdrecord.StateDone},
			holdrecord.Operation{Command: holdrecord.CommandRelease, State: holdrecord.StateIntent},
			holdrecord.Operation{Command: holdrecord.CommandRelease, State: holdrecord.StateDone})
		d.Complete = true
		value, err := json.Marshal(d)
		if err != nil {
			t.Fatal(err)
		}
		sig, err := signedrecord.Sign(r.SigningKey, signedrecord.Record("h-live", "done", "hold", gen+3, value))
		if err != nil {
			t.Fatal(err)
		}
		exec(t, r, `update nats_hold_records set value = $1::jsonb, gen = $2, sig = $3 where hold_id = 'h-live' and kind = 'done'`, string(value), int64(gen+3), sig)
		loseRow(t, r)
		fresh := r.client(t)
		got := restoreRefusal(t, fresh, lostRowGen(t, fresh), natsgrant.OperatorPhaseReleased, "")
		if !refusedWith(got, http.StatusConflict, "below_floor", `hold "h-live"'s journal entry`, "buckets create done", "names no hold row it was written under", handRepair) {
			t.Errorf("the released restore answered %s; want 409 below_floor naming h-live's entry that names no row and the hand repair", refusalText(got))
		}
		if g, phase := rowStored(t, r); g != 0 {
			t.Errorf("the table holds %s at gen %d, want no row", phase, g)
		}
	})

	t.Run("a log cut back below an ended hold's entries, its journal kept: refused, naming the row the log lost; the README's repair stores on every replica", func(t *testing.T) {
		// A database writer's residual infra/README.md lists: the log cut back below h-a's
		// entries leaves h-a's journal naming rows the log no longer holds, which the restore
		// cannot tell from another history's journal put back alone, so it reads it as unknown;
		// the hand repair deletes the journal by name, then restarts each replica that served the
		// row before the cut (the Legion reviewer's round-11 M-6: the second replica, which served
		// the row and did not start again, keeps refusing the cut log under it). Fails with no
		// binding check, as at 886868daac: released is stored; and on a0a2c7622f's README, which
		// names no restart after the delete.
		readmeSays(t, cutLogRestart)
		r := newRig(t)
		c := r.client(t)
		hold(t, c, "h-a", holdrecord.WorkflowBucketsCreate, nil)
		run(t, c, "h-a", cmd("buckets create"), nil, nil)
		release(t, c, "h-a")
		r.UseReplica(true)
		row(t, r.client(t))
		r.UseReplica(false)
		exec(t, r, `delete from nats_hold_log where gen > 1`)
		loseRow(t, r)
		r.Restart()
		fresh := r.client(t)
		got := restoreRefusal(t, fresh, lostRowGen(t, fresh), natsgrant.OperatorPhaseReleased, "")
		if !refusedWith(got, http.StatusConflict, "below_floor", `hold "h-a"'s journal entry`, "the hold row at gen 2", "which the verified log does not hold", handRepair) {
			t.Errorf("the released restore answered %s; want 409 below_floor naming h-a's entry written under the row at gen 2 and the hand repair", refusalText(got))
		}
		exec(t, r, `delete from nats_hold_records where hold_id = 'h-a' and kind = 'done'`)
		gen := lostRowGen(t, r.client(t))
		r.UseReplica(true)
		defer r.UseReplica(false)
		if got := restoreRefusal(t, r.client(t), gen, natsgrant.OperatorPhaseReleased, ""); got == nil || got.Code != "below_floor" {
			t.Errorf("after the delete, on the replica that served the row and did not start again, the released restore at gen %d answered %s; want 409 below_floor", gen, refusalText(got))
		}
		r.RestartSecond()
		second := r.client(t)
		if got := restoreRefusal(t, second, lostRowGen(t, second), natsgrant.OperatorPhaseReleased, ""); got != nil {
			t.Errorf("after the delete and each replica's restart, the released restore answered %s; want it stored", refusalText(got))
		}
		for _, onSecond := range []bool{false, true} {
			if p, known := r.HoldView(onSecond).State(ctx); !known || p != natsgrant.OperatorPhaseReleased {
				t.Errorf("after the repair, replica second=%v reads %q, known %v; want released, known", onSecond, p, known)
			}
		}
	})
}

// cutLogRestart is infra/README.md's step after the cut-back log's delete by name, which the row
// above follows.
const cutLogRestart = "then restart each broker replica that served the row before the cut, since a replica that did not start again keeps refusing the cut log under the row it served"
