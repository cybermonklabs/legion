package store

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/sjawhar/legion/daemon/internal/record"
)

// pullRequestsByTaskVersion is the version of the migration this file tests, so the test can
// migrate through everything before it whatever lands beside it.
const pullRequestsByTaskVersion = 20261122122826

// taskCopiesKinds is every outbox kind 20261121165047 names, thirty-seven, the check this migration
// writes again: main's two basis seeds, its merge_holds_changed and its three state kinds among
// them, which this stack's lists name as main's own (the last migration to write a list owns every
// value of it). A test that queues a row of each before a way back holds that way back to keep
// them: main's 20261122154643, 20261126110259 and 20261126163320 stay recorded through it, and a
// way back whose check left one out would stop at that row (SQLSTATE 23514).
var taskCopiesKinds = []string{
	"dispatch_status", "dispatch_message", "notice", "controller_notice", "supervise", "gate_seed",
	"linger_close", "workspace_remove", "issue_branch", "issue_suspend", "check_wake",
	"check_look", "check_bound", "role_message", "phase_ended", "issue_bundle", "issue_release",
	"gated_steps_publish", "claim_release", "hook_prompt", "design_gates_publish", "update_ask",
	"merge_queue_read", "merge_queue_removal", "role_holders_publish", "step_record", "plan_record",
	"conflict_confirm", "merge_forward", "design_changes", "task_branch", "pending_basis_seed",
	"reparent_seed", "merge_holds_changed", "state_change", "state_admission", "state_boot",
}

