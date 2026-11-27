package store

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"
)

// awaitingChildrenVersion is the version of the migration this file tests, so the test migrates
// through everything before it whatever lands beside it.
const awaitingChildrenVersion = 20261114232555

// 20261114232555 adds the awaiting_children phase and the held start a parent in it keeps, with a
// shape check that ties the held start to the phase as 0057 ties the awaiting_* columns, and a
// column that outlives the phase. Before the migration the phase check refuses the new value; after
// it the value is accepted with a full held start, the old values still pass, and the shape check
// refuses a held start without the phase and the phase without a held start. It adds
// phases.entry_from beside the phase entry as well, null on every row the upgrade finds, since no
// row before it says which phase the move that raised its entry left, and never empty, and
// phases.entry_given_task, entry_given_entry and entry_given_generation, the task a role was last
// given for an entry as it was composed, that entry and the issue's generation it was given in,
// empty, 0 and 0 on every row the upgrade finds, since no release before it wrote them, and never
// null.
func TestTheAwaitingChildrenPhaseAndItsHeldStart(t *testing.T) {
	ctx := context.Background()
	store := emptyStore(t)
	migrateThrough(t, store, awaitingChildrenVersion-1)
	exec := func(statement string) {
		t.Helper()
		if _, err := store.Pool().Exec(ctx, statement); err != nil {
			t.Fatalf("%s: %v", statement, err)
		}
	}
	exec(`insert into issues (key, tree, project, title, phase, generation, status, rank, last_dispatch_seq)
		values ('LEGION-208', 'LEGION-208', 'LEGION', 'parent', 'implementing', 1, 'in_progress', 'A', 1)`)
	exec(`insert into phases (issue, role, claim, handoff_commit, rounds, verdict, entry, entry_task, entry_raised_at)
		values ('LEGION-208', 'implementer', 'legion-LEGION-208-implementer', '', 0, '', 2, 'Merge main forward.', now())`)

	// Before the migration the phase check refuses the new value: a daemon of an earlier release
	// cannot write a parent into the wait.
	if _, err := store.Pool().Exec(ctx, `update issues set phase = 'awaiting_children' where key = 'LEGION-208'`); err == nil ||
		!strings.Contains(err.Error(), "issues_phase_check") {
		t.Fatalf("setting awaiting_children before the migration = %v, want the phase check to refuse it", err)
	}

	if _, err := store.Migrate(ctx); err != nil {
		t.Fatalf("migrate through awaiting_children: %v", err)
	}

	// The phases row the upgrade found names no phase its entry was raised from.
	var from *string
	if err := store.Pool().QueryRow(ctx, `select entry_from from phases where issue = 'LEGION-208' and role = 'implementer'`).
		Scan(&from); err != nil {
		t.Fatalf("read the implementer's phases row after the migration: %v", err)
	}
	if from != nil {
		t.Fatalf("the phases row the upgrade found reads entry_from %q, want null: no row before the column said which move raised its entry", *from)
	}
	exec(`update phases set entry_from = 'retro' where issue = 'LEGION-208' and role = 'implementer'`)
	if _, err := store.Pool().Exec(ctx, `update phases set entry_from = '' where issue = 'LEGION-208' and role = 'implementer'`); err == nil ||
		!strings.Contains(err.Error(), "constraint") {
		t.Fatalf("an empty entry_from = %v, want a constraint to refuse it: an entry no move raised reads null", err)
	}

	// The phases row the upgrade found carries no given task, no entry and no generation it was
	// given in, read as '', 0 and 0, and each column refuses a null.
	var given string
	var givenEntry, givenGeneration int64
	if err := store.Pool().QueryRow(ctx, `select entry_given_task, entry_given_entry, entry_given_generation from phases
		where issue = 'LEGION-208' and role = 'implementer'`).Scan(&given, &givenEntry, &givenGeneration); err != nil {
		t.Fatalf("read the implementer's given task after the migration: %v", err)
	}
	if given != "" || givenEntry != 0 || givenGeneration != 0 {
		t.Fatalf("the phases row the upgrade found reads entry_given_task %q for entry %d in generation %d, want '' for 0 in 0: no release before the columns wrote them",
			given, givenEntry, givenGeneration)
	}
	for _, column := range []string{"entry_given_task", "entry_given_entry", "entry_given_generation"} {
		if _, err := store.Pool().Exec(ctx, `update phases set `+column+` = null where issue = 'LEGION-208' and role = 'implementer'`); err == nil ||
			!strings.Contains(err.Error(), "null") {
			t.Fatalf("a null %s = %v, want the column to refuse it: a row no release wrote it on reads its default", column, err)
		}
	}

	// The new value is accepted with a full held start, the children it waits on among it, and the
	// outliving column stands beside it: children_waited_at set.
	exec(`update issues set phase = 'awaiting_children', children_wait_start = 'implementing',
		children_wait_task = 'merge the base and finish', children_wait_reason = 'a child of it is open',
		children_wait_since = now(), children_wait_children = '{LEGION-209,LEGION-210}', children_waited_at = now()
		where key = 'LEGION-208'`)

	var waitedAt, since *string
	var children []string
	if err := store.Pool().QueryRow(ctx, `select children_waited_at::text, children_wait_since::text, children_wait_children
		from issues where key = 'LEGION-208'`).Scan(&waitedAt, &since, &children); err != nil {
		t.Fatalf("read the held start back: %v", err)
	}
	if waitedAt == nil || since == nil {
		t.Fatalf("after the move children_waited_at = %v and children_wait_since = %v, want both times set", waitedAt, since)
	}
	if want := []string{"LEGION-209", "LEGION-210"}; !slices.Equal(children, want) {
		t.Fatalf("after the move children_wait_children = %v, want %v, in the order written", children, want)
	}

	// Leaving the phase clears the held start, the children with it; the outliving column may
	// stand, so the move out clears only the held-start columns.
	exec(`update issues set phase = 'implementing', children_wait_start = null, children_wait_task = null,
		children_wait_reason = null, children_wait_since = null, children_wait_children = null where key = 'LEGION-208'`)
	if err := store.Pool().QueryRow(ctx, `select children_waited_at::text from issues where key = 'LEGION-208'`).
		Scan(&waitedAt); err != nil {
		t.Fatalf("read the outliving column back: %v", err)
	}
	if waitedAt == nil {
		t.Fatalf("after the return children_waited_at = %v, want it to outlive the phase", waitedAt)
	}

	// The old phase values still pass the phase check, each with no held start.
	for _, p := range []string{"admitted", "planning", "testing", "reviewing", "accepting", "retro", "merging", "done"} {
		exec(fmt.Sprintf(`update issues set phase = '%s' where key = 'LEGION-208'`, p))
	}

	// The shape check ties the held start to the phase.
	for _, refused := range []struct{ why, statement string }{
		{"a held start outside the phase", `update issues set children_wait_start = 'implementing', children_wait_task = 't',
			children_wait_reason = 'r', children_wait_since = now() where key = 'LEGION-208'`},
		{"the children of a wait outside the phase", `update issues set children_wait_children = '{LEGION-209}' where key = 'LEGION-208'`},
		{"the phase with no held start", `update issues set phase = 'awaiting_children' where key = 'LEGION-208'`},
		{"the phase with only part of the held start", `update issues set phase = 'awaiting_children',
			children_wait_start = 'implementing' where key = 'LEGION-208'`},
		{"the phase with a held_from set", `update issues set phase = 'awaiting_children', children_wait_start = 'implementing',
			children_wait_task = 't', children_wait_reason = 'r', children_wait_since = now(), held_from = 'implementing' where key = 'LEGION-208'`},
		{"an empty held phase", `update issues set phase = 'awaiting_children', children_wait_start = '',
			children_wait_task = 't', children_wait_reason = 'r', children_wait_since = now() where key = 'LEGION-208'`},
	} {
		if _, err := store.Pool().Exec(ctx, refused.statement); err == nil || !strings.Contains(err.Error(), "constraint") {
			t.Fatalf("%s = %v, want a constraint to refuse it", refused.why, err)
		}
	}
}
