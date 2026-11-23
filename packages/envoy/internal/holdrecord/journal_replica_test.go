package holdrecord_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"maps"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/bus/signedrecord"
	"github.com/sjawhar/envoy/internal/holdrecord"
	"github.com/sjawhar/envoy/natsgrant"
)

// replicaName names one of the rig's two replicas in a failure.
func replicaName(second bool) string {
	if second {
		return "the second replica"
	}
	return "the first replica"
}

// replicaRows are TestTheHoldJournal's rows of two broker replicas on one database
// (holdrecordtest's UseReplica), each with a records floor of its own. A cohort the routes delete
// reads as deleted on both, since the delete the store keeps is the operator's signed one, and a
// cohort gone with no delete that verifies reads as unknown on each replica that saw it.
func replicaRows(t *testing.T) {
	ctx := context.Background()

	// prepared is a rig whose row is prepare under holdID, with its cohort written on the first
	// replica, writes times, and then read on both, so each has a floor for it at its last gen.
	prepared := func(t *testing.T, holdID string, writes int) rig {
		t.Helper()
		r := newRig(t)
		c := r.client(t)
		current := row(t, c)
		if _, err := c.MoveRow(ctx, current.Gen, natsgrant.OperatorPhasePrepare, holdID, "prepare"); err != nil {
			t.Fatal(err)
		}
		for gen := range writes {
			if _, err := c.PutRecord(ctx, holdID, "cohort", "cohort", uint64(gen), json.RawMessage(`{"raised":["a"]}`)); err != nil {
				t.Fatal(err)
			}
		}
		for _, second := range []bool{false, true} {
			r.UseReplica(second)
			if records, err := c.Records(ctx, holdID, "cohort"); err != nil || len(records) != 1 {
				t.Fatalf("%s reads the cohort: %d records, %v", replicaName(second), len(records), err)
			}
		}
		r.UseReplica(false)
		return r
	}

	t.Run("a cohort the routes delete on one replica reads as deleted on the other, and the abort's rerun there answers nil", func(t *testing.T) {
		// Fails while the delete is kept only in the floor of the replica that served it: the
		// second replica, which saw the cohort, reads its absence as a database writer's delete
		// (ErrUnknown, below_floor), and so fails the abort's rerun.
		r := prepared(t, "h-replicas", 1)
		c := r.client(t)
		var value, sig string
		var gen int64
		if err := r.Pool.QueryRow(ctx, `select value::text, gen, sig from nats_hold_records where hold_id = 'h-replicas' and kind = 'cohort'`).Scan(&value, &gen, &sig); err != nil {
			t.Fatal(err)
		}
		if err := c.DeleteCohort(ctx, "h-replicas"); err != nil {
			t.Fatalf("DeleteCohort on the first replica: %v", err)
		}
		for _, second := range []bool{false, true} {
			r.UseReplica(second)
			if records, err := c.Records(ctx, "h-replicas", "cohort"); err != nil || len(records) != 0 {
				t.Fatalf("%s reads the deleted cohort: %d records, %v; want none and no error", replicaName(second), len(records), err)
			}
			if err := c.DeleteCohort(ctx, "h-replicas"); err != nil {
				t.Fatalf("the abort's rerun of DeleteCohort on %s: %v", replicaName(second), err)
			}
		}
		// A database writer puts the deleted cohort back: on each replica it is a deleted record
		// put back, which reads as unknown.
		if _, err := r.Pool.Exec(ctx, `insert into nats_hold_records (hold_id, kind, key, value, gen, written_at, sig)
			values ('h-replicas', 'cohort', 'cohort', $1::jsonb, $2, now(), $3)`, value, gen, sig); err != nil {
			t.Fatal(err)
		}
		for _, second := range []bool{false, true} {
			r.UseReplica(second)
			if _, err := c.Records(ctx, "h-replicas", "cohort"); !errors.Is(err, holdrecord.ErrUnknown) {
				t.Fatalf("%s reads the deleted cohort put back as %v, want ErrUnknown", replicaName(second), err)
			}
		}
	})

	t.Run("a cohort a database writer deletes, with no signed delete, reads as unknown on each replica that saw it", func(t *testing.T) {
		r := prepared(t, "h-sql", 1)
		c := r.client(t)
		if _, err := r.Pool.Exec(ctx, `delete from nats_hold_records where hold_id = 'h-sql' and kind = 'cohort'`); err != nil {
			t.Fatal(err)
		}
		for _, second := range []bool{false, true} {
			r.UseReplica(second)
			if _, err := c.Records(ctx, "h-sql", "cohort"); !errors.Is(err, holdrecord.ErrUnknown) {
				t.Fatalf("%s reads the cohort deleted outside the routes as %v, want ErrUnknown", replicaName(second), err)
			}
			if err := c.DeleteCohort(ctx, "h-sql"); !errors.Is(err, holdrecord.ErrUnknown) {
				t.Fatalf("DeleteCohort on %s, with the cohort deleted outside the routes, answered %v, want ErrUnknown", replicaName(second), err)
			}
		}
	})

	t.Run("a delete the store holds that does not verify, or that names an earlier gen, deletes nothing a replica saw", func(t *testing.T) {
		// A database writer deletes the cohort, read at gen 2 on both replicas, and writes a delete
		// row beside it. Each row here must leave the absence a database writer's: one that
		// counted a delete without verifying it, or one signed over gen 1, fails.
		_, foreign, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		for _, x := range []struct {
			name     string
			gen      uint64             // the delete row's gen
			foreign  ed25519.PrivateKey // its signer when not the operator
			operator bool               // signed by the operator's key; with no foreign key either, no signature
			hold     string             // the hold its signature names; "" for the cohort's own
		}{
			{name: "signed by a key the broker does not hold", gen: 2, foreign: foreign},
			{name: "with no signature", gen: 2},
			{name: "signed by the operator over another hold's cohort", gen: 2, operator: true, hold: "h-other"},
			{name: "signed by the operator over gen 1, below the gen both replicas read", gen: 1, operator: true},
		} {
			r := prepared(t, "h-forged-delete", 2)
			c := r.client(t)
			signer, hold := x.foreign, x.hold
			if x.operator {
				signer = r.SigningKey
			}
			if hold == "" {
				hold = "h-forged-delete"
			}
			sig := ""
			if signer != nil {
				if sig, err = signedrecord.Sign(signer, signedrecord.RecordDelete(hold, "cohort", "cohort", x.gen)); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := r.Pool.Exec(ctx, `with gone as (delete from nats_hold_records where hold_id = 'h-forged-delete' and kind = 'cohort')
				insert into nats_hold_record_deletes (hold_id, kind, key, gen, deleted_at, sig) values ('h-forged-delete', 'cohort', 'cohort', $1, now(), $2)`,
				int64(x.gen), sig); err != nil {
				t.Fatal(err)
			}
			for _, second := range []bool{false, true} {
				r.UseReplica(second)
				if _, err := c.Records(ctx, "h-forged-delete", "cohort"); !errors.Is(err, holdrecord.ErrUnknown) {
					t.Errorf("a delete row %s: %s reads the cohort as %v, want ErrUnknown", x.name, replicaName(second), err)
				}
			}
		}
	})

	t.Run("a cohort the routes deleted is not put back, on a replica that never read it", func(t *testing.T) {
		// The second replica's floor never saw the cohort, so only the signed delete the store keeps
		// stands against a put of its key at gen 0. Fails while the store inserts a deleted key again:
		// the cohort is back at gen 1.
		r := newRig(t)
		c := r.client(t)
		const holdID = "h-cohort-again"
		current := row(t, c)
		if _, err := c.MoveRow(ctx, current.Gen, natsgrant.OperatorPhasePrepare, holdID, "prepare"); err != nil {
			t.Fatal(err)
		}
		value := json.RawMessage(`{"raised":["a"]}`)
		if _, err := c.PutRecord(ctx, holdID, "cohort", "cohort", 0, value); err != nil {
			t.Fatal(err)
		}
		if err := c.DeleteCohort(ctx, holdID); err != nil {
			t.Fatal(err)
		}
		r.UseReplica(true)
		defer r.UseReplica(false)
		var refusal *holdrecord.RouteError
		if got, err := c.PutRecord(ctx, holdID, "cohort", "cohort", 0, value); !errors.As(err, &refusal) || refusal.Code != "gen_mismatch" {
			t.Errorf("PutRecord of the deleted cohort on the second replica answered %+v, %v; want 409 gen_mismatch", got, err)
		}
		var n int
		if err := r.Pool.QueryRow(ctx, `select count(*) from nats_hold_records where hold_id = $1 and kind = 'cohort'`, holdID).Scan(&n); err != nil || n != 0 {
			t.Errorf("after the refused put the table holds %d cohort records (%v), want 0", n, err)
		}
	})
}

// savedHoldRow is the hold row's every column as the table holds it, for a database writer to put
// back (putHoldRowBack).
func savedHoldRow(t *testing.T, r rig) []any {
	t.Helper()
	var gen int64
	var phase, holdID, reason, updatedBy, sig string
	var updatedAt, signedAt time.Time
	if err := r.Pool.QueryRow(context.Background(), `select gen, phase, hold_id, reason, updated_at, updated_by, signed_at, sig from nats_hold`).
		Scan(&gen, &phase, &holdID, &reason, &updatedAt, &updatedBy, &signedAt, &sig); err != nil {
		t.Fatal(err)
	}
	return []any{gen, phase, holdID, reason, updatedAt, updatedBy, signedAt, sig}
}

// putHoldRowBack writes saved over the hold row, as a database writer other than the operator can.
func putHoldRowBack(t *testing.T, r rig, saved []any) {
	t.Helper()
	if _, err := r.Pool.Exec(context.Background(), `update nats_hold set gen = $1, phase = $2, hold_id = $3, reason = $4, updated_at = $5, updated_by = $6, signed_at = $7, sig = $8`, saved...); err != nil {
		t.Fatal(err)
	}
}

// rowFloorRows are TestTheHoldJournal's rows of the routes' floor of the hold row: a row a
// database writer puts back below the gen the broker served or wrote, or another verified row at
// that gen, reads as unknown, no move is made from it, and no refusal hands it out.
func rowFloorRows(t *testing.T) {
	ctx := context.Background()

	// replayedPrepare is the reviewer's replay: prepare at gen 2, held at gen 3 with hold set done
	// and a command's intent pending, both written through the routes; then a database writer puts
	// the signed prepare row back and deletes log row 3. It returns the rig, its client and the
	// prepare row.
	replayedPrepare := func(t *testing.T, holdID string) (rig, *holdrecord.Client, holdrecord.Row) {
		t.Helper()
		r := newRig(t)
		c := r.client(t)
		current := row(t, c)
		prepared, err := c.MoveRow(ctx, current.Gen, natsgrant.OperatorPhasePrepare, holdID, "prepare")
		if err != nil {
			t.Fatal(err)
		}
		saved := savedHoldRow(t, r)
		if err := c.StartJournal(ctx, holdID, holdrecord.WorkflowCutover, nil); err != nil {
			t.Fatal(err)
		}
		if _, err := c.Admit(ctx, holdID, cmd(holdrecord.CommandSet), nil); err != nil {
			t.Fatal(err)
		}
		held, err := c.MoveRow(ctx, prepared.Gen, natsgrant.OperatorPhaseHeld, holdID, "held")
		if err != nil {
			t.Fatal(err)
		}
		if err := c.AppendDone(ctx, holdID, holdrecord.CommandSet, nil); err != nil {
			t.Fatal(err)
		}
		if _, err := c.Admit(ctx, holdID, cmd("create-bucket"), nil); err != nil {
			t.Fatal(err)
		}
		putHoldRowBack(t, r, saved)
		if _, err := r.Pool.Exec(ctx, `delete from nats_hold_log where gen = $1`, int64(held.Gen)); err != nil {
			t.Fatal(err)
		}
		return r, c, prepared
	}

	t.Run("a prepare row put back reads as unknown on a replica whose hold view read the held row, and no release is moved from it", func(t *testing.T) {
		// The reviewer's case: the second replica's routes never served the held row, but its auth
		// service's hold view read it, and one floor serves both. Fails while the routes and the view
		// keep floors of their own: the routes take the put-back prepare row, MoveRow(2, released)
		// stores released at gen 3, and the view, which read held at gen 3, then reads unknown.
		r := newRig(t)
		c := r.client(t)
		current := row(t, c)
		prepared, err := c.MoveRow(ctx, current.Gen, natsgrant.OperatorPhasePrepare, "h-shared-floor", "prepare")
		if err != nil {
			t.Fatal(err)
		}
		saved := savedHoldRow(t, r)
		if err := c.StartJournal(ctx, "h-shared-floor", holdrecord.WorkflowCutover, nil); err != nil {
			t.Fatal(err)
		}
		if _, err := c.Admit(ctx, "h-shared-floor", cmd(holdrecord.CommandSet), nil); err != nil {
			t.Fatal(err)
		}
		held, err := c.MoveRow(ctx, prepared.Gen, natsgrant.OperatorPhaseHeld, "h-shared-floor", "held")
		if err != nil {
			t.Fatal(err)
		}
		view := r.HoldView(true)
		if phase, known := view.State(ctx); !known || phase != natsgrant.OperatorPhaseHeld {
			t.Fatalf("the second replica's view read %q, known %v; want held", phase, known)
		}
		putHoldRowBack(t, r, saved)
		if _, err := r.Pool.Exec(ctx, `delete from nats_hold_log where gen = $1`, int64(held.Gen)); err != nil {
			t.Fatal(err)
		}
		r.UseReplica(true)
		defer r.UseReplica(false)
		if moved, err := c.MoveRow(ctx, prepared.Gen, natsgrant.OperatorPhaseReleased, "h-shared-floor", "abort"); err == nil {
			phase, known := view.State(ctx)
			t.Errorf("MoveRow(%d, released) on the second replica stored %+v, and its view then reads %q, known %v; want a refusal", prepared.Gen, moved, phase, known)
		}
		var storedGen int64
		var storedPhase string
		if err := r.Pool.QueryRow(ctx, `select gen, phase from nats_hold`).Scan(&storedGen, &storedPhase); err != nil || storedPhase != "prepare" || storedGen != int64(prepared.Gen) {
			t.Errorf("after the move the row is %s at gen %d (%v), want the put-back prepare at gen %d", storedPhase, storedGen, err, prepared.Gen)
		}
	})

	t.Run("a move from a row put back below the log, on a replica that served only that row, is 409 below_floor", func(t *testing.T) {
		// TN's round-4 case: the second replica served the prepare row alone, so its floor takes
		// the prepare row put back after the move to held, and the move from it would store gen 3,
		// which the log already holds. Fails while that collision answers 503 DATABASE_UNAVAILABLE.
		r := newRig(t)
		c := r.client(t)
		current := row(t, c)
		prepared, err := c.MoveRow(ctx, current.Gen, natsgrant.OperatorPhasePrepare, "h-logged", "prepare")
		if err != nil {
			t.Fatal(err)
		}
		r.UseReplica(true)
		if _, err := c.Row(ctx); err != nil {
			t.Fatal(err)
		}
		r.UseReplica(false)
		saved := savedHoldRow(t, r)
		if _, err := c.MoveRow(ctx, prepared.Gen, natsgrant.OperatorPhaseHeld, "h-logged", "held"); err != nil {
			t.Fatal(err)
		}
		putHoldRowBack(t, r, saved)
		r.UseReplica(true)
		defer r.UseReplica(false)
		var refusal *holdrecord.RouteError
		moved, err := c.MoveRow(ctx, prepared.Gen, natsgrant.OperatorPhaseReleased, "h-logged", "release from the put-back row")
		if !errors.Is(err, holdrecord.ErrUnknown) || !errors.As(err, &refusal) || refusal.Status != http.StatusConflict || refusal.Code != "below_floor" {
			t.Errorf("MoveRow(%d, released) on the second replica answered %+v, %v; want 409 below_floor, read as ErrUnknown", prepared.Gen, moved, err)
		}
		var storedGen int64
		var storedPhase string
		if err := r.Pool.QueryRow(ctx, `select gen, phase from nats_hold`).Scan(&storedGen, &storedPhase); err != nil || storedPhase != "prepare" || storedGen != int64(prepared.Gen) {
			t.Errorf("after the refused move the row is %s at gen %d (%v), want the put-back prepare at gen %d", storedPhase, storedGen, err, prepared.Gen)
		}
	})

	t.Run("a prepare row put back mid-hold reads as unknown, and no release is moved from it", func(t *testing.T) {
		// Fails while the routes keep no floor for the row: Row reads prepare at gen 2, and
		// MoveRow(2, released) stores released at gen 3, freeing every client mid-hold.
		r, c, prepared := replayedPrepare(t, "h-replayed")
		// Each step reports and the next still runs, so the control shows where the move lands.
		if got, err := c.Row(ctx); !errors.Is(err, holdrecord.ErrUnknown) {
			t.Errorf("Row with the prepare row put back answered %+v, %v; want ErrUnknown", got, err)
		}
		var refusal *holdrecord.RouteError
		if moved, err := c.MoveRow(ctx, prepared.Gen, natsgrant.OperatorPhaseReleased, "h-replayed", "abort"); !errors.As(err, &refusal) || refusal.Code != "gen_mismatch" {
			t.Errorf("MoveRow(%d, released) from the put-back prepare row answered %+v, %v; want 409 gen_mismatch", prepared.Gen, moved, err)
		}
		var storedGen int64
		var storedPhase string
		if err := r.Pool.QueryRow(ctx, `select gen, phase from nats_hold`).Scan(&storedGen, &storedPhase); err != nil || storedPhase != "prepare" || storedGen != int64(prepared.Gen) {
			t.Errorf("after the refused move the row is %s at gen %d (%v), want the put-back prepare at gen %d", storedPhase, storedGen, err, prepared.Gen)
		}
	})

	t.Run("a records write refused against a prepare row put back carries no row, and reads as unknown", func(t *testing.T) {
		// After the replay, a records PUT the store refuses 409 carries the row it read in current.
		// Fails while that row skips the floor: the body hands out the put-back prepare row, which
		// verifies, as the row the write was refused against.
		r, c, _ := replayedPrepare(t, "h-replayed-current")
		var body string
		r.once(recordPut, func(w http.ResponseWriter, req *http.Request) bool {
			answer := httptest.NewRecorder()
			r.Routes().ServeHTTP(answer, req)
			body = answer.Body.String()
			maps.Copy(w.Header(), answer.Header())
			w.WriteHeader(answer.Code)
			_, _ = w.Write(answer.Body.Bytes())
			return true
		})
		_, err := c.PutRecord(ctx, "h-replayed-current", "selection", "inputs", 5, json.RawMessage(`{"a":1}`))
		var refusal *holdrecord.RouteError
		if strings.Contains(body, `"current"`) || !errors.Is(err, holdrecord.ErrUnknown) || !errors.As(err, &refusal) || refusal.Code != "below_floor" || refusal.Row != nil {
			t.Errorf("a records PUT at a gen the record does not hold answered %v, body %s; want 409 below_floor with no current row, read as ErrUnknown", err, body)
		}
	})

	t.Run("another verified row at the gen the broker served reads as unknown", func(t *testing.T) {
		// A database writer holding a released row signed at the held row's gen (the second effect
		// of the replay above) swaps it in. Fails while the floor keeps no digest of the row.
		r := newRig(t)
		c := r.client(t)
		held := hold(t, c, "h-swapped", holdrecord.WorkflowCutover, nil)
		if _, err := c.Row(ctx); err != nil {
			t.Fatal(err)
		}
		signedAt := time.Now().UTC().Truncate(time.Microsecond)
		sig, err := signedrecord.Sign(r.SigningKey, signedrecord.HoldRow(held.Gen, string(natsgrant.OperatorPhaseReleased), "h-swapped", signedAt))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := r.Pool.Exec(ctx, `update nats_hold set phase = 'released', signed_at = $1, sig = $2`, signedAt, sig); err != nil {
			t.Fatal(err)
		}
		if got, err := c.Row(ctx); !errors.Is(err, holdrecord.ErrUnknown) {
			t.Fatalf("Row with a released row swapped in at gen %d answered %+v, %v; want ErrUnknown", held.Gen, got, err)
		}
		// A move from the swapped row is a move from a fork: 409 below_floor, read as unknown,
		// not gen_mismatch, which would send the operator to read the same row again.
		var refusal *holdrecord.RouteError
		if moved, err := c.MoveRow(ctx, held.Gen, natsgrant.OperatorPhaseReleased, "h-swapped", "release from the swapped row"); !errors.Is(err, holdrecord.ErrUnknown) ||
			!errors.As(err, &refusal) || refusal.Code != "below_floor" {
			t.Errorf("MoveRow(%d, released) from the swapped row answered %+v, %v; want 409 below_floor, read as ErrUnknown", held.Gen, moved, err)
		}
	})
}
