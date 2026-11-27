package store

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
)

// planTasksParentRefVersion is the version of the migration this file tests, so the test can
// migrate through everything before it whatever lands beside it.
const planTasksParentRefVersion = 20261124022547

// 20261124022547 adds plan_tasks.parent_ref, the parent each task's block names, and back-fills
// nothing: a task the release before wrote reads the empty parent, a block that names none, and
// that release's own insert, which names its columns and none of this one, still lands empty. The
// negative control is the read of the column before the migration, which the database refuses as a
// column it does not know.
//
// The way back of the owner's plan (20261117034837) drops plan_tasks, this column with it, so its
// block deletes this migration's version row too: the roll forward's Migrate then creates the
// table and adds the column again. The control of that is the block without the delete of this
// version's row: the roll forward then creates the table without the column, and this test fails.
func TestThePlanTasksParentStartsEmptyAndComesBackAfterTheOwnersPlanWayBack(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := emptyStore(t)
	migrateThrough(t, store, planTasksParentRefVersion-1)
	exec := func(statement string) {
		t.Helper()
		if _, err := store.Pool().Exec(ctx, statement, pgx.QueryExecModeSimpleProtocol); err != nil {
			t.Fatalf("%s: %v", statement, err)
		}
	}
	// A plan as the release before writes it: each statement names its columns.
	const releaseBeforesPlan = `insert into issue_plans (issue, generation, version, source, artifact_id, given_by, given_at)
		values ('LEGION-208', 1, 1, 'owner', 'artifact-208', 'legion-legion-legion-208-architect', now())`
	const releaseBeforesTask = `insert into plan_tasks (issue, generation, task, plan_version, block_text, state, running_kind, position)
		values ('LEGION-208', 1, '%s', 1, 'Build the widget.', 'running', 'step', 1)`
	exec(releaseBeforesPlan)
	exec(fmt.Sprintf(releaseBeforesTask, "a"))
	const parents = `select task || '=' || parent_ref from plan_tasks where issue = 'LEGION-208' order by task`
	if _, err := store.Pool().Exec(ctx, parents); err == nil || !strings.Contains(err.Error(), "parent_ref") {
		t.Fatalf("reading parent_ref before the migration = %v, want the column to be unknown", err)
	}

	if _, err := store.Migrate(ctx); err != nil {
		t.Fatalf("migrate through the parent of a plan task: %v", err)
	}
	if got, want := rowsAsText(t, store, parents), []string{"a="}; !reflect.DeepEqual(got, want) {
		t.Fatalf("the tasks after the migration (task=parent_ref) = %v, want %v: the row it found reads no parent", got, want)
	}
	exec(fmt.Sprintf(releaseBeforesTask, "b"))
	if got, want := rowsAsText(t, store, parents), []string{"a=", "b="}; !reflect.DeepEqual(got, want) {
		t.Fatalf("the tasks after the release before's insert = %v, want %v: its row reads no parent", got, want)
	}

	exec(strings.ReplaceAll(ownerStepsWayBack(t), "<PROJECT>", "LEGION"))
	const recorded = `select version::text from schema_version where version in (20261117034837, 20261124022547) order by version`
	if got := rowsAsText(t, store, recorded); len(got) != 0 {
		t.Fatalf("after the way back of %s the versions %v stay recorded, want neither: a roll forward runs only a migration with no row", ownerStepsMigration, got)
	}
	if _, err := store.Migrate(ctx); err != nil {
		t.Fatalf("roll forward after the way back of %s: %v", ownerStepsMigration, err)
	}
	exec(releaseBeforesPlan)
	exec(`insert into plan_tasks (issue, generation, task, plan_version, block_text, state, parent_ref)
		values ('LEGION-208', 1, 'c', 1, 'Build the gadget.', 'waiting', 'dispatch://ACME-1/spec#b-r1')`)
	if got, want := rowsAsText(t, store, parents), []string{"c=dispatch://ACME-1/spec#b-r1"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("the tasks after the roll forward = %v, want %v: the table again, with its parent column", got, want)
	}
}
