package store

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// doneByChecksVersion is the version of the migration this file tests, so each test can migrate
// through everything before it whatever lands beside it.
const doneByChecksVersion = 20261125004932

// doneByChecksRelease names the release whose way back the migration's header carries, as a
// failure to read that block names it.
const doneByChecksRelease = "the release that closes an issue when the checks of its goal pass"

// doneByChecksWayBack is the migration's way back as its header's last paragraph carries it, cut in
// its two blocks: block A, which runs once for each project with <PROJECT> its key, and block B,
// which runs once after it. Each is one transaction, opened by begin and closed by commit. A block
// of another shape stops the test by name.
func doneByChecksWayBack(t *testing.T) (blockA, blockB string) {
	t.Helper()
	block := wayBackOf(t, doneByChecksVersion, doneByChecksRelease)
	blockA, blockB, found := strings.Cut(block, "\n-- Block B:")
	if !found || !strings.HasPrefix(blockA, "-- Block A:") {
		t.Fatalf("the way back of %s is not block A then block B:\n%s", doneByChecksRelease, block)
	}
	blockB = "-- Block B:" + blockB
	for name, text := range map[string]string{"A": blockA, "B": blockB} {
		lines := strings.Split(strings.TrimSpace(text), "\n")
		if len(lines) < 3 || lines[1] != "begin;" || lines[len(lines)-1] != "commit;" {
			t.Fatalf("block %s of the way back of %s is not one transaction:\n%s", name, doneByChecksRelease, text)
		}
	}
	return blockA, blockB
}

// wayBackConn is a connection of its own to store's database that keeps each notice the server
// sends it, as psql prints them: block A prints its lists as notices.
type wayBackConn struct {
	conn    *pgx.Conn
	mu      sync.Mutex
	notices []string
}