// 20261122122826 keys pull_requests by the issue and the branch, and the table stores no task. The
// release before kept one row for each issue, with no task column, and ran plans of one task: each
// row it wrote keeps every field and reads the task of its issue's plan at the issue's generation
// whose branch it is on, so the row of the issue's merge (record.MergePullRequest) is that row; a
// row of an issue with no plan rows at its generation, one that ran before plans and one whose plan
// is of an earlier generation, reads the task "", and that row is its merge's. After the migration
// the table still has no task column, a second branch of an issue keeps a row of its own, and one
// branch keeps one row. The outbox check is written again with every kind 20261121165047 named and
// task_pull_requests_close, which it refused before; a kind that is none is still refused. The way
// back the header gives keeps the row of each issue's merge and deletes the others, keys the table
// by the issue again, drops the kind, keeps the queued row of each kind 20261121165047 names, main's
// two basis seeds and its merge_holds_changed among them (taskCopiesKinds), and deletes its version
// row, so the roll-forward runs the migration again.
func TestMigrationPullRequestsByTaskKeysEachRowByItsBranch(t *testing.T) {
	ctx := context.Background()
	store := emptyStore(t)
	migrateThrough(t, store, pullRequestsByTaskVersion-1)
	exec := func(sql string, args ...any) error {
		_, err := store.Pool().Exec(ctx, sql, args...)
		return err
	}
	seed := func(sql string, args ...any) {
		t.Helper()
		if err := exec(sql, args...); err != nil {
			t.Fatalf("record before the migration: %s: %v", sql, err)
		}
	}
	// LEGION-208's plan holds one task, x; LEGION-209 has no plan rows; LEGION-210's plan is of
	// generation 1, and the issue stands at generation 2.
	for _, issue := range []struct {
		key                string
		generation, number int
	}{{"LEGION-208", 1, 1208}, {"LEGION-209", 1, 1209}, {"LEGION-210", 2, 1210}} {
		seed(`insert into issues (key, tree, project, title, phase, generation, status, rank, last_dispatch_seq)
			values ($1, $1, 'LEGION', 'the issue', 'reviewing', $2, 'needs_review', 'A', 1)`, issue.key, issue.generation)
		seed(`insert into pull_requests (issue, repo, number, branch, head_sha, head_updated_at,
			failing, cancelled, fix_attempts, blocked_attempts, check_runs, generation, snapshot, head_counted, planned_red,
			review_seen, state, checked_head, required_workflows, required_workflows_head, mergeability, merge_state, base, base_moved)
			values ($1, 'acme/widgets', $2, 'legion/' || $1, 'head-' || $1, now(), '[]', '[]', 0, 0,
			 '[]', 3, 'snap-' || $1, '', false, 0, 'open', 'head-' || $1, '[]', '', 'MERGEABLE', 'clean', 'main', '')`,
			issue.key, issue.number)
	}
	for _, issue := range []string{"LEGION-208", "LEGION-210"} {
		seed(`insert into issue_plans (issue, generation, version, source, artifact_id, given_by, given_at)
			values ($1, 1, 1, 'owner', 'artifact', 'legion-legion-legion-208-architect', now())`, issue)
		seed(`insert into plan_tasks (issue, generation, task, plan_version, block_text, state)
			values ($1, 1, 'x', 1, 'Build the widget.', 'running')`, issue)
	}
	if err := exec(`update pull_requests set task = 'x'`); err == nil || !strings.Contains(err.Error(), "does not exist") {
		t.Fatalf("update pull_requests set task before the migration = %v, want the column unknown: the fixture is not the old shape", err)
	}
	refusedBy := func(err error, code, constraint string) bool {
		var pgErr *pgconn.PgError
		return errors.As(err, &pgErr) && pgErr.Code == code && pgErr.ConstraintName == constraint
	}
	queue := func(kind string) error {
		return exec(`insert into outbox (kind, issue, payload, attempts, next_at, last_error)
			values ($1, 'LEGION-208', '{}', 0, now(), '')`, kind)
	}
	if err := queue("task_pull_requests_close"); !refusedBy(err, "23514", "outbox_kind_check") {
		t.Fatalf("queueing a task_pull_requests_close row before the migration = %v, want the kind constraint to refuse it (23514)", err)
	}
	before := rowsAsText(t, store, `select issue || ' ' || number || ' ' || branch || ' ' || head_sha || ' ' || snapshot || ' ' || mergeability
		from pull_requests order by issue`)

	if _, err := store.Migrate(ctx); err != nil {
		t.Fatalf("migrate through the pull requests by task: %v", err)
	}
	if after := rowsAsText(t, store, `select issue || ' ' || number || ' ' || branch || ' ' || head_sha || ' ' || snapshot || ' ' || mergeability
		from pull_requests order by issue`); strings.Join(after, "\n") != strings.Join(before, "\n") {
		t.Fatalf("the pull requests after the migration = %q, want each row whole: %q", after, before)
	}
	read := func(name string, fn func(tx pgx.Tx) (*record.PullRequest, error)) *record.PullRequest {
		t.Helper()
		var pr *record.PullRequest
		if err := Transact(ctx, store.Pool(), func(ctx context.Context, tx pgx.Tx) error {
			var err error
			pr, err = fn(tx)
			return err
		}); err != nil {
			t.Fatalf("read %s after the migration: %v", name, err)
		}
		return pr
	}
	records := record.NewStore()
	for _, want := range []struct{ issue, task string }{{"LEGION-208", "x"}, {"LEGION-209", ""}, {"LEGION-210", ""}} {
		merge := read("the merge pull request of "+want.issue, func(tx pgx.Tx) (*record.PullRequest, error) {
			return records.MergePullRequest(ctx, tx, want.issue)
		})
		if merge == nil || merge.Issue != want.issue || merge.Task != want.task || merge.HeadSHA != "head-"+want.issue {
			t.Fatalf("the merge pull request of %s after the migration = %+v, want its one row, of task %q", want.issue, merge, want.task)
		}
		own := read("the pull request of task "+want.task+" of "+want.issue, func(tx pgx.Tx) (*record.PullRequest, error) {
			return records.PullRequestOfTask(ctx, tx, want.issue, want.task)
		})
		if own == nil || own.Number != merge.Number {
			t.Fatalf("the pull request of task %q of %s after the migration = %+v, want #%d", want.task, want.issue, own, merge.Number)
		}
	}

	if err := exec(`update pull_requests set task = 'x'`); err == nil || !strings.Contains(err.Error(), "does not exist") {
		t.Fatalf("update pull_requests set task after the migration = %v, want the column unknown: the table stores no task", err)
	}
	second := `insert into pull_requests (issue, repo, number, branch, head_sha, head_updated_at, failing, fix_attempts,
		blocked_attempts, check_runs, generation, snapshot, head_counted)
		values ('LEGION-208', $1, $2, $3, 'head-y', now(), '[]', 0, 0, '[]', 0, '', '')`
	if err := exec(second, "acme/widgets", 2208, "legion/LEGION-208.y"); err != nil {
		t.Fatalf("record the pull request of a second branch of LEGION-208: %v", err)
	}
	// Another spelling of the repository, which 0004's pull_requests_repo_branch keys apart, so only
	// the key (issue, branch) can refuse it.
	if err := exec(second, "ACME/widgets", 3208, "legion/LEGION-208.y"); !refusedBy(err, "23505", "pull_requests_pkey") {
		t.Fatalf("a second row of legion/LEGION-208.y of LEGION-208 = %v, want the key (issue, branch) to refuse it (23505)", err)
	}
	for _, kind := range append(append([]string{}, taskCopiesKinds...), "task_pull_requests_close") {
		if err := queue(kind); err != nil {
			t.Fatalf("queue a %s row after the migration: %v", kind, err)
		}
	}
	if err := queue("nope"); !refusedBy(err, "23514", "outbox_kind_check") {
		t.Fatalf("queueing a row of a kind that is none = %v, want the constraint to refuse it (23514)", err)
	}

	if err := exec(wayBackOf(t, pullRequestsByTaskVersion, "the pull requests by task")); err != nil {
		t.Fatalf("run the way back: %v", err)
	}
	if got, want := rowsAsText(t, store, `select issue || ' ' || number from pull_requests order by issue`), []string{
		"LEGION-208 1208", "LEGION-209 1209", "LEGION-210 1210",
	}; strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("the pull requests after the way back = %q, want %q: each issue's merge row, task y's deleted", got, want)
	}
	if got := rowsAsText(t, store, `select count(*)::text from outbox where kind = 'task_pull_requests_close'`); got[0] != "0" {
		t.Fatalf("task_pull_requests_close rows after the way back = %s, want none", got[0])
	}
	if err := queue("task_pull_requests_close"); !refusedBy(err, "23514", "outbox_kind_check") {
		t.Fatalf("queueing a task_pull_requests_close row after the way back = %v, want the kind constraint to refuse it (23514)", err)
	}
	if err := exec(`insert into pull_requests (issue, repo, number, branch, head_sha, head_updated_at, failing, fix_attempts,
		blocked_attempts, check_runs, generation, snapshot, head_counted)
		values ('LEGION-208', 'acme/widgets', 4208, 'legion/LEGION-208.z', 'head-z', now(), '[]', 0, 0, '[]', 0, '', '')`); !refusedBy(err, "23505", "pull_requests_pkey") {
		t.Fatalf("a second row of LEGION-208 after the way back = %v, want the key issue to refuse it (23505)", err)
	}
	if got := rowsAsText(t, store, `select count(*)::text from schema_version where version = 20261122122826`); got[0] != "0" {
		t.Fatalf("schema_version rows of %d after the way back = %s, want none", pullRequestsByTaskVersion, got[0])
	}
	if _, err := store.Migrate(ctx); err != nil {
		t.Fatalf("roll forward after the way back: %v", err)
	}
	if merge := read("the merge pull request of LEGION-208 after the roll-forward", func(tx pgx.Tx) (*record.PullRequest, error) {
		return records.MergePullRequest(ctx, tx, "LEGION-208")
	}); merge == nil || merge.Task != "x" {
		t.Fatalf("the merge pull request of LEGION-208 after the roll-forward = %+v, want its row of task x again", merge)
	}
}

