package store

import (
	"context"
	"testing"
)

// deliveryResumeVersion is the version of the migration this file tests, so the test can migrate
// through everything before it whatever lands beside it.
const deliveryResumeVersion = 20261112215432

// 20261112215432 gives a wait a changes request answered before it the version that request
// reviewed (issues.awaiting_answered_version), read once from the detail every earlier release
// wrote for it, with the answer's moment beside it (awaiting_answered_document_at, the fence of the
// answered document), so the resume asks of a number and the wait reads as it did. A wait answered
// any other way, and one still unanswered, takes no version. It adds the columns the document the
// answering act was of and the step whose row held it are recorded in (awaiting_answered_artifact,
// awaiting_answered_step), and gives no wait either: an answer before it records neither. The
// negative control is the same rows read before the migration, which hold no such column.
func TestAWaitAChangesRequestAnsweredTakesItsVersionIn20261112215432(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := emptyStore(t)
	migrateThrough(t, store, deliveryResumeVersion-1)
	exec := func(statement string) {
		t.Helper()
		if _, err := store.Pool().Exec(ctx, statement); err != nil {
			t.Fatalf("seed %q: %v", statement, err)
		}
	}
	for _, row := range []struct{ key, answer string }{
		{"LEGION-1", `now(), 'changes_requested', 'version 13: "split it"'`},
		{"LEGION-2", `now(), 'approved', 'version 4'`},
		{"LEGION-3", `null, null, null`},
	} {
		exec(`insert into issues (key, tree, project, title, phase, generation, status, rank, last_dispatch_seq,
			awaiting_ask, awaiting_since_seq, awaiting_since, awaiting_answered_at, awaiting_answered_by, awaiting_answered_detail)
			values ('` + row.key + `', '` + row.key + `', 'LEGION', 'a waiting root', 'awaiting_person', 1, 'in_progress', 'A', 4,
			'ask-q', 11, now(), ` + row.answer + `)`)
	}
	var columns int
	if err := store.Pool().QueryRow(ctx, `select count(*) from information_schema.columns
		where table_name = 'issues' and column_name in ('awaiting_answered_version', 'awaiting_answered_document_at',
			'awaiting_answered_artifact', 'awaiting_answered_step')`).Scan(&columns); err != nil {
		t.Fatalf("read the answer's columns before the migration: %v", err)
	}
	if columns != 0 {
		t.Fatalf("issues holds %d of the answer's columns before the migration, want none", columns)
	}

	if _, err := store.Migrate(ctx); err != nil {
		t.Fatalf("migrate through 20261112215432: %v", err)
	}

	for _, want := range []struct {
		key     string
		version *int
	}{
		{"LEGION-1", new(13)},
		{"LEGION-2", nil},
		{"LEGION-3", nil},
	} {
		var version *int
		var document, step *string
		var beside bool
		if err := store.Pool().QueryRow(ctx, `select awaiting_answered_version, awaiting_answered_artifact, awaiting_answered_step,
			coalesce(awaiting_answered_document_at = awaiting_answered_at, false) from issues where key = $1`, want.key).Scan(&version, &document, &step, &beside); err != nil {
			t.Fatalf("read the version of %s: %v", want.key, err)
		}
		if document != nil {
			t.Errorf("%s holds the answered document %q after the migration, want none: no answer before it recorded one", want.key, *document)
		}
		if step != nil {
			t.Errorf("%s holds the answered step %q after the migration, want none: no answer before it recorded one", want.key, *step)
		}
		switch {
		case want.version == nil && version != nil:
			t.Errorf("%s holds version %d after the migration, want none: no changes request answered its wait", want.key, *version)
		case want.version != nil && (version == nil || *version != *want.version || !beside):
			t.Errorf("%s holds version %v (beside its answer: %v) after the migration, want %d beside the answer it was read from",
				want.key, version, beside, *want.version)
		}
	}
}