func openWayBackConn(t *testing.T, store *Store) *wayBackConn {
	t.Helper()
	w := &wayBackConn{}
	config := store.Pool().Config().ConnConfig.Copy()
	config.OnNotice = func(_ *pgconn.PgConn, notice *pgconn.Notice) {
		w.mu.Lock()
		defer w.mu.Unlock()
		w.notices = append(w.notices, notice.Message)
	}
	conn, err := pgx.ConnectConfig(t.Context(), config)
	if err != nil {
		t.Fatalf("open a connection for the way back: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close(context.Background()) })
	w.conn = conn
	return w
}

// run runs block as an operator runs it in psql, with <PROJECT> replaced by project where it is
// block A, and answers the notices it printed. A block that fails leaves its transaction rolled
// back, as psql leaves it once the operator ends the failed transaction.
func (w *wayBackConn) run(t *testing.T, block, project string) ([]string, error) {
	t.Helper()
	w.mu.Lock()
	w.notices = nil
	w.mu.Unlock()
	_, err := w.conn.Exec(t.Context(), strings.ReplaceAll(block, "<PROJECT>", project))
	if err != nil {
		if _, rollback := w.conn.Exec(t.Context(), "rollback"); rollback != nil {
			t.Fatalf("end the failed way back's transaction: %v", rollback)
		}
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	return slices.Clone(w.notices), err
}

// schemaShape is what a daemon reads of a schema: each column with its type, nullability and
// default, each constraint, each function with its definition, each index, and the versions
// recorded, each as one line, sorted.
func schemaShape(t *testing.T, store *Store) []string {
	t.Helper()
	var shape []string
	for _, query := range []string{
		`select 'column ' || table_name || '.' || column_name || ' ' || data_type || ' ' || is_nullable || ' ' || coalesce(column_default, '')
			from information_schema.columns where table_schema = current_schema()`,
		`select 'constraint ' || conrelid::regclass::text || '.' || conname || ' ' || pg_get_constraintdef(oid)
			from pg_constraint where connamespace = current_schema()::regnamespace`,
		`select 'function ' || p.proname || '(' || pg_get_function_identity_arguments(p.oid) || ') ' || pg_get_functiondef(p.oid)
			from pg_proc p where p.pronamespace = current_schema()::regnamespace`,
		`select 'index ' || indexname || ' ' || indexdef from pg_indexes where schemaname = current_schema()`,
		`select 'version ' || version::text from schema_version`,
	} {
		shape = append(shape, rowsAsText(t, store, query)...)
	}
	slices.Sort(shape)
	return shape
}

// doneByChecksIssue is the insert of one issue as this release writes it: the phase, the status,
// the generation, the tree's generation and the merge it names.
func doneByChecksIssue(key, tree, phase, status string, generation, treeGeneration int) string {
	return fmt.Sprintf(`insert into issues (key, tree, project, title, phase, generation, status, rank, last_dispatch_seq,
		tree_generation, merge_number, merge_sha, close_wait_since)
		values ('%s', '%s', split_part('%s', '-', 1), '%s', '%s', %d, '%s', '%s', 1, %d, 7, '%s', now())`,
		key, tree, key, key, phase, generation, status, key, treeGeneration, strings.Repeat("7", 40))
}

func execAll(t *testing.T, store *Store, statements ...string) {
	t.Helper()
	for _, statement := range statements {
		if _, err := store.Pool().Exec(t.Context(), statement); err != nil {
			t.Fatalf("%s: %v", statement, err)
		}
	}
}

// 20261125004932 adds the phase verifying and the records the close of an issue on its checks
// reads, and moves no row: every issue the upgrade finds, in each phase main writes and done, keeps
// its phase, its status and its generation. After it a write of verifying is taken, and before it
// refused, and a hold from verifying is refused after it as before, since no role works the phase.
// Each production check it finds reads generation 0, which binds no generation, and relook 0. Each
// issue reads its tree's generation as its own generation, and none of the wait's columns and no
// merge snapshot. A delivery it finds starts no working copy anywhere.
//
// The negative control is the migration without verifying in issues_phase_check: the write of
// verifying after it is refused, and this test fails at it.
func TestTheMigrationAddsVerifyingAndKeepsEveryRow(t *testing.T) {
	ctx := context.Background()
	store := emptyStore(t)
	migrateThrough(t, store, doneByChecksVersion-1)

	phases := map[string]string{
		"admitted": "todo", "awaiting_person": "in_progress", "planning": "in_progress", "implementing": "in_progress",
		"testing": "testing", "reviewing": "needs_review", "accepting": "needs_review", "retro": "retro", "merging": "retro",
		"done": "done", "held": "in_progress",
	}
	n := 0
	for _, at := range slices.Sorted(maps.Keys(phases)) {
		n++
		key := fmt.Sprintf("LEGION-%d", 300+n)
		columns, values := "", ""
		switch at {
		case "held":
			columns, values = ", held_from", ", 'implementing'"
		case "awaiting_person":
			columns, values = ", awaiting_ask", ", 'ask-1'"
		}
		execAll(t, store, fmt.Sprintf(`insert into issues (key, tree, project, title, phase, generation, status, rank, last_dispatch_seq%s)
			values ('%s', '%s', 'LEGION', '%s', '%s', %d, '%s', 'A', 1%s)`, columns, key, key, key, at, n, phases[at], values))
	}
	execAll(t, store, `insert into production_checks (issue, project, tree, repo, pr_number, merge_sha, opened_at, bound_at)
		values ('LEGION-301', 'LEGION', 'LEGION-301', 'acme/widgets', 12, 'd4e5f6', now(), now() + interval '336 hours')`)
	const kept = `select row(key, tree, phase, held_from, generation, status)::text from issues order by key`
	before := rowsAsText(t, store, kept)
	if len(before) != len(phases) {
		t.Fatalf("the store holds %d issues before the migration, want one in each of the %d phases main writes", len(before), len(phases))
	}
	verifying := `insert into issues (key, tree, project, title, phase, generation, status, rank, last_dispatch_seq)
		values ('LEGION-399', 'LEGION-399', 'LEGION', 'merged', 'verifying', 1, 'testing', 'A', 1)`
	if _, err := store.Pool().Exec(ctx, verifying); err == nil || !strings.Contains(err.Error(), "issues_phase_check") {
		t.Fatalf("writing verifying before the migration = %v, want issues_phase_check to refuse it", err)
	}

	if _, err := store.Migrate(ctx); err != nil {
		t.Fatalf("migrate through the done-by-checks migration: %v", err)
	}

	if after := rowsAsText(t, store, kept); !reflect.DeepEqual(after, before) {
		t.Fatalf("the issues after the migration = %v, want each as the release before left it: %v", after, before)
	}
	if _, err := store.Pool().Exec(ctx, verifying); err != nil {
		t.Fatalf("write verifying after the migration: %v", err)
	}
	if _, err := store.Pool().Exec(ctx, `update issues set phase = 'held', held_from = 'verifying' where key = 'LEGION-399'`); err == nil ||
		!strings.Contains(err.Error(), "issues_held_from_check") {
		t.Fatalf("holding an issue from verifying = %v, want issues_held_from_check to refuse it: no role works the phase", err)
	}
	if got := rowsAsText(t, store, `select issue || ' ' || generation || ' ' || relook_seq from production_checks`); !reflect.DeepEqual(got, []string{"LEGION-301 0 0"}) {
		t.Fatalf("the production checks after the migration = %v, want the one found at generation 0 and relook 0", got)
	}
	const fresh = `select key || ' ' || (tree_generation = generation)::text || ' ' ||
		num_nulls(merge_number, merge_head, merge_sha, merge_base, merge_repo, merged_at, close_wait_since, close_wait,
			closed_on_checks_at, next_change, standing_told_at) || ' ' || next_changes || ' ' || merger_completion_owed::text
		from issues where key <> 'LEGION-399' order by key`
	for _, row := range rowsAsText(t, store, fresh) {
		if !strings.HasSuffix(row, " true 11 0 false") {
			t.Errorf("issue row %q after the migration, want its tree's generation at its own generation, none of the snapshot's or the wait's columns, no next change and no completion owed", row)
		}
	}
	if _, err := store.Pool().Exec(ctx, "select start_at from pending_task_deliveries"); err != nil {
		t.Fatalf("read where a delivery starts its working copy after the migration: %v", err)
	}
}

// An issue an earlier daemon closed stays closed across the migration: a done issue whose
// production check is still open, and one whose production check recorded a fail, keep their
// phase, status and generation, and each check keeps every column it held. The migration queues
// nothing and records no reason of a next change for either.
func TestAReleaseLeavesEachClosedIssueClosed(t *testing.T) {
	ctx := context.Background()
	store := emptyStore(t)
	migrateThrough(t, store, doneByChecksVersion-1)
	execAll(t, store,
		`insert into issues (key, tree, project, title, phase, generation, status, rank, last_dispatch_seq, linger_until, dispatch_status)
			values ('LEGION-501', 'LEGION-501', 'LEGION', 'closed, its check open', 'done', 2, 'done', 'A', 1, now() + interval '1 hour', 'done')`,
		`insert into issues (key, tree, project, title, phase, generation, status, rank, last_dispatch_seq, dispatch_status)
			values ('LEGION-502', 'LEGION-502', 'LEGION', 'closed, its check failed', 'done', 1, 'done', 'B', 1, 'done')`,
		`insert into production_checks (issue, project, tree, repo, pr_number, merge_sha, opened_at, bound_at, looks)
			values ('LEGION-501', 'LEGION', 'LEGION-501', 'acme/widgets', 51, 'abc', now(), now() + interval '336 hours', 1)`,
		`insert into production_checks (issue, project, tree, repo, pr_number, merge_sha, opened_at, bound_at, looks, closed_at, outcome)
			values ('LEGION-502', 'LEGION', 'LEGION-502', 'acme/widgets', 52, 'def', now(), now() + interval '336 hours', 2, now(), 'defect')`,
	)
	const issues = `select row(key, tree, phase, generation, status, linger_until, held_from, dispatch_status)::text from issues order by key`
	const checks = `select row(issue, project, tree, repo, pr_number, merge_sha, opened_at, bound_at, role, looks, look_open, look_wake_at,
		last_wake_kind, last_wake_detail, last_wake_at, wake_at, last, closed_at, outcome)::text from production_checks order by issue`
	issuesBefore, checksBefore := rowsAsText(t, store, issues), rowsAsText(t, store, checks)

	if _, err := store.Migrate(ctx); err != nil {
		t.Fatalf("migrate through the done-by-checks migration: %v", err)
	}

	if got := rowsAsText(t, store, issues); !reflect.DeepEqual(got, issuesBefore) {
		t.Fatalf("the closed issues after the migration = %v, want them as they were: %v", got, issuesBefore)
	}
	if got := rowsAsText(t, store, checks); !reflect.DeepEqual(got, checksBefore) {
		t.Fatalf("the production checks after the migration = %v, want them as they were: %v", got, checksBefore)
	}
	if got := rowsAsText(t, store, `select (select count(*) from outbox)::text || ' ' || (select count(*) from defects)::text`); !reflect.DeepEqual(got, []string{"0 0"}) {
		t.Fatalf("outbox rows and defects after the migration = %v, want none of either", got)
	}
}

// seedTheWait is a store migrated to this release, holding what block A acts on for project: a
// root in verifying at generation 3 whose tree's generation is 2, since a next change raised its
// own, with an open defect and a withdrawn one, a child at work, the outbox rows of the kinds and
// fields the release before cannot decode, and a suspension, a release and a workspace check of the
// child that name the tree's generation 2.
func seedTheWait(t *testing.T, store *Store, project string) {
	t.Helper()
	root, child := project+"-1", project+"-2"
	notice := func(kind, noticeKind string) string {
		return fmt.Sprintf(`insert into outbox (kind, issue, payload, attempts, next_at, last_error)
			values ('%s', '%s', '{"kind": "%s"}', 0, now(), '')`, kind, root, noticeKind)
	}
	row := func(kind, issue, payload string) string {
		return fmt.Sprintf(`insert into outbox (kind, issue, payload, attempts, next_at, last_error)
			values ('%s', '%s', '%s', 0, now(), '')`, kind, issue, payload)
	}
	execAll(t, store,
		doneByChecksIssue(root, root, "verifying", "testing", 3, 2),
		doneByChecksIssue(child, root, "implementing", "in_progress", 1, 1),
		fmt.Sprintf(`insert into defects (issue, generation, kind, text, role, head, reported_at)
			values ('%s', 3, 'defect', 'the open defect', 'merger', '%s', now())`, root, strings.Repeat("b", 40)),
		fmt.Sprintf(`insert into defects (issue, generation, kind, text, role, head, reported_at, withdrawn_at, withdraw_reason)
			values ('%s', 3, 'defect', 'the withdrawn defect', 'merger', '%s', now(), now(), 'same as the open one')`, root, strings.Repeat("b", 40)),
		row("close_read", root, `{"generation": 3}`),
		row("close_wait_publish", root, `{"generation": 3}`),
		notice("notice", "close-waits"),
		notice("controller_notice", "closed-check-failed"),
		notice("notice", "worker-died"),
		row("issue_branch", root, `{"generation": 3, "from": {"base": "main", "mergedHead": "`+strings.Repeat("c", 40)+`"}}`),
		row("issue_branch", child, `{"generation": 1}`),
		row("supervise", root, `{"op": "start", "tree": "`+root+`", "role": "planner", "generation": 3, "startAt": {"bookmark": "legion/`+root+`", "base": "main"}}`),
		row("supervise", child, `{"op": "start", "tree": "`+root+`", "role": "implementer", "generation": 1}`),
		row("issue_suspend", child, `{"tree": "`+root+`", "generation": 1, "treeGeneration": 2}`),
		row("issue_release", child, `{"tree": "`+root+`", "generation": 1, "treeGeneration": 2}`),
		row("issue_bundle", child, `{"tree": "`+root+`", "generation": 1, "treeGeneration": 2, "role": "implementer"}`),
	)
}

// The way back of the release that closes an issue on its checks, run as its header carries it:
// block A for the one project, then block B. Block A prints the list a person keeps, the issue that
// stood in verifying and its open defect, moves that issue to merging with status retro, deletes
// the rows the release before cannot decode and keeps every other, and gives the child's
// suspension, release and workspace check the root's generation, the tree generation the release
// before reads. Block B leaves the store exactly as the release before migrated it, which is the
// store that release boots on: each column, constraint, function, index and version recorded. The
// release before applies every embedded migration but this one, those that sort above it among
// them. So a difference is what the block leaves of this migration, or what it takes of the release
// before.
// Before that, a delivery's copy in the release before's form is read through design_news_credits,
// and so through design_news_current, and counts as a credit, and step_run_witness answers for an
// issue: neither fails on the dropped tree_generation.
//
// The negative control is block B without its restore of the four function bodies: the read of
// the credit then fails at run time, on a function or a column the block dropped. A second is
// block B without its drop of pending_task_deliveries.start_at: the store then holds that column
// besides the release before's.
func TestTheWayBackOfDoneByChecks(t *testing.T) {
	store := emptyStore(t)
	if _, err := store.Migrate(t.Context()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	seedTheWait(t, store, "ACME")
	blockA, blockB := doneByChecksWayBack(t)
	conn := openWayBackConn(t, store)

	notices, err := conn.run(t, blockA, "ACME")
	if err != nil {
		t.Fatalf("run block A for ACME: %v", err)
	}
	if len(notices) != 2 || !strings.Contains(notices[0], `"key":"ACME-1"`) || !strings.Contains(notices[1], `"text":"the open defect"`) ||
		strings.Contains(notices[1], "the withdrawn defect") {
		t.Fatalf("block A printed %q, want the issue in verifying and then its open defect alone", notices)
	}
	if got := rowsAsText(t, store, `select key || ' ' || phase || ' ' || status from issues order by key`); !reflect.DeepEqual(got, []string{
		"ACME-1 merging retro", "ACME-2 implementing in_progress",
	}) {
		t.Fatalf("the issues after block A = %v, want the root in merging with status retro and the child as it was", got)
	}
	if got := rowsAsText(t, store, `select kind || ' ' || issue || ' ' || coalesce(payload->>'kind', '') || coalesce(payload->>'treeGeneration', '')
		from outbox order by id`); !reflect.DeepEqual(got, []string{
		"notice ACME-1 worker-died", "issue_branch ACME-2 ", "supervise ACME-2 ",
		"issue_suspend ACME-2 3", "issue_release ACME-2 3", "issue_bundle ACME-2 3",
	}) {
		t.Fatalf("the outbox after block A = %v, want the rows of the release before alone, each of the child's pod rows at the root's generation", got)
	}

	if _, err := conn.run(t, blockB, ""); err != nil {
		t.Fatalf("run block B: %v", err)
	}
	carried := `{"items": [{"issue": "ACME-2", "audience": "holder", "step": "spec", "from": null, "to": 2, "generation": 1, "tree_generation": 3}]}`
	credits, err := store.Pool().Query(t.Context(), `select issue || ' ' || audience from design_news_credits($1::jsonb)`, carried)
	if err != nil {
		t.Fatalf("read a delivery's copy of the release before through design_news_credits after the way back: %v", err)
	}
	counted, err := pgx.CollectRows(credits, pgx.RowTo[string])
	if err != nil {
		t.Fatalf("read a delivery's copy of the release before through design_news_credits after the way back: %v", err)
	}
	if !reflect.DeepEqual(counted, []string{"ACME-2 holder"}) {
		t.Fatalf("the credits of the copy after the way back = %v, want its one item counted: it was rendered in the run the record holds", counted)
	}
	if got := rowsAsText(t, store, `select coalesce(step_run_witness('ACME-2', 'ACME-1')->>'root_generation', 'none')`); !reflect.DeepEqual(got, []string{"3"}) {
		t.Fatalf("step_run_witness of the child after the way back names the root generation %v, want the root's 3", got)
	}

	before := emptyStore(t)
	migrateAllBut(t, before, doneByChecksVersion)
	if got, want := schemaShape(t, store), schemaShape(t, before); !reflect.DeepEqual(got, want) {
		var extra, missing []string
		for _, line := range got {
			if !slices.Contains(want, line) {
				extra = append(extra, line)
			}
		}
		for _, line := range want {
			if !slices.Contains(got, line) {
				missing = append(missing, line)
			}
		}
		t.Fatalf("the store after the way back is not the store the release before migrated:\nit holds besides: %q\nit lacks: %q", extra, missing)
	}
}

// Block A runs once for each project in any order, and block B after the last. Each project
// rolled back first moves its own issue in verifying to merging and prints its own list, and leaves
// the other project's alone. Block B, run while another project's issue still stands in verifying,
// refuses with the words that say what to do, and changes nothing: the table of defects, the
// columns, the four-argument design_news_current and the migration's version all stand, and the
// other project's issue still stands in verifying. Once that project's block A ran, block B runs.
//
// The negative control is block B without its refusal: run first, it fails on issues_phase_check
// instead, with no words for the operator, and this test fails at the words it wants.
func TestTheWayBackRunsForEachProjectInAnyOrder(t *testing.T) {
	blockA, blockB := doneByChecksWayBack(t)
	seeded := func(t *testing.T) (*Store, *wayBackConn) {
		store := emptyStore(t)
		if _, err := store.Migrate(t.Context()); err != nil {
			t.Fatalf("migrate: %v", err)
		}
		seedTheWait(t, store, "ACME")
		seedTheWait(t, store, "OTHER")
		return store, openWayBackConn(t, store)
	}
	phases := func(t *testing.T, store *Store) []string {
		return rowsAsText(t, store, `select key || ' ' || phase from issues where key like '%-1' order by key`)
	}

	t.Run("the second project first", func(t *testing.T) {
		store, conn := seeded(t)
		for _, step := range []struct {
			project string
			want    []string
		}{
			{"OTHER", []string{"ACME-1 verifying", "OTHER-1 merging"}},
			{"ACME", []string{"ACME-1 merging", "OTHER-1 merging"}},
		} {
			notices, err := conn.run(t, blockA, step.project)
			if err != nil {
				t.Fatalf("run block A for %s: %v", step.project, err)
			}
			if len(notices) != 2 || !strings.Contains(notices[0], `"key":"`+step.project+`-1"`) || !strings.Contains(notices[1], `"issue":"`+step.project+`-1"`) {
				t.Fatalf("block A for %s printed %q, want that project's issue in verifying and its open defect", step.project, notices)
			}
			if strings.Contains(strings.Join(notices, "\n"), map[string]string{"OTHER": "ACME-1", "ACME": "OTHER-1"}[step.project]) {
				t.Fatalf("block A for %s printed %q, which names the other project's issue", step.project, notices)
			}
			if got := phases(t, store); !reflect.DeepEqual(got, step.want) {
				t.Fatalf("the roots after block A for %s = %v, want %v", step.project, got, step.want)
			}
		}
		if _, err := conn.run(t, blockB, ""); err != nil {
			t.Fatalf("run block B after block A ran for both projects: %v", err)
		}
		if got := rowsAsText(t, store, fmt.Sprintf(`select count(*)::text from schema_version where version = %d`, doneByChecksVersion)); !reflect.DeepEqual(got, []string{"0"}) {
			t.Fatalf("the migration's version rows after block B = %v, want none, so the roll forward applies it again", got)
		}
	})

	t.Run("block B before the second project's block A", func(t *testing.T) {
		store, conn := seeded(t)
		if _, err := conn.run(t, blockA, "ACME"); err != nil {
			t.Fatalf("run block A for ACME: %v", err)
		}
		before := schemaShape(t, store)
		_, err := conn.run(t, blockB, "")
		var refusal *pgconn.PgError
		if !errors.As(err, &refusal) || refusal.Message != "an issue still stands in verifying: run block A for its project first" {
			t.Fatalf("block B while OTHER-1 stands in verifying = %v, want its refusal, which says to run block A for that project first", err)
		}
		if after := schemaShape(t, store); !reflect.DeepEqual(after, before) {
			t.Fatalf("the refused block B changed the schema")
		}
		if got := phases(t, store); !reflect.DeepEqual(got, []string{"ACME-1 merging", "OTHER-1 verifying"}) {
			t.Fatalf("the roots after the refused block B = %v, want OTHER-1 still in verifying", got)
		}
		if _, err := conn.run(t, blockA, "OTHER"); err != nil {
			t.Fatalf("run block A for OTHER: %v", err)
		}
		if _, err := conn.run(t, blockB, ""); err != nil {
			t.Fatalf("run block B once block A ran for OTHER: %v", err)
		}
		// Block A run again after block B reads no table and no column it dropped.
		if notices, err := conn.run(t, blockA, "ACME"); err != nil || len(notices) != 0 {
			t.Fatalf("block A again after block B = %q, %v, want nothing printed and no failure", notices, err)
		}
	})
}
