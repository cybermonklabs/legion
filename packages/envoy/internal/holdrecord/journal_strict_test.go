package holdrecord_test

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/bus/signedrecord"
	"github.com/sjawhar/envoy/internal/holdrecord"
	"github.com/sjawhar/envoy/natsgrant"
)

// putBackEntry inserts a log entry the operator's key signed at gen, an hour before now: an entry
// of another history of the tables, put back by a database writer.
func putBackEntry(t *testing.T, r rig, gen uint64, phase natsgrant.OperatorPhase, holdID string) {
	t.Helper()
	at := time.Now().Add(-time.Hour).UTC().Truncate(time.Microsecond)
	sig, err := signedrecord.Sign(r.SigningKey, signedrecord.HoldRow(gen, string(phase), holdID, at))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.Pool.Exec(context.Background(), `insert into nats_hold_log (gen, phase, hold_id, reason, written_at, written_by, signed_at, sig) values ($1, $2, $3, 'another history', $4, 'op', $4, $5)`,
		int64(gen), string(phase), holdID, at, sig); err != nil {
		t.Fatal(err)
	}
}

// loseRow deletes the hold row, as a database writer can.
func loseRow(t *testing.T, r rig) {
	t.Helper()
	if _, err := r.Pool.Exec(context.Background(), `delete from nats_hold`); err != nil {
		t.Fatal(err)
	}
}

// rowStored is the hold row's gen and phase as the table holds it, gen 0 for no row.
func rowStored(t *testing.T, r rig) (int64, string) {
	t.Helper()
	var gen int64
	var phase string
	if err := r.Pool.QueryRow(context.Background(), `select gen, phase from nats_hold`).Scan(&gen, &phase); err != nil && !errors.Is(err, pgx.ErrNoRows) {
		t.Fatal(err)
	}
	return gen, phase
}

// lostRowGen is the gen a restore of the lost row must carry, which Bootstrap's RestoreGenError
// names.
func lostRowGen(t *testing.T, c *holdrecord.Client) uint64 {
	t.Helper()
	var mismatch *holdrecord.RestoreGenError
	if err := c.Bootstrap(context.Background()); !errors.As(err, &mismatch) {
		t.Fatalf("Bootstrap with the row lost answered %v, want a RestoreGenError", err)
	}
	return mismatch.RestoreGen
}

