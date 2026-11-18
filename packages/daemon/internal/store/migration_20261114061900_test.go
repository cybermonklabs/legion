package store

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// phaseEntryVerbatimVersion is the version of the migration this file tests, so the test can
// migrate through everything before it whatever lands beside it.
const phaseEntryVerbatimVersion = 20261114061900

// 20261114061900 adds the mark that the sweep's send of a root architect's entry again sends the
// entry's task as written (record.PhaseRow.EntryVerbatim). Every row the table holds before it
// reads back unmarked, which that send derives again: here the architect row of a root the
// architect upgrade settled at entry 1 with no task, and a phase worker's row of the same root,
// whose task is sent as written whatever the mark says. A mark written after it reads back as it
// was written, through a store reopened as a daemon restart reopens it.
func TestThePhaseEntryVerbatimMigrationLeavesEveryRowUnmarkedAndTheMarkRoundTrips(t *testing.T) {
	ctx := context.Background()
	store := emptyStore(t)
	migrateThrough(t, store, phaseEntryVerbatimVersion-1)
	for _, seed := range []string{
		`insert into issues (key, tree, project, title, phase, generation, status, rank, last_dispatch_seq)
			values ('LEGION-208', 'LEGION-208', 'LEGION', 'root', 'admitted', 3, 'in_progress', 'A', 1)`,
		`insert into phases (issue, role, claim, handoff_commit, rounds, verdict, entry)
			values ('LEGION-208', 'architect', '', '', 0, '', 1)`,
		`insert into phases (issue, role, claim, handoff_commit, rounds, verdict, entry, entry_task)
			values ('LEGION-208', 'implementer', 'legion-LEGION-208-implementer', '', 0, '', 4, 'Implement the change.')`,
	} {
		if _, err := store.Pool().Exec(ctx, seed); err != nil {
			t.Fatalf("record before the migration: %v", err)
		}
	}

	if _, err := store.Migrate(ctx); err != nil {
		t.Fatalf("migrate through the mark: %v", err)
	}
	rowOf := func(st *Store, role claim.Role) record.PhaseRow {
		t.Helper()
		var rows []record.PhaseRow
		if err := pgx.BeginFunc(ctx, st.Pool(), func(tx pgx.Tx) error {
			var err error
			rows, err = record.NewStore().Phases(ctx, tx, "LEGION-208")
			return err
		}); err != nil {
			t.Fatalf("read the phases rows of LEGION-208: %v", err)
		}
		return record.PhaseRowOf(rows, role)
	}
	for _, role := range []claim.Role{claim.RoleArchitect, claim.RoleImplementer} {
		if row := rowOf(store, role); row.Entry == 0 || row.EntryVerbatim {
			t.Fatalf("the %s's row after the migration = entry %d, sent as written %t; want the entry it stood on, unmarked: "+
				"no row of the release before this one is a resume's", role, row.Entry, row.EntryVerbatim)
		}
	}

	// The resume of an answered root raises the architect's entry 2 and marks its task.
	want := rowOf(store, claim.RoleArchitect)
	want.Entry, want.EntryTask, want.EntryVerbatim = 2, "A person answered your ask ask-1 on LEGION-208; read it. Continue.", true
	if err := pgx.BeginFunc(ctx, store.Pool(), func(tx pgx.Tx) error {
		return record.NewStore().PutPhase(ctx, tx, want)
	}); err != nil {
		t.Fatalf("put the architect's row of the resume's entry: %v", err)
	}
	for _, st := range []*Store{store, reopenStore(t, store)} {
		if got := rowOf(st, claim.RoleArchitect); got.Entry != 2 || got.EntryTask != want.EntryTask || !got.EntryVerbatim {
			t.Fatalf("the architect's row = entry %d, task %q, sent as written %t; want entry 2 carrying %q, marked to be sent as written",
				got.Entry, got.EntryTask, got.EntryVerbatim, want.EntryTask)
		}
	}
}
