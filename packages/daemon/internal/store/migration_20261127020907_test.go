package store

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
)

// stepWaitFromVersion is the version of the migration this file tests, so the test can migrate
// through everything before it whatever lands beside it.
const stepWaitFromVersion = 20261127020907

// 20261127020907 adds plan_tasks.step_wait_from, the phase the start whose step waits moved the
// issue from, at the empty string on every row it finds: no release before it kept that phase. A
// release before it names its columns in every insert, so its own write of a task still succeeds
// afterwards, and its row reads the empty string. The way back its header gives drops the column
// and deletes the migration's version row, and runs a second time with no error; the roll
// forward's Migrate then adds the column again, empty on a row that held a phase before the
// rollback.
//
// The negative control is the read of the column before the migration, which the database refuses
// as a column it does not know.
func TestMigrationStepWaitFrom(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := emptyStore(t)
	migrateThrough(t, store, stepWaitFromVersion-1)

	// A version and its task as a release before this migration writes them, each column named.
	write := func(issue string) {
		t.Helper()
		for _, statement := range []string{
			`insert into issue_plans (issue, generation, version, source, artifact_id, given_by, given_at)
				values ($1, 1, 1, 'owner', '6a1f0c3e-2b7d-4e5a-9c81-3d4f5e6a7b80', 'legion-legion-legion-208-architect', now())`,
			`insert into plan_tasks (issue, generation, task, plan_version, block_text, state) values ($1, 1, 'a', 1, 'Do a.', 'waiting')`,
		} {
			if _, err := store.Pool().Exec(ctx, statement, issue); err != nil {
				t.Fatalf("record the plan of %s as a release before the migration writes it: %v", issue, err)
			}
		}
	}
	write("LEGION-601")
	var from string
	err := store.Pool().QueryRow(ctx, `select step_wait_from from plan_tasks where issue = 'LEGION-601'`).Scan(&from)
	if err == nil || !strings.Contains(err.Error(), "step_wait_from") {
		t.Fatalf("reading step_wait_from before the migration = %v, want the column to be unknown", err)
	}

	if _, err := store.Migrate(ctx); err != nil {
		t.Fatalf("migrate through the phase a waited start keeps: %v", err)
	}
	read := func(issue string) string {
		t.Helper()
		var from string
		if err := store.Pool().QueryRow(ctx, `select step_wait_from from plan_tasks where issue = $1 and task = 'a'`, issue).Scan(&from); err != nil {
			t.Fatalf("read step_wait_from of task a of %s: %v", issue, err)
		}
		return from
	}
	if got := read("LEGION-601"); got != "" {
		t.Errorf("the task written before the migration reads step_wait_from %q, want '': no release before it kept that phase", got)
	}
	write("LEGION-602")
	if got := read("LEGION-602"); got != "" {
		t.Errorf("the earlier release's task written after the migration reads step_wait_from %q, want ''", got)
	}
	if _, err := store.Pool().Exec(ctx, `update plan_tasks set step_wait_from = 'retro' where issue = 'LEGION-602'`); err != nil {
		t.Fatalf("keep retro as the phase a waited start of task a of LEGION-602 moved the issue from: %v", err)
	}

	block := wayBackOf(t, stepWaitFromVersion, "the phase a waited start keeps")
	for run := 1; run <= 2; run++ {
		if _, err := store.Pool().Exec(ctx, block, pgx.QueryExecModeSimpleProtocol); err != nil {
			t.Fatalf("run %d of the way back: %v", run, err)
		}
	}
	var column, version bool
	if err := store.Pool().QueryRow(ctx, `select
		exists (select 1 from information_schema.columns where table_name = 'plan_tasks' and column_name = 'step_wait_from'),
		exists (select 1 from schema_version where version = $1)`, stepWaitFromVersion).Scan(&column, &version); err != nil {
		t.Fatalf("read the schema after the way back: %v", err)
	}
	if column || version {
		t.Fatalf("after the way back the column stands (%v) or the version row does (%v), want neither", column, version)
	}

	if _, err := store.Migrate(ctx); err != nil {
		t.Fatalf("the roll forward after the way back: %v", err)
	}
	if got := read("LEGION-602"); got != "" {
		t.Errorf("after the rollback and the roll forward task a of LEGION-602 reads step_wait_from %q, want '': the way back drops what the release before neither writes nor clears", got)
	}
}
