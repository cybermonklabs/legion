package store

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
)

// waitedStartsVersion is the version of the migration this file tests, so the test can migrate
// through everything before it whatever lands beside it.
const waitedStartsVersion = 20261127020907

// waitedStartColumns are the columns of plan_tasks 20261127020907 adds, in the order the test reads
// them.
var waitedStartColumns = []string{"step_wait_from", "start_report", "start_task"}

// 20261127020907 adds plan_tasks.step_wait_from, the phase the start whose step waits moved the
// issue from, plan_tasks.start_report, the report of the step a move forward ended, and
// plan_tasks.start_task, the task a return out of awaiting_children hands its start, each at the
// empty string on every row it finds: no release before it kept any of them. A release before it
// names its columns in every insert, so its own write of a task still succeeds afterwards, and its
// row reads the empty string in all three. The way back its header gives drops the three columns
// and deletes the migration's version row, and runs a second time with no error; the roll
// forward's Migrate then adds them again, empty on a row that held values before the rollback.
//
// The negative control is the read of each column before the migration, which the database
// refuses as a column it does not know.
func TestMigrationWaitedStarts(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := emptyStore(t)
	migrateThrough(t, store, waitedStartsVersion-1)

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
	for _, column := range waitedStartColumns {
		var value string
		err := store.Pool().QueryRow(ctx, `select `+column+` from plan_tasks where issue = 'LEGION-601'`).Scan(&value)
		if err == nil || !strings.Contains(err.Error(), column) {
			t.Fatalf("reading %s before the migration = %v, want the column to be unknown", column, err)
		}
	}

	if _, err := store.Migrate(ctx); err != nil {
		t.Fatalf("migrate through what a waited start keeps: %v", err)
	}
	read := func(issue string) []string {
		t.Helper()
		values := make([]string, len(waitedStartColumns))
		if err := store.Pool().QueryRow(ctx, `select `+strings.Join(waitedStartColumns, ", ")+` from plan_tasks where issue = $1 and task = 'a'`, issue).
			Scan(&values[0], &values[1], &values[2]); err != nil {
			t.Fatalf("read %v of task a of %s: %v", waitedStartColumns, issue, err)
		}
		return values
	}
	empty := func(values []string) bool { return strings.Join(values, "") == "" }
	if got := read("LEGION-601"); !empty(got) {
		t.Errorf("the task written before the migration reads %v as %q, want all empty: no release before it kept any of them", waitedStartColumns, got)
	}
	write("LEGION-602")
	if got := read("LEGION-602"); !empty(got) {
		t.Errorf("the earlier release's task written after the migration reads %v as %q, want all empty", waitedStartColumns, got)
	}
	if _, err := store.Pool().Exec(ctx, `update plan_tasks set step_wait_from = 'awaiting_children', start_report = ' Handoff commit: head.',
		start_task = 'Do the held work.' where issue = 'LEGION-602'`); err != nil {
		t.Fatalf("keep a waited start's phase, report and task on task a of LEGION-602: %v", err)
	}

	block := wayBackOf(t, waitedStartsVersion, "what a waited start keeps")
	for run := 1; run <= 2; run++ {
		if _, err := store.Pool().Exec(ctx, block, pgx.QueryExecModeSimpleProtocol); err != nil {
			t.Fatalf("run %d of the way back: %v", run, err)
		}
	}
	var columns, version bool
	if err := store.Pool().QueryRow(ctx, `select
		exists (select 1 from information_schema.columns where table_name = 'plan_tasks' and column_name::text = any($2::text[])),
		exists (select 1 from schema_version where version = $1)`, waitedStartsVersion, waitedStartColumns).Scan(&columns, &version); err != nil {
		t.Fatalf("read the schema after the way back: %v", err)
	}
	if columns || version {
		t.Fatalf("after the way back a column stands (%v) or the version row does (%v), want neither", columns, version)
	}

	if _, err := store.Migrate(ctx); err != nil {
		t.Fatalf("the roll forward after the way back: %v", err)
	}
	if got := read("LEGION-602"); !empty(got) {
		t.Errorf("after the rollback and the roll forward task a of LEGION-602 reads %v as %q, want all empty: the way back drops what the release before neither writes nor clears", waitedStartColumns, got)
	}
}
