package store

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

// taskCopiesVersion is the version of the migration this file tests, so a test can migrate through
// everything before it whatever lands beside it.
const taskCopiesVersion = 20261121165047

// 20261121165047 gives each task of a plan its branch, the needs whose join conflicted and why it
// started again, keeps the names a generation gave to its tasks, and lets the outbox queue a
// task's branch. A task the upgrade finds was written by a release that gave no task a branch of
// its own, so it reads no branch, which is the issue's own, no conflicted join and no reason; the
// three columns are unknown before the migration, which is what shows the fixture is the old
// shape. The outbox refuses a task_branch
// row before the migration and queues it after, and every kind the check named before is still
// queued, since the migration writes the list whole, the kinds of 20261119003157, the list it
// stands above, among them; a kind that is none is still refused. A name given twice in one
// generation is refused by the table's own key.
func TestMigrationTaskCopiesKeepsEachTaskAndEveryOutboxKind(t *testing.T) {
	ctx := context.Background()
	store := emptyStore(t)
	migrateThrough(t, store, taskCopiesVersion-1)
	exec := func(sql string) error {
		_, err := store.Pool().Exec(ctx, sql)
		return err
	}
	for _, seed := range []string{
		`insert into issue_plans (issue, generation, version, source, artifact_id, given_by, given_at)
			values ('LEGION-208', 1, 1, 'owner', 'artifact-208', 'legion-legion-legion-208-architect', now())`,
		`insert into plan_tasks (issue, generation, task, plan_version, block_text, state)
			values ('LEGION-208', 1, 'a', 1, 'Build the widget.', 'running')`,
	} {
		if err := exec(seed); err != nil {
			t.Fatalf("record before the migration: %v", err)
		}
	}
	for _, column := range []string{"branch = 'legion/LEGION-208'", "join_conflicts = '{b}'", "start_reason = 'again'"} {
		if err := exec("update plan_tasks set " + column); err == nil || !strings.Contains(err.Error(), "does not exist") {
			t.Fatalf("update plan_tasks set %s before the migration = %v, want the column unknown: the fixture is not the old shape", column, err)
		}
	}
	queue := func(kind string) error {
		return exec(`insert into outbox (kind, issue, payload, attempts, next_at, last_error)
			values ('` + kind + `', 'LEGION-208', '{}', 0, now(), '')`)
	}
	refusedBy := func(err error, code, constraint string) bool {
		var pgErr *pgconn.PgError
		return errors.As(err, &pgErr) && pgErr.Code == code && pgErr.ConstraintName == constraint
	}
	if err := queue("task_branch"); !refusedBy(err, "23514", "outbox_kind_check") {
		t.Fatalf("queueing a task_branch row before the migration = %v, want the kind constraint to refuse it (23514): it is the row the migration exists for", err)
	}

	if _, err := store.Migrate(ctx); err != nil {
		t.Fatalf("migrate through the task copies: %v", err)
	}
	var branch, reason string
	var conflicts []string
	if err := store.Pool().QueryRow(ctx, `select branch, join_conflicts, start_reason from plan_tasks
		where issue = 'LEGION-208' and generation = 1 and task = 'a'`).Scan(&branch, &conflicts, &reason); err != nil {
		t.Fatalf("read task a after the migration: %v", err)
	}
	if branch != "" || len(conflicts) != 0 || conflicts == nil || reason != "" {
		t.Fatalf("task a after the migration holds branch %q, join conflicts %#v and start reason %q, want '', '{}' and '': no release before gave a task a branch",
			branch, conflicts, reason)
	}
	for _, kind := range []string{
		"dispatch_status", "dispatch_message", "notice", "controller_notice", "supervise", "gate_seed",
		"linger_close", "workspace_remove", "issue_branch", "issue_suspend", "check_wake",
		"check_look", "check_bound", "role_message", "phase_ended", "issue_bundle", "issue_release",
		"gated_steps_publish", "claim_release", "hook_prompt", "design_gates_publish", "update_ask",
		"merge_queue_read", "merge_queue_removal", "role_holders_publish", "step_record", "plan_record",
		"conflict_confirm", "merge_forward", "task_branch",
	} {
		if err := queue(kind); err != nil {
			t.Fatalf("queue a %s row after the migration: %v", kind, err)
		}
	}
	if err := queue("nope"); !refusedBy(err, "23514", "outbox_kind_check") {
		t.Fatalf("queueing a row of a kind that is none = %v, want the constraint to refuse it (23514)", err)
	}
	give := `insert into plan_branches (issue, generation, branch) values ('LEGION-208', 1, 'legion/LEGION-208')`
	if err := exec(give); err != nil {
		t.Fatalf("record a name given: %v", err)
	}
	if err := exec(give); !refusedBy(err, "23505", "plan_branches_pkey") {
		t.Fatalf("recording the same name again in one generation = %v, want the key to refuse it (23505)", err)
	}
}