// strictEndRows are TestTheHoldJournal's rows of the live hold's own released entry from another
// history put back above its entries (B5's shape: a point-in-time restore taken while the hold was
// prepared, whose first history released it), in each shape a live hold's journal has: hold set's
// intent pending with the row held, hold set done and idle, a command done and the next not
// started, and a command stopped. A verified released entry ends its hold only while the hold's
// journal shows hold release's or hold abort's intent as that command's latest entry, the end
// that history admitted, and a journal without a done hold set counts as held while the verified
// log holds its held entry, so each is refused; and an ended hold's older journal, put back, is
// refused naming that cause.
func strictEndRows(t *testing.T) {
	ctx := context.Background()
	// b5 runs B5's attack on the live hold setup leaves: its own released entry from another
	// history put back at the next gen, the row deleted, one replica restarted (a deploy), and the
	// released restore sent to it. It asserts the refusal names want, and no row is stored.
	b5 := func(t *testing.T, setup func(t *testing.T, c *holdrecord.Client) holdrecord.Row, want ...string) (rig, holdrecord.Row) {
		t.Helper()
		r := newRig(t)
		held := setup(t, r.client(t))
		putBackEntry(t, r, held.Gen+1, natsgrant.OperatorPhaseReleased, "h-live")
		loseRow(t, r)
		r.Restart()
		fresh := r.client(t)
		got := restoreRefusal(t, fresh, lostRowGen(t, fresh), natsgrant.OperatorPhaseReleased, "")
		want = append([]string{"hold h-live's journal", "the log's released entry naming it, at gen", "did not end it", handRepair}, want...)
		if !refusedWith(got, http.StatusBadRequest, "bad_transition", want...) {
			t.Errorf("the released restore answered %s; want 400 bad_transition naming %q", refusalText(got), want)
		}
		if gen, phase := rowStored(t, r); gen != 0 {
			p, known := r.HoldView(true).State(ctx)
			t.Errorf("the table holds %s at gen %d while h-live is held; the replica that never restarted reads %q, known %v", phase, gen, p, known)
		}
		return r, held
	}
	noEnd := "since its journal shows neither hold release's nor hold abort's intent as that command's latest entry"

	t.Run("B5-SET: hold set's intent pending, the row held: the released restore is refused", func(t *testing.T) {
		// Claude's round-10 probe B5-SET. Fails while a journal with no done hold set is skipped
		// even after the row moved to held, as at 886868daac: released is stored, and the replica
		// that never restarted reads it, known.
		b5(t, func(t *testing.T, c *holdrecord.Client) holdrecord.Row {
			return prepare(t, c, "h-live", holdrecord.WorkflowBucketsCreate, nil)
		}, "hold set's intent pending", "its held entry at gen 3")
	})

	t.Run("B2-SET: another history's prepare put back while hold set runs: the released restore naming it is refused", func(t *testing.T) {
		// Claude's round-10 probe B2-SET. Fails as B5-SET does: released naming h-other is stored,
		// and both replicas read it, known.
		r := newRig(t)
		held := prepare(t, r.client(t), "h-live", holdrecord.WorkflowBucketsCreate, nil)
		putBackEntry(t, r, held.Gen+2, natsgrant.OperatorPhasePrepare, "h-other")
		loseRow(t, r)
		r.Restart()
		fresh := r.client(t)
		got := restoreRefusal(t, fresh, lostRowGen(t, fresh), natsgrant.OperatorPhaseReleased, "h-other")
		if !refusedWith(got, http.StatusBadRequest, "bad_transition", "hold h-live's journal", "hold set's intent pending", "its held entry at gen 3",
			`prepare at gen 5 with hold id "h-other"`, handRepair) {
			t.Errorf("the released restore naming h-other answered %s; want 400 bad_transition naming h-live's journal, its held entry, the put-back entry and the hand repair", refusalText(got))
		}
		if gen, phase := rowStored(t, r); gen != 0 {
			p, known := r.HoldView(true).State(ctx)
			t.Errorf("the table holds %s at gen %d while h-live is held; the replica that never restarted reads %q, known %v", phase, gen, p, known)
		}
	})

	t.Run("B5-IDLE: hold set done, no command run yet: the released restore is refused", func(t *testing.T) {
		// Claude's B5-IDLE and TN deep's R10-1 first state. Fails while a released entry ends any
		// hold with no intent pending (option A alone, as at 886868daac): released is stored, and
		// both replicas read it, known.
		b5(t, func(t *testing.T, c *holdrecord.Client) holdrecord.Row {
			return hold(t, c, "h-live", holdrecord.WorkflowBucketsCreate, nil)
		}, noEnd)
	})

	t.Run("a command done, the next not started: the released restore is refused", func(t *testing.T) {
		// TN deep's R10-1 second state. Fails as B5-IDLE does.
		b5(t, func(t *testing.T, c *holdrecord.Client) holdrecord.Row {
			held := hold(t, c, "h-live", holdrecord.WorkflowBucketsCreate, nil)
			run(t, c, "h-live", cmd("buckets create"), nil, nil)
			return held
		}, noEnd)
	})

	t.Run("a command stopped after leaving a change: the released restore is refused", func(t *testing.T) {
		// TN deep's R10-1 third state. Fails as B5-IDLE does.
		b5(t, func(t *testing.T, c *holdrecord.Client) holdrecord.Row {
			held := hold(t, c, "h-live", holdrecord.WorkflowBucketsCreate, nil)
			if _, err := c.Admit(ctx, "h-live", cmd("buckets create"), nil); err != nil {
				t.Fatal(err)
			}
			if err := c.AppendStopLeft(ctx, "h-live", "buckets create", "refused after its change", "bucket b1 created"); err != nil {
				t.Fatal(err)
			}
			return held
		}, noEnd)
	})

	t.Run("B5-STOPPED: a command stopped, abort not yet run: refused; after the README's repair, the abort is admitted", func(t *testing.T) {
		// Claude's B5-STOPPED. Fails while option A ends the stopped hold, as at 886868daac:
		// released is stored, and hold abort, the stop's way out, is then refused on the released
		// row. With the refusal, infra/README.md's repair for an entry from another history
		// (delete it by gen, restart, restore the row held) leaves the abort admitted.
		readmeSays(t, logDelete)
		r, held := b5(t, func(t *testing.T, c *holdrecord.Client) holdrecord.Row {
			held := hold(t, c, "h-live", holdrecord.WorkflowBucketsCreate, nil)
			if _, err := c.Admit(ctx, "h-live", cmd("buckets create"), nil); err != nil {
				t.Fatal(err)
			}
			if err := c.AppendStop(ctx, "h-live", "buckets create", "refused"); err != nil {
				t.Fatal(err)
			}
			return held
		}, noEnd)
		if _, err := r.Pool.Exec(ctx, `delete from nats_hold_log where gen = $1`, int64(held.Gen+1)); err != nil {
			t.Fatal(err)
		}
		r.Restart()
		op := r.client(t)
		if _, err := op.RestoreRow(ctx, held.Gen+1, natsgrant.OperatorPhaseHeld, "h-live", "after the repair"); err != nil {
			t.Fatalf("after the repair the held restore answered %v; want it stored", err)
		}
		if _, err := op.Admit(ctx, "h-live", cmd(holdrecord.CommandAbort), nil); err != nil {
			t.Errorf("after the repair hold abort answered %v; want it admitted", err)
		}
	})

	t.Run("S8I: an ended hold's journal from while a command ran, put back after a restart: refused, naming the older journal", func(t *testing.T) {
		// Claude's round-10 probe S8I (Legion M-3). The released entry naming h-a is genuine, and
		// its journal is an older version put back, so the repair is the journal from a backup,
		// not deleting the released entry. Fails while the refusal names only the causes of a
		// live hold, as at 886868daac.
		r := newRig(t)
		c := r.client(t)
		hold(t, c, "h-a", holdrecord.WorkflowBucketsCreate, nil)
		if _, err := c.Admit(ctx, "h-a", cmd("buckets create"), nil); err != nil {
			t.Fatal(err)
		}
		mid := saveJournal(t, r, "h-a")
		if err := c.AppendDone(ctx, "h-a", "buckets create", nil); err != nil {
			t.Fatal(err)
		}
		release(t, c, "h-a")
		putJournalBack(t, r, "h-a", mid)
		loseRow(t, r)
		r.Restart()
		fresh := r.client(t)
		got := restoreRefusal(t, fresh, lostRowGen(t, fresh), natsgrant.OperatorPhaseReleased, "")
		if !refusedWith(got, http.StatusBadRequest, "bad_transition", "hold h-a's journal", "buckets create's intent pending",
			"an older version of this hold's journal put back", handRepair) {
			t.Errorf("the released restore answered %s; want 400 bad_transition naming h-a's journal, its pending intent, the older journal and the hand repair", refusalText(got))
		}
	})
}

