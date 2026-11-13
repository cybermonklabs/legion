package store

import (
	"context"
	"strings"
	"testing"
)

// phaseEntryMovedBackVersion is the version of the migration this file tests, so the test can
// migrate through everything before it whatever lands beside it.
const phaseEntryMovedBackVersion = 20261113090512

// 20261113090512 adds phases.entry_moved_back, whether the start that raised a role's entry was a
// move back, which the completion route reads to refuse a behind completion whatever head it
// names. No row written before it says which move raised its entry, and the release before it
// refused every behind completion, so every row the upgrade finds reads true: a role on an entry a
// forward move raised and a role no entry has been raised on alike. A row inserted afterwards by a
// write that does not name the column — a rolled-back release's own insert — reads false, as a
// forward move's.
//
// The negative control is the write of the column before the migration, which the database
// refuses as a column it does not know.
func TestEveryPhasesRowTheUpgradeFindsReadsMovedBackAndALaterInsertReadsForward(t *testing.T) {
	ctx := context.Background()
	store := emptyStore(t)
	migrateThrough(t, store, phaseEntryMovedBackVersion-1)

	for _, seed := range []string{
		`insert into issues (key, tree, project, title, phase, generation, status, rank, last_dispatch_seq)
			values ('LEGION-208', 'LEGION-208', 'LEGION', 'root', 'testing', 1, 'in_progress', 'A', 1)`,
		// The tester on its second testing entry, which the release before raised without saying
		// which move it was.
		`insert into phases (issue, role, claim, handoff_commit, rounds, verdict, entry, entry_task, entry_raised_at)
			values ('LEGION-208', 'tester', 'legion-LEGION-208-tester', '', 0, '', 2, 'Test the change.', now())`,
		// The reviewer, whose phase the issue has not reached: no entry raised on it at all.
		`insert into phases (issue, role, claim, handoff_commit, rounds, verdict)
			values ('LEGION-208', 'reviewer', 'legion-LEGION-208-reviewer', '', 0, '')`,
	} {
		if _, err := store.Pool().Exec(ctx, seed); err != nil {
			t.Fatalf("record before the migration: %v", err)
		}
	}
	_, err := store.Pool().Exec(ctx, "update phases set entry_moved_back = false")
	if err == nil || !strings.Contains(err.Error(), "entry_moved_back") {
		t.Fatalf("writing entry_moved_back before the migration = %v, want the column to be unknown", err)
	}

	if _, err := store.Migrate(ctx); err != nil {
		t.Fatalf("migrate through the phase entry's move back: %v", err)
	}

	movedBack := func(role string) bool {
		t.Helper()
		var held bool
		if err := store.Pool().QueryRow(ctx,
			"select entry_moved_back from phases where issue = 'LEGION-208' and role = $1", role,
		).Scan(&held); err != nil {
			t.Fatalf("read the %s's phases row after the migration: %v", role, err)
		}
		return held
	}
	for _, role := range []string{"tester", "reviewer"} {
		if !movedBack(role) {
			t.Errorf("the %s's phases row the upgrade found reads entry_moved_back false, want true: no row before the column said which move raised its entry, "+
				"and only true refuses a behind completion at the recorded head as the release before did", role)
		}
	}

	// An insert that names no such column, as the release before writes its own rows.
	if _, err := store.Pool().Exec(ctx, `insert into phases (issue, role, claim, handoff_commit, rounds, verdict, entry, entry_task, entry_raised_at)
		values ('LEGION-208', 'implementer', 'legion-LEGION-208-implementer', '', 0, '', 1, 'Implement the change.', now())`); err != nil {
		t.Fatalf("insert a phases row that does not name entry_moved_back: %v", err)
	}
	if movedBack("implementer") {
		t.Errorf("a phases row inserted after the migration without naming entry_moved_back reads true, want false: " +
			"true is for the rows the upgrade found, and an insert that does not name the column reads as a forward move's")
	}
}
