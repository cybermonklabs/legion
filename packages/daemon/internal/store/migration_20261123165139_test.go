package store

import (
	"context"
	"strings"
	"testing"
)

// experimentAssignmentPlanVersion is the version of the migration this file tests, so the test can
// migrate through everything before it whatever lands beside it.
const experimentAssignmentPlanVersion = 20261123165139

// 20261123165139 adds experiment_assignments.plan, how the issue a row was drawn for is done, and
// back-fills nothing: a row drawn before it reads the empty plan, the issue of no group, which
// runs as an issue with no experiment runs. The column takes the empty word and the three words of
// a variant's `plan` and no other, so a write of any other word is refused. A release before it
// names its columns in every insert, so its own write of a draw — the seven-column insert of its
// record.Store.PutAssignments — still succeeds afterwards, and its row reads the empty plan.
//
// The negative control is the read of the column before the migration, which the database
// refuses as a column it does not know. The control of the check is the migration without it:
// the write of 'alone' then succeeds, and this test fails.
func TestMigrationExperimentAssignmentPlan(t *testing.T) {
	ctx := context.Background()
	store := emptyStore(t)
	migrateThrough(t, store, experimentAssignmentPlanVersion-1)

	for _, seed := range []string{
		`insert into issues (key, tree, project, title, phase, generation, status, rank, last_dispatch_seq)
			values ('LEGION-501', 'LEGION-501', 'LEGION', 'root', 'admitted', 1, 'in_progress', 'A', 1)`,
		`insert into issues (key, tree, project, title, phase, generation, status, rank, last_dispatch_seq)
			values ('LEGION-502', 'LEGION-502', 'LEGION', 'root', 'admitted', 1, 'in_progress', 'A', 1)`,
		// A draw as a release before this migration writes it: its seven columns, named.
		`insert into experiment_assignments (issue_key, experiment, unit, variant, mode, drawn_at, carried)
			values ('LEGION-501', 'planning', 'issue', 'b', 'random', now(), false)`,
	} {
		if _, err := store.Pool().Exec(ctx, seed); err != nil {
			t.Fatalf("record before the migration: %v", err)
		}
	}
	var plan string
	err := store.Pool().QueryRow(ctx, `select plan from experiment_assignments where issue_key = 'LEGION-501'`).Scan(&plan)
	if err == nil || !strings.Contains(err.Error(), "plan") {
		t.Fatalf("reading plan before the migration = %v, want the column to be unknown", err)
	}

	if _, err := store.Migrate(ctx); err != nil {
		t.Fatalf("migrate through the plan of an assignment: %v", err)
	}

	if err := store.Pool().QueryRow(ctx, `select plan from experiment_assignments
		where issue_key = 'LEGION-501' and experiment = 'planning'`).Scan(&plan); err != nil {
		t.Fatalf("read the plan of the row drawn before the migration: %v", err)
	}
	if plan != "" {
		t.Errorf("the row drawn before the migration reads the plan %q, want '': the migration back-fills nothing", plan)
	}

	if _, err := store.Pool().Exec(ctx, `insert into experiment_assignments
		(issue_key, experiment, unit, variant, mode, drawn_at, carried, plan)
		values ('LEGION-502', 'other', 'issue', 'a', 'random', now(), false, 'alone')`); err == nil {
		t.Error("an assignment of the plan 'alone' was written, want the check to refuse a word that is none of the plans")
	}

	// The write of a release before this migration still succeeds over the new column.
	if _, err := store.Pool().Exec(ctx, `insert into experiment_assignments (issue_key, experiment, unit, variant, mode, drawn_at, carried)
		values ('LEGION-502', 'planning', 'issue', 'a', 'random', now(), false)`); err != nil {
		t.Fatalf("the earlier release's write of a draw after the migration: %v", err)
	}
	if err := store.Pool().QueryRow(ctx, `select plan from experiment_assignments
		where issue_key = 'LEGION-502' and experiment = 'planning'`).Scan(&plan); err != nil {
		t.Fatalf("read the plan of the earlier release's row: %v", err)
	}
	if plan != "" {
		t.Errorf("the earlier release's row reads the plan %q, want ''", plan)
	}
	for _, word := range []string{"owner", "default", "owner-alone"} {
		if _, err := store.Pool().Exec(ctx, `insert into experiment_assignments
			(issue_key, experiment, unit, variant, mode, drawn_at, carried, plan)
			values ('LEGION-502', $1, 'issue', 'a', 'random', now(), false, $2)`, "takes-"+word, word); err != nil {
			t.Errorf("an assignment of the plan %q: %v, want it written", word, err)
		}
	}
}
