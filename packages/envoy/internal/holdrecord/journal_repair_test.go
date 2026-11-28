package holdrecord_test

//ci:reads packages/envoy/infra/README.md readmeSays, which TestTheHoldJournal's repairRows calls, reads it through ../../infra/README.md

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/bus/signedrecord"
	"github.com/sjawhar/envoy/internal/holdrecord"
	"github.com/sjawhar/envoy/natsgrant"
)

// keyRead is infra/README.md's read of the rows a key signed ("Rotating the hold signing key",
// step 3), with <old> the key's id, and keyDeletes its hand repair's deletes by name (step 4), for
// each table the statements to run in order for one row the read lists, each placeholder a value
// it lists: a log entry's hold's journal goes before the entry, since a journal names the log
// entries its entries were written under. logDelete is the delete of one log entry by gen that
// the repair of an entry put back from another history runs alone. readmeSays fails the test
// unless the README holds each word for word, so a row that follows them follows the README as
// written.
const keyRead = `select 'nats_hold' as tbl, gen::text as row from nats_hold where split_part(sig, '.', 1) in ('<old>', sig)
   union all select 'nats_hold_log', gen::text from nats_hold_log where split_part(sig, '.', 1) in ('<old>', sig)
   union all select 'nats_hold_records', hold_id || ' ' || kind || ' ' || key from nats_hold_records where split_part(sig, '.', 1) in ('<old>', sig)
   union all select 'nats_hold_record_deletes', hold_id || ' ' || kind || ' ' || key from nats_hold_record_deletes where split_part(sig, '.', 1) in ('<old>', sig)
   union all select 'nats_cutover', family from nats_cutover where split_part(sig, '.', 1) in ('<old>', sig);`

const logDelete = "delete from nats_hold_log where gen = <gen>;"

var keyDeletes = map[string][]string{
	"nats_hold": {"delete from nats_hold where gen = <gen>;"},
	"nats_hold_log": {"delete from nats_hold_records where kind = 'done' and hold_id in (select hold_id from nats_hold_log where gen = <gen>);",
		logDelete},
	"nats_hold_records":        {"delete from nats_hold_records where hold_id = '<hold>' and kind = '<kind>' and key = '<key>';"},
	"nats_hold_record_deletes": {"delete from nats_hold_record_deletes where hold_id = '<hold>' and kind = '<kind>' and key = '<key>';"},
	"nats_cutover":             {"delete from nats_cutover where family = '<family>';"},
}

// readKeyRows runs keyRead with kid, answering each row it lists as its table and name.
func readKeyRows(t *testing.T, r rig, kid string) [][2]string {
	t.Helper()
	rows, err := r.Pool.Query(context.Background(), strings.ReplaceAll(keyRead, "<old>", kid))
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var listed [][2]string
	for rows.Next() {
		var table, name string
		if err := rows.Scan(&table, &name); err != nil {
			t.Fatal(err)
		}
		listed = append(listed, [2]string{table, name})
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return listed
}

// runKeyDeletes runs step 4's deletes, as keyDeletes writes them, for each listed row keep takes,
// and answers the tables the read listed.
func runKeyDeletes(t *testing.T, r rig, listed [][2]string, keep func(table, name string) bool) map[string]bool {
	t.Helper()
	tables := map[string]bool{}
	for _, row := range listed {
		tables[row[0]] = true
		if !keep(row[0], row[1]) {
			continue
		}
		fields := strings.Fields(row[1])
		for _, statement := range keyDeletes[row[0]] {
			switch row[0] {
			case "nats_hold", "nats_hold_log":
				statement = strings.ReplaceAll(statement, "<gen>", row[1])
			case "nats_cutover":
				statement = strings.ReplaceAll(statement, "<family>", row[1])
			default:
				statement = strings.NewReplacer("<hold>", fields[0], "<kind>", fields[1], "<key>", fields[2]).Replace(statement)
			}
			if _, err := r.Pool.Exec(context.Background(), statement); err != nil {
				t.Fatalf("%s: %v", statement, err)
			}
		}
	}
	return tables
}

// readmeSays fails t unless infra/README.md holds text word for word, with every run of spaces and
// line breaks read as one space, so a sentence the README wraps still matches.
func readmeSays(t *testing.T, text string) {
	t.Helper()
	readme, err := os.ReadFile("../../infra/README.md")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(strings.Fields(string(readme)), " "), strings.Join(strings.Fields(text), " ")) {
		t.Fatalf("infra/README.md does not hold, word for word:\n%s", text)
	}
}

