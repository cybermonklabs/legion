package holdrecord_test

import (
	"context"
	"testing"

	"github.com/sjawhar/envoy/internal/holdrecord"
	"github.com/sjawhar/envoy/natsgrant"
)

// forkWholeResidual and abortIntentResidual are infra/README.md's words for two cases that still
// store a released row while a hold is live ("Restoring a lost hold row"), which the rows below
// pin: each row reads the README for its case, so a fix that refuses the case flips the row on
// purpose, and the README stops stating a residual the code no longer has.
const (
	forkWholeResidual   = "another history's journal of the live hold put back together with the log entries it names, from a fork taken while the row was `held`"
	forkWholeRemedy     = "A floor both replicas share, kept outside the database, is the remedy for this case"
	abortIntentResidual = "`hold release`'s or `hold abort`'s intent written and the row not yet moved"
	abortIntentCheck    = "`hold abort` compares each disk with the done prove's record, and may write a stop"
)

// residualRows are TestTheHoldJournal's rows of the cases infra/README.md says still store a
// released row while a hold is live, each a database writer's: the fork put back whole (Claude's
// round-11 FORK-WHOLE, TN deep's R11-1, the Legion reviewer's N-G), and an abort's pending intent
// beside a put-back released entry (Claude's ABORT-INTENT). Each pins today's behaviour, the row
// stored and both replicas reading released, known; journalBindingRows' B5-JOURNAL is their
// control, the journal put back without the log entries it names, which is refused 409.
func residualRows(t *testing.T) {
	ctx := context.Background()
	exec := func(t *testing.T, r rig, sql string, args ...any) {
		t.Helper()
		if _, err := r.Pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	// storedMidHold asserts the released restore, sent to the replica that never restarted, is
	// stored, and that both replicas then read released, known.
	storedMidHold := func(t *testing.T, r rig) {
		t.Helper()
		fresh := r.client(t)
		if got := restoreRefusal(t, fresh, lostRowGen(t, fresh), natsgrant.OperatorPhaseReleased, ""); got != nil {
			t.Fatalf("the released restore answered %s; the README states this case stores, so a refusal is a fix: flip this row and the README together", refusalText(got))
		}
		if gen, phase := rowStored(t, r); gen == 0 || phase != string(natsgrant.OperatorPhaseReleased) {
			t.Errorf("the table holds %q at gen %d, want released", phase, gen)
		}
		for _, second := range []bool{false, true} {
			if p, known := r.HoldView(second).State(ctx); !known || p != natsgrant.OperatorPhaseReleased {
				t.Errorf("replica second=%v reads %q, known %v; want released, known", second, p, known)
			}
		}
	}

	for _, c := range []struct {
		name    string
		h1Done  bool
		h2Stops bool
	}{
		{"history 1 released with its done entry, history 2 stopped after a change", true, true},
		{"history 1 released, cut before its done entry, history 2 stopped after a change", false, true},
		{"history 1 released with its done entry, history 2 idle", true, false},
	} {
		t.Run("FORK-WHOLE: another history's journal and the log entries it names put back, no restart: stored, the README's residual: "+c.name, func(t *testing.T) {
			// A fork taken while h-live is held with buckets create's intent pending. History 1
			// runs on the second replica: create done, then hold release (or its intent and the
			// row's move, cut before the done entry). The tables go back to the fork; history 2
			// runs on the first replica, which never saw history 1. A database writer puts back
			// history 1's journal and its log entries above the fork, and deletes the row. Every
			// entry history 1 wrote while the row sat at held names the held row both histories
			// share, and the entry it wrote under a row it moved to, its release's done entry,
			// names its released row, which the writer put back, so the journal binds. Cut before
			// that done entry, history 1 wrote no entry under a row it moved to, so its journal
			// binds even put back alone (and is then refused 400 for showing hold set done and no
			// end); put back with its released entry it stores, not because of the binding but
			// because that entry ends the hold by the strict-end rule, the journal showing hold
			// release's intent as that command's latest entry. Nothing in the database tells this
			// state from history 1's own lost row.
			readmeSays(t, forkWholeResidual)
			readmeSays(t, forkWholeRemedy)
			r := newRig(t)
			first := r.client(t)
			held := hold(t, first, "h-live", holdrecord.WorkflowBucketsCreate, nil)
			if _, err := first.Admit(ctx, "h-live", cmd("buckets create"), nil); err != nil {
				t.Fatal(err)
			}
			tables := []string{"nats_hold", "nats_hold_log", "nats_hold_records", "nats_hold_record_deletes", "nats_cutover"}
			for _, table := range tables {
				exec(t, r, `create table snap_`+table+` as select * from `+table)
			}
			r.UseReplica(true)
			h1 := r.client(t)
			if err := h1.AppendDone(ctx, "h-live", "buckets create", nil); err != nil {
				t.Fatal(err)
			}
			if c.h1Done {
				release(t, h1, "h-live")
			} else {
				if _, err := h1.Admit(ctx, "h-live", cmd(holdrecord.CommandRelease), nil); err != nil {
					t.Fatal(err)
				}
				if _, err := h1.MoveRow(ctx, held.Gen, natsgrant.OperatorPhaseReleased, "h-live", "released"); err != nil {
					t.Fatal(err)
				}
			}
			history1 := saveJournal(t, r, "h-live")
			exec(t, r, `create table history1_log as select * from nats_hold_log where gen > $1`, int64(held.Gen))
			r.UseReplica(false)
			for _, table := range tables {
				exec(t, r, `delete from `+table)
				exec(t, r, `insert into `+table+` select * from snap_`+table)
			}
			if c.h2Stops {
				if err := r.client(t).AppendStopLeft(ctx, "h-live", "buckets create", "refused after its change", "bucket b1 created"); err != nil {
					t.Fatal(err)
				}
			}
			putJournalBack(t, r, "h-live", history1)
			exec(t, r, `insert into nats_hold_log select * from history1_log`)
			loseRow(t, r)
			storedMidHold(t, r)
		})
	}

	t.Run("ABORT-INTENT: hold abort admitted, the row still held, the live hold's own released entry put back: stored, the README's residual", func(t *testing.T) {
		// Claude's round-11 probe ABORT-INTENT (P3-1). buckets create stopped and hold abort was
		// admitted, its intent pending, the row still held; a database writer puts back the live
		// hold's own released entry from another history and deletes the row; the first replica
		// restarts. The journal shows the end it admitted, so the released entry ends the hold,
		// though in hub-restore the abort's disk comparison, which may write a stop, may not have
		// run. Refusing an abort's intent would refuse the normal ending "abort after a stop, cut
		// before its done entry" (normalEndRows), so this is stated, not refused.
		readmeSays(t, abortIntentResidual)
		readmeSays(t, abortIntentCheck)
		r := newRig(t)
		c := r.client(t)
		held := hold(t, c, "h-live", holdrecord.WorkflowBucketsCreate, nil)
		if _, err := c.Admit(ctx, "h-live", cmd("buckets create"), nil); err != nil {
			t.Fatal(err)
		}
		if err := c.AppendStop(ctx, "h-live", "buckets create", "refused after its change"); err != nil {
			t.Fatal(err)
		}
		if _, err := c.Admit(ctx, "h-live", cmd(holdrecord.CommandAbort), nil); err != nil {
			t.Fatal(err)
		}
		r.UseReplica(true)
		row(t, r.client(t))
		r.UseReplica(false)
		putBackEntry(t, r, held.Gen+1, natsgrant.OperatorPhaseReleased, "h-live")
		loseRow(t, r)
		r.Restart()
		storedMidHold(t, r)
	})
}
