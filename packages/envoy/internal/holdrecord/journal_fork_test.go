package holdrecord_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/bus/signedrecord"
	"github.com/sjawhar/envoy/internal/holdrecord"
	"github.com/sjawhar/envoy/natsgrant"
)

// handRepair is the wording a refusal names when no route can clear what it refused.
const handRepair = "a database admin deletes or restores the rows named here"

// restoreRefusal restores the row at gen through c, as the operator does, and answers the route's
// refusal as the operator's client reads it, its Message included; nil when the row was stored.
func restoreRefusal(t *testing.T, c *holdrecord.Client, gen uint64, phase natsgrant.OperatorPhase, holdID string) *holdrecord.RouteError {
	t.Helper()
	_, err := c.RestoreRow(context.Background(), gen, phase, holdID, "the lost row")
	if err == nil {
		return nil
	}
	var refusal *holdrecord.RouteError
	if !errors.As(err, &refusal) {
		t.Fatalf("RestoreRow of %s naming %q at gen %d answered %v, which is no route's refusal", phase, holdID, gen, err)
	}
	return refusal
}

// refusedWith reports whether got is a refusal of status and code whose Message holds every one
// of want.
func refusedWith(got *holdrecord.RouteError, status int, code string, want ...string) bool {
	if got == nil || got.Status != status || got.Code != code {
		return false
	}
	for _, w := range want {
		if !strings.Contains(got.Message, w) {
			return false
		}
	}
	return true
}

// refusalText is got as a failure prints it: its status, code and message, or that the row was
// stored.
func refusalText(got *holdrecord.RouteError) string {
	if got == nil {
		return "nothing: the row was stored"
	}
	return fmt.Sprintf("%d %s %q", got.Status, got.Code, got.Message)
}

// holdIDOf is the hold id got names, "" for none or a stored row.
func holdIDOf(got *holdrecord.RouteError) string {
	if got == nil {
		return ""
	}
	return got.HoldID
}