// repairRows are TestTheHoldJournal's rows of a restore after a database writer's change that
// only a hand repair clears, each repair followed as infra/README.md writes it: a released entry
// of the live hold from another history (B5), which no longer ends it; an entry from another
// history put back above a live hold, which the refusal names, with its repair; a live hold's
// journal deleted; and a leaked signing key's repair.
func repairRows(t *testing.T) {
	ctx := context.Background()
	exec := func(t *testing.T, r rig, sql string, args ...any) {
		t.Helper()
		if _, err := r.Pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
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
	// putBack inserts a log entry the operator's key signed at gen, an hour before now: an entry
	// of another history of the tables, put back by a database writer.
	putBack := func(t *testing.T, r rig, gen uint64, phase natsgrant.OperatorPhase, holdID string) {
		t.Helper()
		at := time.Now().Add(-time.Hour).UTC().Truncate(time.Microsecond)
		sig, err := signedrecord.Sign(r.SigningKey, signedrecord.HoldRow(gen, string(phase), holdID, at))
		if err != nil {
			t.Fatal(err)
		}
		exec(t, r, `insert into nats_hold_log (gen, phase, hold_id, reason, written_at, written_by, signed_at, sig) values ($1, $2, $3, 'another history', $4, 'op', $4, $5)`,
			int64(gen), string(phase), holdID, at, sig)
	}
	liveHold := func(t *testing.T, c *holdrecord.Client, holdID string) holdrecord.Row {
		t.Helper()
		held := hold(t, c, holdID, holdrecord.WorkflowBucketsCreate, nil)
		if _, err := c.Admit(ctx, holdID, cmd("buckets create"), nil); err != nil {
			t.Fatal(err)
		}
		return held
	}

	t.Run("B5: the live hold's own released entry from another history, put back mid-hold: the released restore is refused", func(t *testing.T) {
		// Claude's round-9 probe B5: a point-in-time restore taken while h-live was prepared, whose
		// first history released it an hour before the second history held it again. The put-back
		// released entry names h-live, but its journal shows buckets create's intent pending, so
		// that entry did not end it. Fails while a verified released entry ends a hold before its
		// journal is read, as at e481b26500: released is stored, and both replicas read it.
		r := newRig(t)
		c := r.client(t)
		held := liveHold(t, c, "h-live")
		putBack(t, r, held.Gen+1, natsgrant.OperatorPhaseReleased, "h-live")
		exec(t, r, `delete from nats_hold`)
		r.Restart()
		fresh := r.client(t)
		got := restoreRefusal(t, fresh, restoreGen(t, fresh), natsgrant.OperatorPhaseReleased, "")
		latest := fmt.Sprintf("released at gen %d with hold id %q", held.Gen+1, "h-live")
		if !refusedWith(got, http.StatusBadRequest, "bad_transition", "hold h-live's journal", "buckets create's intent pending", latest, "an entry from another history put back above a live hold", handRepair) {
			t.Errorf("the released restore answered %s; want 400 bad_transition naming h-live's journal, its pending intent, the latest entry (%s) and the hand repair", refusalText(got), latest)
		}
		if gen, phase := stored(t, r); gen != 0 {
			p, known := r.HoldView(true).State(ctx)
			t.Errorf("the table holds %s at gen %d while h-live's journal shows buckets create's intent pending; the replica that never restarted reads %q, known %v", phase, gen, p, known)
		}
	})

	t.Run("an entry from another history put back above a live hold: the refusal names it; deleted by gen and the replica restarted, the held restore stores", func(t *testing.T) {
		// Claude's round-9 probe REPAIR, B2's shape, followed by infra/README.md's repair. Fails
		// while the refusal names only the other two causes, as at e481b26500, which leaves the
		// operator no way to find the entry to delete.
		readmeSays(t, logDelete)
		r := newRig(t)
		c := r.client(t)
		held := liveHold(t, c, "h-live")
		forkGen := held.Gen + 2
		putBack(t, r, forkGen, natsgrant.OperatorPhasePrepare, "h-other")
		exec(t, r, `delete from nats_hold`)
		r.Restart()
		fresh := r.client(t)
		got := restoreRefusal(t, fresh, restoreGen(t, fresh), natsgrant.OperatorPhaseReleased, "")
		latest := fmt.Sprintf("prepare at gen %d with hold id %q", forkGen, "h-other")
		if !refusedWith(got, http.StatusBadRequest, "bad_transition", "hold h-live's journal", latest, "an entry from another history put back above a live hold", handRepair) {
			t.Fatalf("the released restore answered %s; want 400 bad_transition naming the put-back entry (%s) and the hand repair", refusalText(got), latest)
		}
		exec(t, r, strings.ReplaceAll(logDelete, "<gen>", fmt.Sprint(forkGen)))
		_, err := r.client(t).RestoreRow(ctx, held.Gen+1, natsgrant.OperatorPhaseHeld, "h-live", "the put-back entry deleted")
		var refusal *holdrecord.RouteError
		if !errors.As(err, &refusal) || refusal.Code != "below_floor" {
			t.Errorf("after the delete, before a restart, the held restore answered %v; want 409 below_floor, since the refused restore took gen %d into this replica's floor", err, forkGen)
		}
		r.Restart()
		if restored, err := r.client(t).RestoreRow(ctx, held.Gen+1, natsgrant.OperatorPhaseHeld, "h-live", "after the restart"); err != nil || restored.Phase != natsgrant.OperatorPhaseHeld {
			t.Errorf("after the delete and a restart, the held restore answered %+v, %v; want it stored", restored, err)
		}
	})

	t.Run("a live hold's journal deleted: the released restore names the missing record and the hand repair, not a held restore the client refuses", func(t *testing.T) {
		// Claude's round-9 probe F7. Fails while the refusal advises restoring the row held, as at
		// e481b26500: RestoreRow of held reads the journal first and refuses a hold with none.
		r := newRig(t)
		c := r.client(t)
		hold(t, c, "h-live", holdrecord.WorkflowBucketsCreate, nil)
		exec(t, r, `delete from nats_hold_records where hold_id = 'h-live' and kind = 'done'`)
		exec(t, r, `delete from nats_hold`)
		r.Restart()
		fresh := r.client(t)
		gen := restoreGen(t, fresh)
		got := restoreRefusal(t, fresh, gen, natsgrant.OperatorPhaseReleased, "")
		if !refusedWith(got, http.StatusBadRequest, "bad_transition", `hold id "h-live"`, "no done record", handRepair) || strings.Contains(got.Message, "restore the row held") {
			t.Errorf("the released restore answered %s; want 400 bad_transition naming h-live's missing done record and the hand repair, not a held restore", refusalText(got))
		}
		if _, err := r.client(t).RestoreRow(ctx, gen, natsgrant.OperatorPhaseHeld, "h-live", "the held restore"); err == nil || !strings.Contains(err.Error(), "no done record") {
			t.Errorf("the held restore of h-live answered %v; want the client's refusal of a hold with no done record", err)
		}
		if g, phase := stored(t, r); g != 0 {
			t.Errorf("the table holds %s at gen %d, want no row", phase, g)
		}
	})

	t.Run("a leaked key's hand repair, followed as written, leaves the view known and a restore stored", func(t *testing.T) {
		// Claude's round-9 probe LEAK, TN deep's Minor 1. Every row was signed by a key that then
		// leaked, the released row and a cutover row included. Fails with the repair's deletes
		// limited to journals and log entries, as e481b26500's README wrote it: the leaked key's
		// row stays, the view reads unknown and the restore answers row_exists.
		readmeSays(t, keyRead)
		for _, statements := range keyDeletes {
			for _, statement := range statements {
				readmeSays(t, statement)
			}
		}
		r := newRig(t)
		c := r.client(t)
		held := hold(t, c, "h-1", holdrecord.WorkflowBucketsCreate, nil)
		run(t, c, "h-1", cmd("buckets create"), nil, nil)
		release(t, c, "h-1")
		leaked, leakedKey, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		signedBy(t, r, leakedKey)
		cutover, err := signedrecord.Sign(leakedKey, signedrecord.Cutover("session", "done", "h-1", held.Gen))
		if err != nil {
			t.Fatal(err)
		}
		exec(t, r, `insert into nats_cutover (family, state, hold_id, hold_gen, written_at, written_by, sig) values ('session', 'done', 'h-1', $1, now(), 'op', $2)`, int64(held.Gen), cutover)
		r.RestartListing(leaked)
		listed := readKeyRows(t, r, signedrecord.KeyID(leaked))
		tables := runKeyDeletes(t, r, listed, leakRepairDeletes)
		if !tables["nats_hold"] || !tables["nats_cutover"] || !tables["nats_hold_log"] || !tables["nats_hold_records"] {
			t.Fatalf("the read by the leaked id listed %v; want the hold row, log entries, records and the cutover row", listed)
		}
		r.RestartListing() // the leaked key removed, and the replica restarted
		readmeSays(t, restoreWhenListed)
		fresh := r.client(t)
		err = fresh.Bootstrap(ctx)
		var mismatch *holdrecord.RestoreGenError
		if errors.As(err, &mismatch) {
			_, err = fresh.RestoreRow(ctx, mismatch.RestoreGen, natsgrant.OperatorPhaseReleased, "", "after the leaked key's repair")
		}
		if err != nil {
			t.Errorf("after the repair, the restore answered %v; want it stored", err)
		}
		if p, known := r.HoldView(false).State(ctx); !known || p != natsgrant.OperatorPhaseReleased {
			t.Errorf("after the repair the auth service's view reads %q, known %v; want released, known", p, known)
		}
		if _, err := fresh.Cutovers(ctx); err != nil {
			t.Errorf("after the repair Cutovers answered %v; want every family's row read", err)
		}
	})
}

// leakRepairDeletes reports whether the leaked key's hand repair deletes the row of table named
// name that the read lists: every row of the five tables, the hold row and the cutover rows
// included.
func leakRepairDeletes(table, name string) bool { return len(keyDeletes[table]) > 0 && name != "" }

// restoreWhenListed is infra/README.md's step-4 rule for the lost row's restore, which a row
// follows: the restore runs only when the read listed the hold row.
const restoreWhenListed = "restore the row, when the read listed the hold row"

// signedBy re-signs the hold row, every log entry and every record with key: rows that key signed.
func signedBy(t *testing.T, r rig, key ed25519.PrivateKey) {
	t.Helper()
	ctx := context.Background()
	type hold struct {
		gen           int64
		phase, holdID string
		signedAt      time.Time
	}
	read := func(sql string) []hold {
		t.Helper()
		rows, err := r.Pool.Query(ctx, sql)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var out []hold
		for rows.Next() {
			var h hold
			if err := rows.Scan(&h.gen, &h.phase, &h.holdID, &h.signedAt); err != nil {
				t.Fatal(err)
			}
			out = append(out, h)
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		return out
	}
	resign := func(h hold) string {
		t.Helper()
		sig, err := signedrecord.Sign(key, signedrecord.HoldRow(uint64(h.gen), h.phase, h.holdID, h.signedAt))
		if err != nil {
			t.Fatal(err)
		}
		return sig
	}
	for _, e := range read(`select gen, phase, hold_id, signed_at from nats_hold_log`) {
		if _, err := r.Pool.Exec(ctx, `update nats_hold_log set sig = $2 where gen = $1`, e.gen, resign(e)); err != nil {
			t.Fatal(err)
		}
	}
	for _, h := range read(`select gen, phase, hold_id, signed_at from nats_hold`) {
		if _, err := r.Pool.Exec(ctx, `update nats_hold set sig = $1`, resign(h)); err != nil {
			t.Fatal(err)
		}
	}
	rows, err := r.Pool.Query(ctx, `select hold_id, kind, key, value::text, gen from nats_hold_records`)
	if err != nil {
		t.Fatal(err)
	}
	type record struct {
		holdID, kind, key, value string
		gen                      int64
	}
	var records []record
	for rows.Next() {
		var rec record
		if err := rows.Scan(&rec.holdID, &rec.kind, &rec.key, &rec.value, &rec.gen); err != nil {
			t.Fatal(err)
		}
		records = append(records, rec)
	}
	rows.Close()
	for _, rec := range records {
		sig, err := signedrecord.Sign(key, signedrecord.Record(rec.holdID, rec.kind, rec.key, uint64(rec.gen), []byte(rec.value)))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := r.Pool.Exec(ctx, `update nats_hold_records set sig = $4 where hold_id = $1 and kind = $2 and key = $3`, rec.holdID, rec.kind, rec.key, sig); err != nil {
			t.Fatal(err)
		}
	}
}
