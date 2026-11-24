package store

import (
	"context"
	"reflect"
	"testing"
)

// appliedSeqVersion is the version of the migration this file tests, so the test can migrate
// through everything before it whatever lands beside it.
const appliedSeqVersion = 20261121170714

// appliedSeqRelease names the release whose way back the migration's header carries, as a
// failure to read that block names it.
const appliedSeqRelease = "the release that keeps the sequence of the newest applied issue fact in the copy of Dispatch's issues"

// 20261121170714 adds dispatch_issues.applied_seq, the sequence of the newest issue fact the
// daemon applied, at its default, 0, on every row the copy holds, and the release before's
// insert, which names none of it, still lands at 0. Its way back, the last paragraph of the
// migration's header (migrations.WayBack), drops the column and deletes the migration's version
// row, so the roll-forward's Migrate applies the file again and adds the column again at 0.
func TestTheAppliedSequenceComesAtZeroAndItsWayBackLetsItComeAgain(t *testing.T) {
	ctx := context.Background()
	store := emptyStore(t)
	migrateThrough(t, store, appliedSeqVersion-1)
	exec := func(statement string) {
		t.Helper()
		if _, err := store.Pool().Exec(ctx, statement); err != nil {
			t.Fatalf("%s: %v", statement, err)
		}
	}
	// The release before's insert names these columns and no other (record's PutDispatchIssue).
	const releaseBeforesInsert = `insert into dispatch_issues (key, project, dispatch_seq, status, status_seq, parent, parent_seq,
		blockers, blockers_seq, labels, labels_seq, rank, rank_seq) values `
	exec(releaseBeforesInsert + `('ACME-1', 'ACME', 5, 'todo', 5, null, 5, '{}', 5, '{legion}', 5, 'a0', 5)`)
	if _, err := store.Migrate(ctx); err != nil {
		t.Fatalf("migrate through the applied sequence: %v", err)
	}
	const applied = `select row(key, dispatch_seq, applied_seq)::text from dispatch_issues order by key`
	if got, want := rowsAsText(t, store, applied), []string{"(ACME-1,5,0)"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("the copy after the migration (key, dispatch_seq, applied_seq) = %v, want %v: the row it found at 0", got, want)
	}
	exec(releaseBeforesInsert + `('ACME-2', 'ACME', 7, 'todo', 7, null, 7, '{}', 7, '{}', 7, '', 7)`)
	if got, want := rowsAsText(t, store, applied), []string{"(ACME-1,5,0)", "(ACME-2,7,0)"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("the copy after the release before's insert = %v, want %v: its row at 0", got, want)
	}
	exec(`update dispatch_issues set applied_seq = 5 where key = 'ACME-1'`)

	exec(wayBackOf(t, appliedSeqVersion, appliedSeqRelease))
	const column = `select count(*)::text from information_schema.columns where table_name = 'dispatch_issues' and column_name = 'applied_seq'`
	const version = `select count(*)::text from schema_version where version = 20261121170714`
	if got := append(rowsAsText(t, store, column), rowsAsText(t, store, version)...); !reflect.DeepEqual(got, []string{"0", "0"}) {
		t.Fatalf("after the way back the column and the version row = %v, want neither", got)
	}
	if _, err := store.Migrate(ctx); err != nil {
		t.Fatalf("roll forward after the way back: %v", err)
	}
	if got, want := rowsAsText(t, store, applied), []string{"(ACME-1,5,0)", "(ACME-2,7,0)"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("the copy after the roll-forward = %v, want %v: the column again, at 0 on every row", got, want)
	}
}
