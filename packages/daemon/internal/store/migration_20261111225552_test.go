package store

import (
	"context"
	"strings"
	"testing"
	"time"
)

// phaseEntryVersion is the version of the migration this file tests, so the test can migrate
// through everything before it whatever lands beside it.
const phaseEntryVersion = 20261111225552

// 20261111225552 puts the phase entry where the assignment stood, and takes the assignment away:
// phases.entry and phases.entry_task hold the number of the work a role is owed and that work,
// claims.given_entry and claims.served_entry hold what the claim was queued and what its worker
// confirmed, and pending_task_deliveries.entry carries the number from the start row to the
// claim. The three columns the token lived in — phases.assignment, claims.serving_assignment and
// pending_task_deliveries.assignment — are dropped by the migration's last three statements, so
// a reader of this record finds one model and not two.
//
// The negative control is the daemon's own write before the migration, which names the entry and
// is refused, and the three reads of the dropped columns afterwards, each of which the database
// answers that the column does not exist.
//
// The fill is read on rows the release before this one left: a phases row holding an assignment
// is a role that was handed work, so it is at entry 1, and the work of that entry is the task of
// the start row the issue still owes it; the claim whose serving_assignment is that same token
// took that work, so it was given it and served it; and the delivery carrying the token is that
// work in flight.
//
// Three things the entry does not replace are read back after the migration, because the rules
// that hold the fleet together are weighed against them: the outbox write mark, which admit's
// stopOutlivedStart compares with the mark a retirement stamps on the claim; the start row's
// needsSlot flag, which the admission cap and the waiting line are read from (record.OwedStart);
// and claims.delivered_rows, which holds a person's recorded decision to the claim that was told
// it and guards a delivery that carries no entry against being queued twice.
func TestThePhaseEntryColumnsReplaceTheAssignmentColumnsWith20261111225552(t *testing.T) {
	ctx := context.Background()
	store := emptyStore(t)
	migrateThrough(t, store, phaseEntryVersion-1)

	const owedTask = "Implement the change. Issue: LEGION-208. Phase: implementing."
	for _, seed := range []string{
		`insert into issues (key, tree, project, title, phase, generation, status, rank, last_dispatch_seq)
			values ('LEGION-208', 'LEGION-208', 'LEGION', 'root', 'implementing', 1, 'in_progress', 'A', 1)`,
		`insert into phases (issue, role, claim, handoff_commit, rounds, verdict, assignment)
			values ('LEGION-208', 'implementer', 'legion-LEGION-208-implementer', '', 0, '', 'A1')`,
		`insert into claims (token, project, tree, issue, role, generation, session, session_file, state,
			launch_failures, prompt_failures, prompt_retires, uncertain_streak, serving_generation, serving_assignment)
			values ('legion-LEGION-208-implementer', 'legion', 'LEGION-208', 'LEGION-208', 'implementer', 1, 'ses_i', '',
				'working', 0, 0, 0, 0, 1, 'A1')`,
		`insert into pending_task_deliveries (claim_token, delivery_id, task, phase, generation, queued_at, interrupted, assignment)
			values ('legion-LEGION-208-implementer', 'outbox:7', 'Implement the change.', 'implementing', 1, now(), false, 'A1')`,
	} {
		if _, err := store.Pool().Exec(ctx, seed); err != nil {
			t.Fatalf("record before the migration: %v", err)
		}
	}
	// The start the issue still owes its implementer: the row the waiting line orders the issue
	// by and the row the fill reads entry 1's work from. It carries the token the implementer's
	// phases row names, as every start that release wrote carried the token of the entry it
	// handed (its workflow started, through StartOrOwe), because the fill matches the row by it.
	var owedRow, owedMark int64
	if err := store.Pool().QueryRow(ctx, `insert into outbox (kind, issue, payload, attempts, next_at, last_error)
		values ('supervise', 'LEGION-208', $1, 0, now(), '') returning id, write_mark`,
		`{"op":"start","tree":"LEGION-208","role":"implementer","phase":"implementing","generation":1,"assignment":"A1","needsSlot":true,"task":"`+owedTask+`"}`,
	).Scan(&owedRow, &owedMark); err != nil {
		t.Fatalf("enqueue the start LEGION-208 owes its implementer: %v", err)
	}
	if _, err := store.Pool().Exec(ctx, `update claims set delivered_rows = array[$1::bigint]
		where token = 'legion-LEGION-208-implementer'`, owedRow); err != nil {
		t.Fatalf("record the row the claim queued a task from: %v", err)
	}

	// The write the daemon makes once a transition raises an entry, against the schema before the
	// migration: the row the migration exists for.
	_, err := store.Pool().Exec(ctx, "update phases set entry = 1 where issue = 'LEGION-208' and role = 'implementer'")
	if err == nil || !strings.Contains(err.Error(), "entry") {
		t.Fatalf("raising a phases row's entry before the migration = %v, want the column to be unknown", err)
	}
	t.Logf("before the migration the entry write is refused: %v", err)
	if _, err := store.Migrate(ctx); err != nil {
		t.Fatalf("migrate through the phase entry: %v", err)
	}

	for _, gone := range []struct {
		what  string
		query string
	}{
		{"the phases row's assignment", "select assignment from phases where issue = 'LEGION-208' and role = 'implementer'"},
		{"the claim's serving assignment", "select serving_assignment from claims where token = 'legion-LEGION-208-implementer'"},
		{"the pending delivery's assignment", "select assignment from pending_task_deliveries where delivery_id = 'outbox:7'"},
	} {
		var held string
		err := store.Pool().QueryRow(ctx, gone.query).Scan(&held)
		if err == nil || !strings.Contains(err.Error(), "does not exist") {
			t.Fatalf("reading %s after the migration = %q, %v; want the column gone: the entry replaced it and no reader finds two models",
				gone.what, held, err)
		}
	}

	var entry int64
	var entryTask string
	var raisedAt *time.Time
	if err := store.Pool().QueryRow(ctx, `select entry, entry_task, entry_raised_at from phases
		where issue = 'LEGION-208' and role = 'implementer'`).Scan(&entry, &entryTask, &raisedAt); err != nil {
		t.Fatalf("read the implementer's phases row after the migration: %v", err)
	}
	if entry != 1 || entryTask != owedTask || raisedAt == nil {
		t.Fatalf("the implementer's phases row = entry %d raised at %v with task %q, want entry 1 raised at the migration's own moment, "+
			"carrying the task of the start the issue owes it: a role the release before this one handed work is at entry 1", entry, raisedAt, entryTask)
	}
	var given, served int64
	if err := store.Pool().QueryRow(ctx, `select given_entry, served_entry from claims
		where token = 'legion-LEGION-208-implementer'`).Scan(&given, &served); err != nil {
		t.Fatalf("read the implementer's claim after the migration: %v", err)
	}
	if given != 1 || served != 1 {
		t.Fatalf("the claim was given entry %d and served entry %d, want 1 and 1: its worker took the work its phases row names", given, served)
	}
	var delivered int64
	if err := store.Pool().QueryRow(ctx, `select entry from pending_task_deliveries where delivery_id = 'outbox:7'`).Scan(&delivered); err != nil {
		t.Fatalf("read the pending delivery after the migration: %v", err)
	}
	if delivered != 1 {
		t.Fatalf("the delivery in flight carries entry %d, want 1: the retirement that records what its worker took needs an entry to record", delivered)
	}

	var mark int64
	var needsSlot string
	if err := store.Pool().QueryRow(ctx, `select write_mark, payload->>'needsSlot' from outbox where id = $1`,
		owedRow).Scan(&mark, &needsSlot); err != nil {
		t.Fatalf("read the owed start after the migration: %v", err)
	}
	if mark != owedMark {
		t.Fatalf("the owed start's write mark = %d, want the %d it took: admit's stopOutlivedStart weighs it against the mark a retirement stamps on the claim",
			mark, owedMark)
	}
	if needsSlot != "true" {
		t.Fatalf("the owed start's needsSlot flag = %q, want \"true\": the admission cap and the waiting line are read from it", needsSlot)
	}
	var rows []int64
	if err := store.Pool().QueryRow(ctx, `select delivered_rows from claims where token = 'legion-LEGION-208-implementer'`).Scan(&rows); err != nil {
		t.Fatalf("read the rows the claim queued a task from after the migration: %v", err)
	}
	if len(rows) != 1 || rows[0] != owedRow {
		t.Fatalf("the claim records rows %v as queued, want the one row %d: a person's recorded decision is held to the claim by them, "+
			"and a delivery that carries no entry is guarded against a second queueing by them", rows, owedRow)
	}
}