// normalEndRows are TestTheHoldJournal's rows of every normal way a hold ends, with no database
// writer (Claude's round-10 TestReview4679R10NormalEnds): the row lost after it, the released
// restore on the same replica with no restart; then a second hold run to its end, the row lost
// again, a restart, and the released restore again. Each stores. Fails with a rule under which no
// released entry ends a hold and only a done end does (round 9's option B): the endings cut
// before their done entry are refused.
func normalEndRows(t *testing.T) {
	ctx := context.Background()
	moveReleased := func(t *testing.T, c *holdrecord.Client, holdID string) {
		t.Helper()
		if _, err := c.MoveRow(ctx, row(t, c).Gen, natsgrant.OperatorPhaseReleased, holdID, "ended"); err != nil {
			t.Fatalf("move the row to released: %v", err)
		}
	}
	admit := func(t *testing.T, c *holdrecord.Client, holdID, command string) {
		t.Helper()
		if _, err := c.Admit(ctx, holdID, cmd(command), nil); err != nil {
			t.Fatalf("admit %s: %v", command, err)
		}
	}
	endings := []struct {
		name string
		end  func(t *testing.T, c *holdrecord.Client)
	}{
		{"release", func(t *testing.T, c *holdrecord.Client) {
			hold(t, c, "h-1", holdrecord.WorkflowBucketsCreate, nil)
			run(t, c, "h-1", cmd("buckets create"), nil, nil)
			release(t, c, "h-1")
		}},
		{"abort after a stop, done", func(t *testing.T, c *holdrecord.Client) {
			hold(t, c, "h-1", holdrecord.WorkflowBucketsCreate, nil)
			admit(t, c, "h-1", "buckets create")
			if err := c.AppendStop(ctx, "h-1", "buckets create", "refused"); err != nil {
				t.Fatal(err)
			}
			admit(t, c, "h-1", holdrecord.CommandAbort)
			moveReleased(t, c, "h-1")
			if err := c.AppendDone(ctx, "h-1", holdrecord.CommandAbort, nil); err != nil {
				t.Fatal(err)
			}
		}},
		{"abort after a stop, cut before its done entry", func(t *testing.T, c *holdrecord.Client) {
			hold(t, c, "h-1", holdrecord.WorkflowBucketsCreate, nil)
			admit(t, c, "h-1", "buckets create")
			if err := c.AppendStop(ctx, "h-1", "buckets create", "refused"); err != nil {
				t.Fatal(err)
			}
			admit(t, c, "h-1", holdrecord.CommandAbort)
			moveReleased(t, c, "h-1")
		}},
		{"a cut run rerun to done, then release cut before its done entry", func(t *testing.T, c *holdrecord.Client) {
			hold(t, c, "h-1", holdrecord.WorkflowBucketsCreate, nil)
			admit(t, c, "h-1", "buckets create")
			admit(t, c, "h-1", "buckets create")
			if err := c.AppendDone(ctx, "h-1", "buckets create", nil); err != nil {
				t.Fatal(err)
			}
			admit(t, c, "h-1", holdrecord.CommandRelease)
			moveReleased(t, c, "h-1")
		}},
		{"release over a stop that left nothing, cut before its done entry", func(t *testing.T, c *holdrecord.Client) {
			hold(t, c, "h-1", holdrecord.WorkflowBucketsCreate, nil)
			admit(t, c, "h-1", "buckets create")
			if err := c.AppendStopLeft(ctx, "h-1", "buckets create", "refused", holdrecord.LeftUnchanged); err != nil {
				t.Fatal(err)
			}
			admit(t, c, "h-1", holdrecord.CommandRelease)
			moveReleased(t, c, "h-1")
		}},
		{"hold set stopped, abort cut before its done entry", func(t *testing.T, c *holdrecord.Client) {
			prepare(t, c, "h-1", holdrecord.WorkflowBucketsCreate, nil)
			if err := c.AppendStop(ctx, "h-1", holdrecord.CommandSet, "the set's steps failed"); err != nil {
				t.Fatal(err)
			}
			admit(t, c, "h-1", holdrecord.CommandAbort)
			moveReleased(t, c, "h-1")
		}},
		{"abort from prepare, hold set's intent pending", func(t *testing.T, c *holdrecord.Client) {
			prepared, err := c.MoveRow(ctx, row(t, c).Gen, natsgrant.OperatorPhasePrepare, "h-1", "prepare")
			if err != nil {
				t.Fatal(err)
			}
			if err := c.StartJournal(ctx, "h-1", holdrecord.WorkflowBucketsCreate, nil); err != nil {
				t.Fatal(err)
			}
			admit(t, c, "h-1", holdrecord.CommandSet)
			if err := c.DeleteCohort(ctx, "h-1"); err != nil {
				t.Fatal(err)
			}
			if _, err := c.MoveRow(ctx, prepared.Gen, natsgrant.OperatorPhaseReleased, "h-1", "abort from prepare"); err != nil {
				t.Fatal(err)
			}
		}},
	}
	for _, e := range endings {
		t.Run("a normal ending, then a lost row, restores released: "+e.name, func(t *testing.T) {
			r := newRig(t)
			e.end(t, r.client(t))
			loseRow(t, r)
			same := r.client(t)
			if got := restoreRefusal(t, same, lostRowGen(t, same), natsgrant.OperatorPhaseReleased, ""); got != nil {
				t.Fatalf("on the same replica, with no restart, the released restore answered %s; want it stored", refusalText(got))
			}
			again := r.client(t)
			hold(t, again, "h-2", holdrecord.WorkflowBucketsCreate, nil)
			run(t, again, "h-2", cmd("buckets create"), nil, nil)
			release(t, again, "h-2")
			loseRow(t, r)
			r.Restart()
			fresh := r.client(t)
			if got := restoreRefusal(t, fresh, lostRowGen(t, fresh), natsgrant.OperatorPhaseReleased, ""); got != nil {
				t.Errorf("after hold h-2 and a restart, the released restore answered %s; want it stored", refusalText(got))
			}
		})
	}
}

