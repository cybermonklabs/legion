package store

import (
	"context"
	"reflect"
	"testing"
)

// phaseReturnToVersion is the version of the migration this file tests, so the test can migrate
// through everything before it whatever lands beside it.
const phaseReturnToVersion = 20261126110302

// phaseReturnToRelease names the release whose way back the migration's header carries, as a
// failure to read that block names it.
const phaseReturnToRelease = "the release that keeps the step a tester's completion names until the check states decide"

// 20261126110302 adds phases.return_to, on the tester's row the step its completion names for its
// task to return to when the check states fail the testing step, at its default, 0, on every row
// the release before wrote, and the release before's insert, which names none of it, still lands
// at 0. Its way back, the last paragraph of the migration's header (migrations.WayBack), drops the
// column and deletes the migration's version row, so the roll-forward's Migrate applies the file
// again, and only it, and adds the column again at 0.
func TestThePhaseReturnToComesAtZeroAndItsWayBackLetsItComeAgain(t *testing.T) {
	ctx := context.Background()
	store := emptyStore(t)
	migrateThrough(t, store, phaseReturnToVersion-1)
	exec := func(statement string) {
		t.Helper()
		if _, err := store.Pool().Exec(ctx, statement); err != nil {
			t.Fatalf("%s: %v", statement, err)
		}
	}
	exec(`insert into issues (key, tree, project, title, phase, generation, status, rank, last_dispatch_seq)
		values ('ACME-1', 'ACME-1', 'ACME', 'root', 'testing', 1, 'testing', 'A', 3)`)
	// The release before's write of a phase row names no return_to (record's PutPhase).
	const releaseBeforesInsert = `insert into phases (issue, role, claim, handoff_commit, handoff_record, handoff_version, rounds, states_serial, summary) values `
	exec(releaseBeforesInsert + `('ACME-1', 'tester', 'tester-claim', 'head', 'test-1', 1, 0, 4, 'tested')`)
	if _, err := store.Migrate(ctx); err != nil {
		t.Fatalf("migrate through the phase's return_to: %v", err)
	}
	const kept = `select row(role, states_serial, return_to)::text from phases order by role`
	if got, want := rowsAsText(t, store, kept), []string{"(tester,4,0)"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("the phase rows after the migration (role, states_serial, return_to) = %v, want %v: the row it found at 0", got, want)
	}
	exec(releaseBeforesInsert + `('ACME-1', 'reviewer', 'reviewer-claim', '', '', 0, 0, 0, '')`)
	if got, want := rowsAsText(t, store, kept), []string{"(reviewer,0,0)", "(tester,4,0)"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("the phase rows after the release before's insert = %v, want %v: its row at 0", got, want)
	}
	exec(`update phases set return_to = 2 where role = 'tester'`)
	if _, err := store.Pool().Exec(ctx, `update phases set return_to = -1 where role = 'reviewer'`); err == nil {
		t.Fatalf("return_to took -1, want the column's check to refuse a place below 0")
	}

	exec(wayBackOf(t, phaseReturnToVersion, phaseReturnToRelease))
	const column = `select count(*)::text from information_schema.columns where table_schema = current_schema()
		and table_name = 'phases' and column_name = 'return_to'`
	const version = `select count(*)::text from schema_version where version = 20261126110302`
	if got := append(rowsAsText(t, store, column), rowsAsText(t, store, version)...); !reflect.DeepEqual(got, []string{"0", "0"}) {
		t.Fatalf("after the way back the column and the version row = %v, want neither", got)
	}
	applied, err := store.Migrate(ctx)
	if err != nil || applied != 1 {
		t.Fatalf("the roll-forward's migrate applied %d, %v; want the phase's return_to alone", applied, err)
	}
	if got, want := rowsAsText(t, store, kept), []string{"(reviewer,0,0)", "(tester,4,0)"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("the phase rows after the roll-forward = %v, want %v: the column again, at 0 on every row", got, want)
	}
}
