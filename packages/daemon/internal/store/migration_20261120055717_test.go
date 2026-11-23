package store

import (
	"context"
	"reflect"
	"testing"
)

// valueSequencesVersion is the version of the migration this file tests, so the test can migrate
// through everything before it whatever lands beside it.
const valueSequencesVersion = 20261120055717

// valueSequencesRelease names the release whose way back the migration's header carries, as a
// failure to read that block names it.
const valueSequencesRelease = "the release that fences each value of the copy of Dispatch's issues"

// copyRowSequences is one row of the copy of Dispatch's issues as this test reads it: the
// sequence of the newest reading the row took (dispatch_seq), the sequence of each value, and the
// two values no release before this one wrote.
type copyRowSequences struct {
	DispatchSeq, StatusSeq, ParentSeq, BlockersSeq int64
	Labels                                         []string
	Rank                                           string
	LabelsSeq, RankSeq                             int64
}

// 20261120055717 gives each value of the copy of Dispatch's issues (dispatch_issues) the sequence
// of the reading that wrote it, adds labels and rank, and marks each project the copy holds rows
// of, so the first boot of that project on this release empties those rows and fills them again
// from its listing. A row the release before left took all its values at its dispatch_seq, which
// is what that release's rule said of it, so its status, parent and blockers each take that
// sequence; its labels and rank, which no reading wrote, are empty at 0. A row of no project gets
// no mark. The release before names none of the new columns, and its insert still lands, each new
// column at its default.
//
// The way back, the last paragraph of the migration's header (migrations.WayBack), marks each
// project again, so its next boot on this release refills it: the mark a boot stamped reads
// unstamped again, and a mark no boot stamped stays as it was.
func TestTheCopysRefillMarksComeFromTheMigrationAndTheWayBack(t *testing.T) {
	ctx := context.Background()
	store := emptyStore(t)
	migrateThrough(t, store, valueSequencesVersion-1)
	exec := func(statement string) {
		t.Helper()
		if _, err := store.Pool().Exec(ctx, statement); err != nil {
			t.Fatalf("%s: %v", statement, err)
		}
	}
	// The release before's insert names these columns and no other (record's PutDispatchIssue).
	const releaseBeforesInsert = `insert into dispatch_issues (key, project, status, blockers, dispatch_seq, parent) values `
	exec(releaseBeforesInsert + `('ACME-1', 'ACME', 'in_progress', '{ACME-2}', 5, 'ACME-9'),
		('ACME-2', 'ACME', 'todo', '{}', 7, null),
		('OTHER-1', 'OTHER', 'in_progress', '{}', 9, null),
		('LOST-1', '', 'todo', '{}', 3, null)`)
	if _, err := store.Migrate(ctx); err != nil {
		t.Fatalf("migrate through the value sequences: %v", err)
	}

	const marks = `select row(project, refilled_at is null)::text from dispatch_issue_refills order by project`
	if got, want := rowsAsText(t, store, marks), []string{"(ACME,t)", "(OTHER,t)"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("the refill marks after the migration (project, unstamped) = %v, want %v: one unstamped mark for each project with rows, none for the row of no project",
			got, want)
	}
	read := func() map[string]copyRowSequences {
		t.Helper()
		rows, err := store.Pool().Query(ctx, `select key, dispatch_seq, status_seq, parent_seq, blockers_seq, labels, rank,
			labels_seq, rank_seq from dispatch_issues`)
		if err != nil {
			t.Fatalf("read the copy's sequences: %v", err)
		}
		defer rows.Close()
		got := map[string]copyRowSequences{}
		for rows.Next() {
			var key string
			var row copyRowSequences
			if err := rows.Scan(&key, &row.DispatchSeq, &row.StatusSeq, &row.ParentSeq, &row.BlockersSeq, &row.Labels, &row.Rank,
				&row.LabelsSeq, &row.RankSeq); err != nil {
				t.Fatalf("read the copy's sequences: %v", err)
			}
			got[key] = row
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("read the copy's sequences: %v", err)
		}
		return got
	}
	migrated := map[string]copyRowSequences{
		"ACME-1":  {DispatchSeq: 5, StatusSeq: 5, ParentSeq: 5, BlockersSeq: 5, Labels: []string{}},
		"ACME-2":  {DispatchSeq: 7, StatusSeq: 7, ParentSeq: 7, BlockersSeq: 7, Labels: []string{}},
		"OTHER-1": {DispatchSeq: 9, StatusSeq: 9, ParentSeq: 9, BlockersSeq: 9, Labels: []string{}},
		"LOST-1":  {DispatchSeq: 3, StatusSeq: 3, ParentSeq: 3, BlockersSeq: 3, Labels: []string{}},
	}
	if got := read(); !reflect.DeepEqual(got, migrated) {
		t.Fatalf("the copy after the migration = %+v, want %+v: status, parent and blockers at each row's dispatch_seq, labels empty and rank '' at 0",
			got, migrated)
	}

	exec(releaseBeforesInsert + `('ACME-3', 'ACME', 'todo', '{}', 11, 'ACME-1')`)
	if got, want := read()["ACME-3"], (copyRowSequences{DispatchSeq: 11, Labels: []string{}}); !reflect.DeepEqual(got, want) {
		t.Fatalf("the release before's row after the migration = %+v, want %+v: every value it wrote at sequence 0, labels empty and rank ''",
			got, want)
	}
	t.Logf("the migration marked ACME and OTHER, set each row's sequences to its dispatch_seq, and took the release before's insert")

	exec(`update dispatch_issue_refills set refilled_at = now() where project = 'ACME'`)
	if got, want := rowsAsText(t, store, marks), []string{"(ACME,f)", "(OTHER,t)"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("the refill marks after a boot stamped ACME's = %v, want %v", got, want)
	}
	const otherMark = `select row(project, marked_at, refilled_at)::text from dispatch_issue_refills where project = 'OTHER'`
	before := rowsAsText(t, store, otherMark)
	exec(wayBackOf(t, valueSequencesVersion, valueSequencesRelease))
	if got, want := rowsAsText(t, store, marks), []string{"(ACME,t)", "(OTHER,t)"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("the refill marks after the way back = %v, want %v: ACME's stamp cleared, so its next boot refills it", got, want)
	}
	if after := rowsAsText(t, store, otherMark); !reflect.DeepEqual(after, before) {
		t.Fatalf("OTHER's mark after the way back = %v, want it as it was: %v", after, before)
	}
}