// settledHeadsKeys is the key and the foreign key of pull_request_settled_heads as the database
// holds them, each as its name and its definition.
const settledHeadsKeys = `select k.conname || ' ' || pg_get_constraintdef(k.oid) from pg_constraint k
	where k.conrelid = 'public.pull_request_settled_heads'::regclass and k.contype in ('p', 'f')
	order by 1`

// 20261122122826 keys what a pull request remembers of its settled heads by the issue and the
// branch, as it keys pull_requests. A row the release before wrote takes the branch of its issue's
// one pull request and keeps every other field. After the migration a second branch of the issue
// keeps a memory of its own, of the same kind and head as the first branch's, and a memory of a
// branch with no pull request is refused by the foreign key on pull_requests (issue, branch). The
// way back deletes the pull request of the second branch, which takes its memory with it (on
// delete cascade), before it keys the table by (issue, kind, head) again, so that key is written
// over rows that hold each of its values once; and it writes the foreign key on pull_requests
// (issue) again, which refuses a memory of an issue with no pull request. The roll-forward gives
// the row its branch again.
func TestMigrationPullRequestsByTaskKeysEachSettledHeadByItsBranch(t *testing.T) {
	ctx := context.Background()
	store := emptyStore(t)
	migrateThrough(t, store, pullRequestsByTaskVersion-1)
	exec := func(sql string, args ...any) error {
		_, err := store.Pool().Exec(ctx, sql, args...)
		return err
	}
	seed := func(sql string, args ...any) {
		t.Helper()
		if err := exec(sql, args...); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	refusedBy := func(err error, code, constraint string) bool {
		var pgErr *pgconn.PgError
		return errors.As(err, &pgErr) && pgErr.Code == code && pgErr.ConstraintName == constraint
	}
	// LEGION-208 has no plan rows, so the way back keeps its pull request on legion/LEGION-208.
	seed(`insert into issues (key, tree, project, title, phase, generation, status, rank, last_dispatch_seq)
		values ('LEGION-208', 'LEGION-208', 'LEGION', 'the issue', 'reviewing', 1, 'needs_review', 'A', 1)`)
	pullRequest := `insert into pull_requests (issue, repo, number, branch, head_sha, head_updated_at, failing, fix_attempts,
		blocked_attempts, check_runs, generation, snapshot, head_counted)
		values ('LEGION-208', 'acme/widgets', $1, $2, 'h2', now(), '[]', 0, 0, '[]', 0, '', '')`
	seed(pullRequest, 1208, "legion/LEGION-208")
	seed(`insert into pull_request_settled_heads (issue, kind, position, head, fence)
		values ('LEGION-208', 'pull_request', 0, 'h1', '{"checkRuns": []}'::jsonb),
		('LEGION-208', 'head_run', 0, 'h1', '{"checkRuns": [{"name": "unit", "id": 7}]}'::jsonb)`)
	const memory = `select issue || ' ' || kind || ' ' || position || ' ' || head || ' ' || fence::text
		from pull_request_settled_heads order by kind`
	before := rowsAsText(t, store, memory)
	if len(before) != 2 {
		t.Fatalf("the settled heads before the migration = %q, want the two rows the fixture wrote", before)
	}
	const branches = `select distinct issue || ' ' || branch from pull_request_settled_heads order by 1`

	if _, err := store.Migrate(ctx); err != nil {
		t.Fatalf("migrate through the pull requests by task: %v", err)
	}
	if got := rowsAsText(t, store, memory); strings.Join(got, "\n") != strings.Join(before, "\n") {
		t.Fatalf("the settled heads after the migration = %q, want each row whole: %q", got, before)
	}
	if got, want := rowsAsText(t, store, branches), []string{"LEGION-208 legion/LEGION-208"}; strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("the branches of the settled heads after the migration = %q, want %q: the branch of the issue's one pull request", got, want)
	}
	if got, want := rowsAsText(t, store, settledHeadsKeys), []string{
		"pull_request_settled_heads_issue_fkey FOREIGN KEY (issue, branch) REFERENCES pull_requests(issue, branch) ON DELETE CASCADE",
		"pull_request_settled_heads_pkey PRIMARY KEY (issue, branch, kind, head)",
	}; strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("the keys of the settled heads after the migration = %q, want %q", got, want)
	}
	seed(pullRequest, 2208, "legion/LEGION-208.y")
	other := `insert into pull_request_settled_heads (issue, branch, kind, position, head, fence)
		values ('LEGION-208', $1, 'pull_request', 0, 'h1', '{"checkRuns": []}'::jsonb)`
	if err := exec(other, "legion/LEGION-208.y"); err != nil {
		t.Fatalf("remember h1 for the pull request of legion/LEGION-208.y, which legion/LEGION-208's remembers too: %v", err)
	}
	if err := exec(other, "legion/LEGION-208.z"); !refusedBy(err, "23503", "pull_request_settled_heads_issue_fkey") {
		t.Fatalf("remember h1 for legion/LEGION-208.z, which has no pull request = %v, want the foreign key to refuse it (23503)", err)
	}

	if err := exec(wayBackOf(t, pullRequestsByTaskVersion, "the pull requests by task")); err != nil {
		t.Fatalf("run the way back: %v", err)
	}
	if got := rowsAsText(t, store, memory); strings.Join(got, "\n") != strings.Join(before, "\n") {
		t.Fatalf("the settled heads after the way back = %q, want legion/LEGION-208's whole and legion/LEGION-208.y's deleted: %q", got, before)
	}
	if err := exec(`select branch from pull_request_settled_heads`); err == nil || !strings.Contains(err.Error(), "does not exist") {
		t.Fatalf("select branch from pull_request_settled_heads after the way back = %v, want the column unknown", err)
	}
	if got, want := rowsAsText(t, store, settledHeadsKeys), []string{
		"pull_request_settled_heads_issue_fkey FOREIGN KEY (issue) REFERENCES pull_requests(issue) ON DELETE CASCADE",
		"pull_request_settled_heads_pkey PRIMARY KEY (issue, kind, head)",
	}; strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("the keys of the settled heads after the way back = %q, want %q: the ones 20261120144241 wrote", got, want)
	}
	if err := exec(`insert into pull_request_settled_heads (issue, kind, position, head, fence)
		values ('LEGION-209', 'pull_request', 0, 'h1', '{"checkRuns": []}'::jsonb)`); !refusedBy(err, "23503", "pull_request_settled_heads_issue_fkey") {
		t.Fatalf("remember h1 for LEGION-209, which has no pull request, after the way back = %v, want the foreign key to refuse it (23503)", err)
	}

	if _, err := store.Migrate(ctx); err != nil {
		t.Fatalf("roll forward after the way back: %v", err)
	}
	if got, want := rowsAsText(t, store, branches), []string{"LEGION-208 legion/LEGION-208"}; strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("the branches of the settled heads after the roll-forward = %q, want %q", got, want)
	}
}
