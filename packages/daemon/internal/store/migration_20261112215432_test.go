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
// wrote for it, with the answer's moment beside it (awaiting_answered_version_at), so the resume
// asks of a number and the wait reads as it did. A wait answered any other way, and one still
// unanswered, takes no version. The negative control is the same rows read before the migration,
// which hold no such column.
func TestAWaitAChangesRequestAnsweredTakesItsVersionIn20261112215432(t *testing.T) {
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
		where table_name = 'issues' and column_name in ('awaiting_answered_version', 'awaiting_answered_version_at')`).Scan(&columns); err != nil {
		t.Fatalf("read the version columns before the migration: %v", err)
	}
	if columns != 0 {
		t.Fatalf("issues holds %d version column(s) before the migration, want none", columns)
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
		var beside bool
		if err := store.Pool().QueryRow(ctx, `select awaiting_answered_version,
			coalesce(awaiting_answered_version_at = awaiting_answered_at, false) from issues where key = $1`, want.key).Scan(&version, &beside); err != nil {
			t.Fatalf("read the version of %s: %v", want.key, err)
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