// forkRows are TestTheHoldJournal's rows of a released restore while a live hold's journal shows
// it held, and the log carries an entry from another history above the hold's own (N-E): a
// prepare of another hold at a higher gen says nothing about the live hold, since gen order and
// signing order disagree across histories, so each restore is refused, and the refusal names the
// hand repair, since no held restore of the hold is admitted either. A refusal advises the held
// restore only when the route admits it.
func forkRows(t *testing.T) {
	ctx := context.Background()
	stored := func(t *testing.T, r rig) (int64, string, string) {
		t.Helper()
		var gen int64
		var phase, holdID string
		if err := r.Pool.QueryRow(ctx, `select gen, phase, hold_id from nats_hold`).Scan(&gen, &phase, &holdID); err != nil && !errors.Is(err, pgx.ErrNoRows) {
			t.Fatal(err)
		}
		return gen, phase, holdID
	}
	exec := func(t *testing.T, r rig, sql string, args ...any) {
		t.Helper()
		if _, err := r.Pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	restoreGen := func(t *testing.T, c *holdrecord.Client) uint64 {
		t.Helper()
		var mismatch *holdrecord.RestoreGenError
		if err := c.Bootstrap(ctx); !errors.As(err, &mismatch) {
			t.Fatalf("Bootstrap with the row lost answered %v, want a RestoreGenError", err)
		}
		return mismatch.RestoreGen
	}
	endedHold := func(t *testing.T, c *holdrecord.Client, holdID string) {
		t.Helper()
		hold(t, c, holdID, holdrecord.WorkflowBucketsCreate, nil)
		run(t, c, holdID, cmd("buckets create"), nil, nil)
		release(t, c, holdID)
	}
	liveHold := func(t *testing.T, c *holdrecord.Client, holdID string) holdrecord.Row {
		t.Helper()
		held := hold(t, c, holdID, holdrecord.WorkflowBucketsCreate, nil)
		if _, err := c.Admit(ctx, holdID, cmd("buckets create"), nil); err != nil {
			t.Fatal(err)
		}
		return held
	}
	// refusedEvery asserts that a released restore naming each of names, sent by a new process,
	// is refused 400 bad_transition naming live and the hand repair, never the held restore, and
	// that no row is stored.
	refusedEvery := func(t *testing.T, r rig, live string, names ...string) {
		t.Helper()
		for _, name := range names {
			fresh := r.client(t)
			got := restoreRefusal(t, fresh, restoreGen(t, fresh), natsgrant.OperatorPhaseReleased, name)
			if !refusedWith(got, http.StatusBadRequest, "bad_transition", "hold "+live+"'s journal", handRepair) || strings.Contains(got.Message, "restore the row held") {
				t.Errorf("a released restore naming %q answered %s; want 400 bad_transition naming %s's journal and the hand repair, not a held restore", name, refusalText(got), live)
			}
			if g, phase, holdID := stored(t, r); g != 0 {
				p, known := r.HoldView(false).State(ctx)
				t.Fatalf("after the restore naming %q the table holds %s/%s at gen %d (view %q, known %v) while %s's journal shows it held; want no row",
					name, phase, holdID, g, p, known, live)
			}
		}
	}

	t.Run("B: a point-in-time restore, then a later history's prepare put back mid-hold: every released restore is refused", func(t *testing.T) {
		// Claude's round-8 probe B. Fails while a later gen's prepare of another hold ends the live
		// hold (endedInLog's later-prepare rule): released is stored naming h-k2.
		r := newRig(t)
		c := r.client(t)
		endedHold(t, c, "h-a")
		tables := []string{"nats_hold", "nats_hold_log", "nats_hold_records", "nats_hold_record_deletes", "nats_cutover"}
		for _, table := range tables {
			exec(t, r, `create table snap_`+table+` as select * from `+table)
		}
		restorePoint := row(t, c).Gen
		endedHold(t, c, "h-k1")
		endedHold(t, c, "h-k2")
		exec(t, r, `create table history1_log as select * from nats_hold_log where gen > $1`, int64(restorePoint))
		for _, table := range tables {
			exec(t, r, `delete from `+table)
			exec(t, r, `insert into `+table+` select * from snap_`+table)
		}
		r.Restart()
		held := liveHold(t, r.client(t), "h-h")
		var k2Gen int64
		if err := r.Pool.QueryRow(ctx, `select gen from history1_log where hold_id = 'h-k2' and phase = 'prepare'`).Scan(&k2Gen); err != nil {
			t.Fatal(err)
		}
		if k2Gen <= int64(held.Gen) {
			t.Fatalf("history 1's prepare of h-k2 is at gen %d, not above history 2's held gen %d", k2Gen, held.Gen)
		}
		exec(t, r, `insert into nats_hold_log select * from history1_log where gen = $1`, k2Gen)
		exec(t, r, `delete from nats_hold`)
		r.Restart()
		refusedEvery(t, r, "h-h", "", "h-k2")
	})

	t.Run("B2: a genuine prepare signed an hour earlier, at a higher gen, put back mid-hold: the released restore is refused", func(t *testing.T) {
		r := newRig(t)
		c := r.client(t)
		held := liveHold(t, c, "h-h2")
		forkGen := held.Gen + 2
		at := time.Now().Add(-time.Hour).UTC().Truncate(time.Microsecond)
		sig, err := signedrecord.Sign(r.SigningKey, signedrecord.HoldRow(forkGen, "prepare", "h-fork", at))
		if err != nil {
			t.Fatal(err)
		}
		exec(t, r, `insert into nats_hold_log (gen, phase, hold_id, reason, written_at, written_by, signed_at, sig) values ($1, 'prepare', 'h-fork', 'fork', $2, 'op', $2, $3)`, int64(forkGen), at, sig)
		exec(t, r, `delete from nats_hold`)
		r.Restart()
		refusedEvery(t, r, "h-h2", "h-fork", "")
	})

	t.Run("B3: a log cut back and restored, then a saved later prepare put back mid-hold: the released restore is refused", func(t *testing.T) {
		// The writer cuts the log back to gen 4 and deletes h-k1's and h-k2's journals with it, as
		// a cut consistent with the log does: a journal left naming rows the cut log no longer
		// holds would refuse the first restore (journalBindingRows).
		r := newRig(t)
		c := r.client(t)
		for _, id := range []string{"h-a", "h-k1", "h-k2"} {
			endedHold(t, c, id)
		}
		exec(t, r, `create table saved_log as select * from nats_hold_log`)
		exec(t, r, `delete from nats_hold_log where gen > 4`)
		exec(t, r, `delete from nats_hold_records where hold_id in ('h-k1', 'h-k2')`)
		exec(t, r, `delete from nats_hold`)
		r.Restart()
		op := r.client(t)
		gen := restoreGen(t, op)
		if got, err := op.RestoreRow(ctx, gen, natsgrant.OperatorPhaseReleased, "", "the lost row"); err != nil {
			t.Fatalf("the first restore at gen %d answered %+v, %v", gen, got, err)
		}
		held := liveHold(t, op, "h-h")
		var k2Gen int64
		if err := r.Pool.QueryRow(ctx, `select gen from saved_log where hold_id = 'h-k2' and phase = 'prepare'`).Scan(&k2Gen); err != nil {
			t.Fatal(err)
		}
		if k2Gen <= int64(held.Gen) {
			t.Fatalf("the saved prepare of h-k2 is at gen %d, not above h-h's held gen %d", k2Gen, held.Gen)
		}
		exec(t, r, `insert into nats_hold_log select * from saved_log where gen = $1`, k2Gen)
		exec(t, r, `delete from nats_hold`)
		r.Restart()
		refusedEvery(t, r, "h-h", "h-k2", "")
	})

	t.Run("B4: B3 with the final restore on the replica that never restarted: the released restore is refused", func(t *testing.T) {
		r := newRig(t)
		c := r.client(t)
		endedHold(t, c, "h-a")
		saved := savedHoldRow(t, r)
		endedHold(t, c, "h-k1")
		endedHold(t, c, "h-y")
		r.UseReplica(true)
		if got := row(t, r.client(t)); got.Gen != 10 {
			t.Fatalf("the second replica read gen %d, want 10", got.Gen)
		}
		r.UseReplica(false)
		exec(t, r, `create table saved_log as select * from nats_hold_log`)
		putHoldRowBack(t, r, saved)
		exec(t, r, `delete from nats_hold_log where gen > 4`)
		r.Restart()
		if held := liveHold(t, r.client(t), "h-h"); held.Gen != 6 {
			t.Fatalf("h-h is held at gen %d, want 6", held.Gen)
		}
		exec(t, r, `insert into nats_hold_log select * from saved_log where gen in (8, 10)`)
		exec(t, r, `delete from nats_hold`)
		r.UseReplica(true)
		defer r.UseReplica(false)
		refusedEvery(t, r, "h-h", "", "h-y")
	})

	t.Run("a released restore mid-hold, with the log intact, advises the held restore, which is stored", func(t *testing.T) {
		// The log's latest entry is the live hold's own held entry, so the route admits the held
		// restore, and the refusal says so. Fails while the refusal names the hand repair here.
		r := newRig(t)
		c := r.client(t)
		liveHold(t, c, "h-intact")
		exec(t, r, `delete from nats_hold`)
		fresh := r.client(t)
		gen := restoreGen(t, fresh)
		got := restoreRefusal(t, fresh, gen, natsgrant.OperatorPhaseReleased, "")
		if !refusedWith(got, http.StatusBadRequest, "bad_transition", "restore the row held with its hold id h-intact") || strings.Contains(got.Message, handRepair) {
			t.Errorf("the released restore answered %s; want 400 bad_transition advising the held restore of h-intact", refusalText(got))
		}
		if restored, err := fresh.RestoreRow(ctx, gen, natsgrant.OperatorPhaseHeld, "h-intact", "the advised restore"); err != nil || restored.Phase != natsgrant.OperatorPhaseHeld {
			t.Errorf("the advised held restore answered %+v, %v; want it stored", restored, err)
		}
	})

	t.Run("one unsigned journal under an unused hold id blocks every released restore until it is deleted by name", func(t *testing.T) {
		// TN deep's R8-4, the database-writer residual: no route can tell a writer's record from a
		// live hold's broken one, so the refusal names the record and the hand repair. Fails while
		// it names neither, as at c23c8c1b87.
		r := newRig(t)
		c := r.client(t)
		endedHold(t, c, "A")
		exec(t, r, `insert into nats_hold_records (hold_id, kind, key, value, gen, written_at, sig) values ('ghost', 'done', 'hold', '{}'::jsonb, 1, now(), 'garbage')`)
		exec(t, r, `delete from nats_hold`)
		for _, restart := range []bool{false, true} {
			if restart {
				r.Restart()
			}
			for _, name := range []string{"", "A"} {
				fresh := r.client(t)
				got := restoreRefusal(t, fresh, restoreGen(t, fresh), natsgrant.OperatorPhaseReleased, name)
				if !refusedWith(got, http.StatusConflict, "below_floor", `hold "ghost"'s done record`, handRepair) || got.HoldID != "" {
					t.Errorf("restart %v: a released restore naming %q answered %s, hold id %q; want 409 below_floor naming ghost's done record and the hand repair, and no hold id", restart, name, refusalText(got), holdIDOf(got))
				}
			}
		}
		if g, phase, holdID := stored(t, r); g != 0 {
			t.Fatalf("before the repair the table holds %s/%s at gen %d, want no row", phase, holdID, g)
		}
		exec(t, r, `delete from nats_hold_records where hold_id = 'ghost' and kind = 'done'`)
		fresh := r.client(t)
		gen := restoreGen(t, fresh)
		if restored, err := fresh.RestoreRow(ctx, gen, natsgrant.OperatorPhaseReleased, "", "after the hand repair"); err != nil || restored.Gen != gen {
			t.Errorf("after the hand repair the released restore at gen %d answered %+v, %v; want it stored", gen, restored, err)
		}
	})
}
