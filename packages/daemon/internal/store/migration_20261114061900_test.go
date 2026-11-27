package store

import (
	"context"
	"testing"
)

// phaseEntryVerbatimVersion is the version of the migration this file tests, so the test can
// migrate through everything before it whatever lands beside it.
const phaseEntryVerbatimVersion = 20261114061900

// 20261114061900 adds the mark that the sweep's send of a root architect's entry again sends the
// entry's task as written. Every row the table holds before it reads back unmarked, which that
// send derives again: here the architect row of a root the architect upgrade settled at entry 1
// with no task, and a phase worker's row of the same root, whose task is sent as written whatever
// the mark says. The rows are read as that release reads them, by the column's name: the record
// of this release reads the wait that replaced the mark (20261118073048_phase_entry_awaiting,
// whose own test reads what the mark becomes, and how a resume's entry round-trips).
func TestThePhaseEntryVerbatimMigrationLeavesEveryRowUnmarked(t *testing.T) {
	t.Parallel()
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

	migrateThrough(t, store, phaseEntryVerbatimVersion)
	for _, role := range []string{"architect", "implementer"} {
		var entry int64
		var verbatim bool
		if err := store.Pool().QueryRow(ctx, `select entry, entry_verbatim from phases where issue = 'LEGION-208' and role = $1`,
			role).Scan(&entry, &verbatim); err != nil {
			t.Fatalf("read the %s's row after the migration: %v", role, err)
		}
		if entry == 0 || verbatim {
			t.Fatalf("the %s's row after the migration = entry %d, sent as written %t; want the entry it stood on, unmarked: "+
				"no row of the release before this one is a resume's", role, entry, verbatim)
		}
	}
}