// everyEndingRows are TestTheHoldJournal's rows of TN deep's fault matrix (R10-M): every way a
// hold ends, each followed by a lost row at once, or by hold B run to its end and then a lost row;
// each lost row tried on the same replica, the second replica and after a restart, with every
// released and held restore the operator could send. Each leaves a restore that stores, with no
// hand repair.
func everyEndingRows(t *testing.T) {
	ctx := context.Background()
	must := func(t *testing.T, what string, err error) {
		t.Helper()
		if err != nil {
			t.Fatalf("%s: %v", what, err)
		}
	}
	// tryRestore restores phase naming holdID through a fresh client at the gen Bootstrap names,
	// and answers "" when it was stored, or what refused it.
	tryRestore := func(t *testing.T, r rig, phase natsgrant.OperatorPhase, holdID string) string {
		t.Helper()
		c := r.client(t)
		gen := uint64(1)
		var mismatch *holdrecord.RestoreGenError
		switch err := c.Bootstrap(ctx); {
		case errors.As(err, &mismatch):
			gen = mismatch.RestoreGen
		case err == nil:
			return ""
		default:
			return "Bootstrap: " + err.Error()
		}
		if _, err := c.RestoreRow(ctx, gen, phase, holdID, "the lost row"); err != nil {
			var refusal *holdrecord.RouteError
			if errors.As(err, &refusal) {
				return refusalText(refusal)
			}
			return err.Error()
		}
		return ""
	}
	restoreOf := func(t *testing.T, r rig, phase natsgrant.OperatorPhase, holdID string) {
		t.Helper()
		if got := tryRestore(t, r, phase, holdID); got != "" {
			t.Fatalf("restore %s naming %q after the lost row: %s", phase, holdID, got)
		}
	}
	stopThenAbort := func(t *testing.T, c *holdrecord.Client, cutBeforeDone bool) {
		held := hold(t, c, "A", holdrecord.WorkflowBucketsCreate, nil)
		_, err := c.Admit(ctx, "A", cmd("buckets create"), nil)
		must(t, "admit buckets create", err)
		must(t, "stop buckets create", c.AppendStopLeft(ctx, "A", "buckets create", "refused after its change", "bucket b1 created"))
		_, err = c.Admit(ctx, "A", cmd(holdrecord.CommandAbort), nil)
		must(t, "admit abort", err)
		_, err = c.MoveRow(ctx, held.Gen, natsgrant.OperatorPhaseReleased, "A", "hold abort")
		must(t, "move released", err)
		if !cutBeforeDone {
			must(t, "abort done", c.AppendDone(ctx, "A", holdrecord.CommandAbort, nil))
		}
	}
	endings := []struct {
		name string
		end  func(t *testing.T, r rig, c *holdrecord.Client)
	}{
		{"release", func(t *testing.T, _ rig, c *holdrecord.Client) {
			hold(t, c, "A", holdrecord.WorkflowStreamsUpdate, nil)
			release(t, c, "A")
		}},
		{"release cut before its done entry", func(t *testing.T, _ rig, c *holdrecord.Client) {
			held := hold(t, c, "A", holdrecord.WorkflowStreamsUpdate, nil)
			_, err := c.Admit(ctx, "A", cmd(holdrecord.CommandRelease), nil)
			must(t, "admit release", err)
			_, err = c.MoveRow(ctx, held.Gen, natsgrant.OperatorPhaseReleased, "A", "release")
			must(t, "move released", err)
		}},
		{"abort from held after a stop", func(t *testing.T, _ rig, c *holdrecord.Client) { stopThenAbort(t, c, false) }},
		{"abort from held after a stop, cut before its done entry", func(t *testing.T, _ rig, c *holdrecord.Client) { stopThenAbort(t, c, true) }},
		{"abort from prepare, journal started, hold set's intent pending", func(t *testing.T, _ rig, c *holdrecord.Client) {
			p, err := c.MoveRow(ctx, row(t, c).Gen, natsgrant.OperatorPhasePrepare, "A", "prepare")
			must(t, "prepare", err)
			must(t, "start journal", c.StartJournal(ctx, "A", holdrecord.WorkflowStreamsUpdate, nil))
			_, err = c.Admit(ctx, "A", cmd(holdrecord.CommandSet), nil)
			must(t, "admit set", err)
			must(t, "delete cohort", c.DeleteCohort(ctx, "A"))
			_, err = c.MoveRow(ctx, p.Gen, natsgrant.OperatorPhaseReleased, "A", "hold abort from prepare")
			must(t, "move released", err)
		}},
		{"abort from prepare, no journal", func(t *testing.T, _ rig, c *holdrecord.Client) {
			p, err := c.MoveRow(ctx, row(t, c).Gen, natsgrant.OperatorPhasePrepare, "A", "prepare")
			must(t, "prepare", err)
			must(t, "delete cohort", c.DeleteCohort(ctx, "A"))
			_, err = c.MoveRow(ctx, p.Gen, natsgrant.OperatorPhaseReleased, "A", "hold abort from prepare")
			must(t, "move released", err)
		}},
		{"lost in prepare with a journal, restored released, aborted", func(t *testing.T, r rig, c *holdrecord.Client) {
			_, err := c.MoveRow(ctx, row(t, c).Gen, natsgrant.OperatorPhasePrepare, "A", "prepare")
			must(t, "prepare", err)
			must(t, "start journal", c.StartJournal(ctx, "A", holdrecord.WorkflowStreamsUpdate, nil))
			loseRow(t, r)
			restoreOf(t, r, natsgrant.OperatorPhaseReleased, "A")
			fresh := r.client(t)
			_, err = fresh.Admit(ctx, "A", cmd(holdrecord.CommandAbort), nil)
			must(t, "admit abort on the released row", err)
			must(t, "delete cohort", fresh.DeleteCohort(ctx, "A"))
			must(t, "abort done", fresh.AppendDone(ctx, "A", holdrecord.CommandAbort, nil))
		}},
		{"lost in prepare with no journal, restored released", func(t *testing.T, r rig, c *holdrecord.Client) {
			_, err := c.MoveRow(ctx, row(t, c).Gen, natsgrant.OperatorPhasePrepare, "A", "prepare")
			must(t, "prepare", err)
			loseRow(t, r)
			restoreOf(t, r, natsgrant.OperatorPhaseReleased, "A")
		}},
		{"lost in held, restored held, released", func(t *testing.T, r rig, c *holdrecord.Client) {
			hold(t, c, "A", holdrecord.WorkflowStreamsUpdate, nil)
			loseRow(t, r)
			restoreOf(t, r, natsgrant.OperatorPhaseHeld, "A")
			release(t, r.client(t), "A")
		}},
		{"lost after release's intent, restored held, release rerun", func(t *testing.T, r rig, c *holdrecord.Client) {
			hold(t, c, "A", holdrecord.WorkflowStreamsUpdate, nil)
			_, err := c.Admit(ctx, "A", cmd(holdrecord.CommandRelease), nil)
			must(t, "admit release", err)
			loseRow(t, r)
			restoreOf(t, r, natsgrant.OperatorPhaseHeld, "A")
			release(t, r.client(t), "A")
		}},
	}
	for _, e := range endings {
		for _, then := range []string{"lost at once", "hold B, then lost"} {
			for _, where := range []string{"same replica", "second replica", "restarted"} {
				t.Run("every ending leaves a restore: "+e.name+"/"+then+"/"+where, func(t *testing.T) {
					r := newRig(t)
					e.end(t, r, r.client(t))
					if then == "hold B, then lost" {
						hold(t, r.client(t), "B", holdrecord.WorkflowStreamsUpdate, nil)
						release(t, r.client(t), "B")
					}
					loseRow(t, r)
					switch where {
					case "second replica":
						r.UseReplica(true)
						defer r.UseReplica(false)
					case "restarted":
						r.Restart()
					}
					var refusals []string
					for _, name := range []string{"", "A", "B"} {
						for _, phase := range []natsgrant.OperatorPhase{natsgrant.OperatorPhaseReleased, natsgrant.OperatorPhaseHeld} {
							if phase == natsgrant.OperatorPhaseHeld && name == "" {
								continue
							}
							got := tryRestore(t, r, phase, name)
							if got == "" {
								return
							}
							refusals = append(refusals, string(phase)+" naming "+name+": "+got)
						}
					}
					t.Errorf("no restore stored: %s", strings.Join(refusals, "; "))
				})
			}
		}
	}
}
