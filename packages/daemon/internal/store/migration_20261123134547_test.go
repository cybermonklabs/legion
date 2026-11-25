package store

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/sjawhar/legion/daemon/internal/store/migrations"
	"github.com/sjawhar/legion/daemon/internal/store/testdata"
)

// fixBesideStepsVersion is the version of the migration this file tests, so the test can migrate
// through everything before it whatever lands beside it, and fixBesideStepsRelease names the
// release whose way back the migration's header carries, as a failure to read that block names it.
const (
	fixBesideStepsVersion = migrations.FixBesideSteps
	fixBesideStepsRelease = "the release that starts a fix beside the other steps of a task"
)

// fixBesideStepsCommit is a head a fix starts from, and fixBesideStepsFixed the head it pushed.
const (
	fixBesideStepsCommit = "1111111111111111111111111111111111111111"
	fixBesideStepsFixed  = "2222222222222222222222222222222222222222"
)

// fixBesideStepsRow is one queued outbox row a test plants: its kind, its issue and its payload as
// JSON text.
type fixBesideStepsRow struct{ kind, issue, payload string }

// The rows the release before writes, queued before the migration: a step's record and a start.
var fixBesideStepsRowsBefore = []fixBesideStepsRow{
	{"step_record", "LEGION-208", `{"id":"0f6c4d2a-9b1e-5c3d-8e7f-a1b2c3d4e5f6","task":"default","step":4,"kind":"step",` +
		`"role":"reviewer","type":"review","agent":"legion-LEGION-208-reviewer","generation":1,"started_at":"2026-11-23T10:00:00Z"}`},
	{"supervise", "LEGION-208", `{"op":"start","tree":"LEGION-208","role":"reviewer","task":"Review the change.",` +
		`"generation":1,"phase":"reviewing","entry":1}`},
}

// The rows this release writes, queued after the migration, of the project that rolls back first
// (LEGION) and of another project on the same database (OTHER).
var fixBesideStepsRowsAfter = []fixBesideStepsRow{
	{"notice", "LEGION-208", `{"kind":"ci-failed","role":"architect","reason":"ci/test failed at ` + fixBesideStepsCommit + `"}`},
	{"controller_notice", "LEGION-208", `{"kind":"ci-failed","role":"architect","reason":"ci/test failed"}`},
	{"fix_job_read", "LEGION-208", `{"generation":1,"task":"default","fix":1,"check":"ci/test","head":"` + fixBesideStepsCommit + `"}`},
	{"supervise", "LEGION-208", `{"op":"start","tree":"LEGION-208","role":"implementer","task":"Fix ci/test.",` +
		`"generation":1,"phase":"implementing","entry":2,"fix":1,"fixFrom":"` + fixBesideStepsCommit + `"}`},
	{"supervise", "LEGION-208", `{"op":"start","tree":"LEGION-208","role":"reviewer","task":"Review again.",` +
		`"generation":1,"phase":"reviewing","entry":2,"moveTo":"` + fixBesideStepsFixed + `"}`},
	{"step_record", "LEGION-208", `{"id":"7a1b2c3d-4e5f-5a6b-8c7d-e8f9a0b1c2d3","task":"default","step":2,"kind":"step",` +
		`"role":"implementer","type":"implementation","agent":"legion-LEGION-208-implementer","generation":1,` +
		`"startCommit":"` + fixBesideStepsCommit + `","endCommit":"` + fixBesideStepsFixed + `","fixCheck":"ci/test"}`},
	{"notice", "OTHER-301", `{"kind":"ci-failed","role":"architect","reason":"ci/test failed"}`},
	{"fix_job_read", "OTHER-301", `{"generation":1,"task":"default","fix":1,"check":"ci/test","head":"` + fixBesideStepsCommit + `"}`},
	{"supervise", "OTHER-301", `{"op":"start","tree":"OTHER-301","role":"implementer","task":"Fix ci/test.",` +
		`"generation":1,"phase":"implementing","entry":2,"fix":1,"fixFrom":"` + fixBesideStepsCommit + `"}`},
	{"ci_news", "LEGION-208", `{"purpose":"state","role":"legion-legion-legion-208-reviewer","generation":1,"task":"default",` +
		`"repo":"acme/widgets","head":"` + fixBesideStepsCommit + `","reader":{"phase":"reviewing","entry":1},"checks":[],"body":"Read Dispatch."}`},
	{"beside_branches_delete", "OTHER-301", `{"generation":1,"task":"default","fix":1}`},
	{"update_ask", "LEGION-208", `{"role":"tester","round":"pod:9d3f0c7a","pod":"9d3f0c7a","text":"Push your work.",` +
		`"branch":"legion-beside/LEGION-208/1/default/fix-1/tester","ownFix":true,"fixReads":true}`},
}

// fixBesideStepsOwnedMerge is the merge forward a fix of LEGION-209's task owns, as this release
// records it on the pull request: the ask and one earlier landing name the fix, beside two ordinary
// landings; and fixBesideStepsOrdinaryMerge is the same record with neither mark, which the way
// back leaves, its landings in their order.
const (
	fixBesideStepsOwnedMerge = `{"head":"h1","base":"main","role":"implementer","phase":"reviewing","why":"merge-next",` +
		`"askedAt":"2026-11-24T12:00:00Z","fix":{"generation":1,"task":"default","number":1},"earlier":[{"head":"h0","landed":"l0"},` +
		`{"head":"h0b","landed":"l0b","fix":{"generation":1,"task":"default","number":1}},{"head":"h0c","landed":"l0c"}]}`
	fixBesideStepsOrdinaryMerge = `{"head":"h1","base":"main","role":"implementer","phase":"reviewing","why":"merge-next",` +
		`"askedAt":"2026-11-24T12:00:00Z","earlier":[{"head":"h0","landed":"l0"},{"head":"h0b","landed":"l0b"},{"head":"h0c","landed":"l0c"}]}`
)

// fixBesideStepsStore is a store at the migration before fixBesideStepsVersion holding what the
// release before writes, migrated through it, with this release's rows planted on it: what each
// case below runs the way back over.
func fixBesideStepsStore(t *testing.T) *Store {
	t.Helper()
	ctx := context.Background()
	store := emptyStore(t)
	migrateThrough(t, store, fixBesideStepsVersion-1)
	exec := func(what, statement string, args ...any) {
		t.Helper()
		if _, err := store.Pool().Exec(ctx, statement, args...); err != nil {
			t.Fatalf("%s: %v", what, err)
		}
	}
	// An issue with plan rows, at its review, with the reviewer's pending delivery and the count
	// of the review's starts, as the release before writes them.
	exec("record the issues", `insert into issues (key, tree, project, title, phase, held_from, hold_reason, generation, status, rank, last_dispatch_seq)
		values ('LEGION-208', 'LEGION-208', 'LEGION', 'root', 'reviewing', null, null, 1, 'needs_review', 'A', 1),
			('LEGION-209', 'LEGION-209', 'LEGION', 'held for its fix', 'held', 'reviewing', null, 1, 'needs_review', 'B', 1),
			('LEGION-210', 'LEGION-210', 'LEGION', 'held for its checks', 'held', 'retro', null, 1, 'needs_review', 'C', 1),
			('LEGION-211', 'LEGION-211', 'LEGION', 'escalated', 'held', 'testing', 'escalated', 1, 'needs_review', 'D', 1),
			('OTHER-301', 'OTHER-301', 'OTHER', 'another project', 'reviewing', null, null, 1, 'needs_review', 'A', 1)`)
	exec("record the plan", `insert into issue_plans (issue, generation, version, source, artifact_id, given_by, given_at)
		values ('LEGION-208', 1, 0, 'default', '', '', now())`)
	exec("record the task", `insert into plan_tasks (issue, generation, task, plan_version, block_text, state, running_kind, position)
		values ('LEGION-208', 1, 'default', 0, '', 'running', 'step', 4)`)
	exec("record the steps", `insert into plan_steps (issue, generation, task, position, role, type)
		values ('LEGION-208', 1, 'default', 2, 'implementer', 'implementation'), ('LEGION-208', 1, 'default', 4, 'reviewer', 'review')`)
	exec("record the claims", `insert into claims (token, project, tree, issue, role, generation, session, session_file, state,
		launch_failures, prompt_failures, prompt_retires, uncertain_streak)
		values ('legion-LEGION-208-reviewer', 'legion', 'LEGION-208', 'LEGION-208', 'reviewer', 1, 'ses_r', '', 'working', 0, 0, 0, 0),
			('legion-LEGION-208-implementer', 'legion', 'LEGION-208', 'LEGION-208', 'implementer', 1, 'ses_i', '', 'idle', 0, 0, 0, 0)`)
	exec("record the reviewer's delivery", `insert into pending_task_deliveries (claim_token, delivery_id, task, phase, generation, queued_at)
		values ('legion-LEGION-208-reviewer', 'outbox:7', 'Review the change.', 'reviewing', 1, now())`)
	exec("record the review's start", `insert into step_starts (issue, task, kind, place, starts, agent, started_at, run, record)
		values ('LEGION-208', 'default', 'step', 4, 1, 'legion-LEGION-208-reviewer', now(), 'open', $1::jsonb)`,
		fixBesideStepsRowsBefore[0].payload)
	exec("record a released workspace's bundle", `insert into workspace_bundles (issue, bundle, size_bytes, at, head, release_id, source_pod_uid)
		values ('LEGION-208', 'PACK'::bytea, 4, now(), $1, 3, 'pod-released')`, fixBesideStepsCommit)
	queue := func(row fixBesideStepsRow) {
		t.Helper()
		exec("queue a "+row.kind+" row of "+row.issue, `insert into outbox (kind, issue, payload, attempts, next_at, last_error)
			values ($1, $2, $3::jsonb, 0, now(), '')`, row.kind, row.issue, row.payload)
	}
	for _, row := range fixBesideStepsRowsBefore {
		// The release before reads the rows it writes: what shows the frozen decoders take a row
		// of that release, so a refusal after the way back is the row's and not the decoder's.
		if err := testdata.DecodeBeforeFix(row.kind, []byte(row.payload)); err != nil {
			t.Fatalf("the release before's decode of its own %s row = %v, want it taken", row.kind, err)
		}
		queue(row)
	}
	const outbox = `select row(kind, issue, payload)::text from outbox order by id`
	before := rowsAsText(t, store, outbox)

	if _, err := store.Migrate(ctx); err != nil {
		t.Fatalf("migrate through %d: %v", fixBesideStepsVersion, err)
	}
	for _, read := range []struct{ what, query, want string }{
		{"the existing task's fixes_ci", `select fixes_ci::text from plan_tasks where issue = 'LEGION-208'`, "false"},
		{"the fixes", `select count(*)::text from task_fixes`, "0"},
		{"the CI failures", `select count(*)::text from ci_failures`, "0"},
		{"the review's start", `select row(start_commit, end_commit, fix_check)::text from step_starts where issue = 'LEGION-208'`, `("","","")`},
		{"the reviewer's delivery", `select row(fix, fix_from, move_to)::text from pending_task_deliveries`, `(0,"","")`},
		{"the saved bundle's fix copy", `select fix_head from workspace_bundles`, ""},
	} {
		if got := rowsAsText(t, store, read.query); !reflect.DeepEqual(got, []string{read.want}) {
			t.Fatalf("%s after the migration = %v, want %s", read.what, got, read.want)
		}
	}
	if after := rowsAsText(t, store, outbox); !reflect.DeepEqual(after, before) {
		t.Fatalf("the queued rows after the migration = %v, want them as the release before left them: %v", after, before)
	}

	// This release's rows: each kind of row the way back deletes or strips, and a hold of each new
	// reason.
	for _, row := range fixBesideStepsRowsAfter {
		queue(row)
	}
	exec("mark the task", `update plan_tasks set fixes_ci = true where issue = 'LEGION-208'`)
	exec("record the fix", `insert into task_fixes (issue, generation, task, fix, state, check_id, head, place, entry, started_at)
		values ('LEGION-208', 1, 'default', 1, 'running', 'ci/test', $1, 2, 2, now())`, fixBesideStepsCommit)
	exec("record the failure", `insert into ci_failures (issue, generation, task, check_id, head, went_to, fix, at)
		values ('LEGION-208', 1, 'default', 'ci/test', $1, 'fix', 1, now())`, fixBesideStepsCommit)
	exec("record the fix's delivery", `insert into pending_task_deliveries (claim_token, delivery_id, task, phase, generation, queued_at, fix, fix_from, fix_branch)
		values ('legion-LEGION-208-implementer', 'outbox:9', 'Fix ci/test.', 'implementing', 1, now(), 1, $1, 'legion-beside/LEGION-208/1/default/fix-1/implementer')`, fixBesideStepsCommit)
	exec("record the merge forward a fix owns", `insert into pull_requests (issue, repo, number, branch, head_sha, head_updated_at,
		failing, fix_attempts, blocked_attempts, check_runs, generation, snapshot, head_counted, state, mergeability, merge_sha, merge_forward)
		values ('LEGION-209', 'acme/widgets', 209, 'legion/LEGION-209', 'h1', now(), '[]', 0, 0, '[]', 1, '', '', 'open', 'MERGEABLE', '', $1::jsonb)`,
		fixBesideStepsOwnedMerge)
	exec("record the commits of the review's start", `update step_starts set start_commit = $1,
		record = record || jsonb_build_object('startCommit', $1::text, 'fixCheck', 'ci/test') where issue = 'LEGION-208'`, fixBesideStepsCommit)
	exec("hold for the fix and for the checks", `update issues set hold_reason = case key when 'LEGION-209' then 'fix' else 'ci' end
		where key in ('LEGION-209', 'LEGION-210')`)
	exec("record the fix's working copy in the saved bundle", `update workspace_bundles set fix_head = $1`, fixBesideStepsFixed)
	return store
}

// runFixBesideStepsWayBack runs block, a way back of fixBesideStepsVersion, as each project's operator
// runs it: once with <PROJECT> LEGION, then once with OTHER, each as one exec, as psql sends it.
func runFixBesideStepsWayBack(t *testing.T, store *Store, block string) {
	t.Helper()
	for _, project := range []string{"LEGION", "OTHER"} {
		if _, err := store.Pool().Exec(context.Background(), strings.ReplaceAll(block, "<PROJECT>", project), pgx.QueryExecModeSimpleProtocol); err != nil {
			t.Fatalf("run the way back of %s for %s: %v", fixBesideStepsRelease, project, err)
		}
	}
}

// queuedRows is each queued outbox row's kind and payload, in the order they were queued.
func queuedRows(t *testing.T, store *Store) []fixBesideStepsRow {
	t.Helper()
	rows, err := store.Pool().Query(t.Context(), `select kind, issue, payload::text from outbox order by id`)
	if err != nil {
		t.Fatalf("read the queued rows: %v", err)
	}
	queued, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (fixBesideStepsRow, error) {
		var r fixBesideStepsRow
		err := row.Scan(&r.kind, &r.issue, &r.payload)
		return r, err
	})
	if err != nil {
		t.Fatalf("read the queued rows: %v", err)
	}
	return queued
}

// The migration fixBesideStepsVersion adds what a fix beside the other steps of a task is recorded
// in: plan_tasks.fixes_ci, the tables task_fixes and ci_failures, the commits and the check of each
// step's start (step_starts), the fix and the copies of a pending delivery
// (pending_task_deliveries), the fix's working copy of a saved bundle (workspace_bundles), and the
// outbox kind fix_job_read. Over a store the release before migrated, it leaves each row it finds
// as it stands: the task reads fixes_ci false, the tables are empty, the saved bundle names no
// fix's working copy, and every queued row is the row the release before queued.
//
// The way back, the last paragraph of the migration's header (migrations.WayBack), run once for
// each project that shares the database, leaves only rows the release before reads: the frozen
// decoders of that release (testdata.DecodeBeforeFix) take each queued row left and the record of
// the review's start, the hold of each new reason is a plain hold, no pending delivery names a
// fix, the saved bundle is kept for the release before to restore, and the schema is the release
// before's, its kind check among it. The roll-forward's Migrate then applies the migration again.
//
// The control is a kept subtest: the same way back without its delete of the ci-failed notices
// leaves notices the frozen notice decoder refuses, so the decoders can see the rows the way back
// exists for.
func TestMigrationFixBesideStepsKeepsEachRowAndItsWayBack(t *testing.T) {
	ctx := context.Background()
	t.Run("the way back leaves only what the release before reads", func(t *testing.T) {
		store := fixBesideStepsStore(t)
		runFixBesideStepsWayBack(t, store, wayBackOf(t, fixBesideStepsVersion, fixBesideStepsRelease))

		queued := queuedRows(t, store)
		for _, row := range queued {
			if err := testdata.DecodeBeforeFix(row.kind, []byte(row.payload)); err != nil {
				t.Errorf("the release before refuses the queued %s row of %s left by the way back: %v\n%s", row.kind, row.issue, err, row.payload)
			}
		}
		if got := len(queued); got != 5 {
			t.Errorf("queued rows after the way back = %d, want 5: the release before's two, the start whose moveTo it took off, the record whose commits it took off and the update ask whose push it took off: %v", got, queued)
		}
		records := rowsAsText(t, store, `select record::text from step_starts where issue = 'LEGION-208'`)
		if len(records) != 1 {
			t.Fatalf("the review's start after the way back = %v, want its one row", records)
		}
		if err := testdata.DecodeBeforeFix("step_record", []byte(records[0])); err != nil {
			t.Errorf("the release before refuses the record of the review's start left by the way back: %v\n%s", err, records[0])
		}
		for _, read := range []struct {
			what, query string
			want        []string
		}{
			{"the pending deliveries", `select claim_token from pending_task_deliveries`, []string{"legion-LEGION-208-reviewer"}},
			{"the saved bundles", `select issue from workspace_bundles`, []string{"LEGION-208"}},
			{"the holds", `select row(key, held_from, hold_reason)::text from issues where phase = 'held' order by key`,
				[]string{"(LEGION-209,reviewing,)", "(LEGION-210,retro,)", "(LEGION-211,testing,escalated)"}},
			{"the columns of this release", `select count(*)::text from information_schema.columns
				where table_schema = current_schema() and (table_name, column_name) in (('plan_tasks', 'fixes_ci'),
				('step_starts', 'start_commit'), ('step_starts', 'end_commit'), ('step_starts', 'fix_check'),
				('step_starts', 'beside_fix'), ('step_starts', 'push_branch'),
				('pending_task_deliveries', 'fix'), ('pending_task_deliveries', 'fix_from'), ('pending_task_deliveries', 'move_to'),
				('pending_task_deliveries', 'fix_branch'), ('workspace_bundles', 'fix_head'))`,
				[]string{"0"}},
			{"the tables of this release", `select count(*)::text from information_schema.tables
				where table_schema = current_schema() and table_name in ('task_fixes', 'ci_failures')`, []string{"0"}},
			{"the migration's version row", fmt.Sprintf(`select count(*)::text from schema_version where version = %d`, fixBesideStepsVersion),
				[]string{"0"}},
		} {
			if got := rowsAsText(t, store, read.query); !reflect.DeepEqual(got, read.want) {
				t.Errorf("%s after the way back = %v, want %v", read.what, got, read.want)
			}
		}
		_, err := store.Pool().Exec(ctx, `insert into outbox (kind, issue, payload, attempts, next_at, last_error)
			values ('fix_job_read', 'LEGION-208', '{}', 0, now(), '')`)
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != "23514" || pgErr.ConstraintName != "outbox_kind_check" {
			t.Errorf("a fix_job_read row after the way back = %v, want the release before's kind check to refuse it", err)
		}

		if _, err := store.Migrate(ctx); err != nil {
			t.Fatalf("roll forward after the way back: %v", err)
		}
		if got := rowsAsText(t, store, `select fixes_ci::text from plan_tasks where issue = 'LEGION-208'`); !reflect.DeepEqual(got, []string{"false"}) {
			t.Errorf("the task's fixes_ci after the roll forward = %v, want false: the migration applied again", got)
		}
	})

	t.Run("control: without its delete of the ci-failed notices the way back leaves notices the release before refuses", func(t *testing.T) {
		block := wayBackOf(t, fixBesideStepsVersion, fixBesideStepsRelease)
		var kept []string
		for _, line := range strings.Split(block, "\n") {
			if !strings.Contains(line, "'ci-failed'") {
				kept = append(kept, line)
			}
		}
		if len(kept) == len(strings.Split(block, "\n")) {
			t.Fatalf("the way back holds no line naming 'ci-failed' to take out:\n%s", block)
		}
		store := fixBesideStepsStore(t)
		runFixBesideStepsWayBack(t, store, strings.Join(kept, "\n"))
		refused := 0
		for _, row := range queuedRows(t, store) {
			err := testdata.DecodeBeforeFix(row.kind, []byte(row.payload))
			if strings.Contains(row.payload, `"ci-failed"`) {
				if err == nil || !strings.Contains(err.Error(), `notice kind "ci-failed"`) {
					t.Errorf("the release before's decode of the %s row of %s left = %v, want it refused as an unknown notice kind", row.kind, row.issue, err)
				}
				refused++
			} else if err != nil {
				t.Errorf("the release before refuses the queued %s row of %s: %v", row.kind, row.issue, err)
			}
		}
		if refused != 3 {
			t.Errorf("ci-failed notices left by the way back without their delete = %d, want the 3 queued", refused)
		}
	})

	// The amendments' rows: a ci_news row, a beside_branches_delete row of another project, an
	// update ask naming a push, a pending delivery naming the fix's branch and a bundle naming the
	// fix's working copy. The release before refuses the first three; after the way back neither
	// new kind is left in any project, the ask decodes, the delivery is gone and the bundle stands.
	t.Run("the amendments' rows", func(t *testing.T) {
		store := fixBesideStepsStore(t)
		planted := rowsAsText(t, store, `select ((select count(*) from outbox where kind in ('ci_news', 'beside_branches_delete'))
			+ (select count(*) from outbox where kind = 'update_ask' and payload ?| array['branch', 'ownFix', 'fixReads'])
			+ (select count(*) from pending_task_deliveries where fix_branch <> '')
			+ (select count(*) from workspace_bundles where fix_head <> ''))::text`)
		if !reflect.DeepEqual(planted, []string{"5"}) {
			t.Fatalf("UNKNOWN: the amendments' planted objects = %v, want 5: a way back over fewer proves nothing of the rest", planted)
		}
		refused := 0
		for _, row := range queuedRows(t, store) {
			if row.kind == "ci_news" || row.kind == "beside_branches_delete" || row.kind == "update_ask" {
				if err := testdata.DecodeBeforeFix(row.kind, []byte(row.payload)); err == nil {
					t.Errorf("the release before takes the %s row of %s before the way back, want it refused: %s", row.kind, row.issue, row.payload)
				}
				refused++
			}
		}
		if refused != 3 {
			t.Fatalf("the amendments' rows the release before refuses = %d, want 3", refused)
		}
		runFixBesideStepsWayBack(t, store, wayBackOf(t, fixBesideStepsVersion, fixBesideStepsRelease))
		for _, read := range []struct {
			what, query string
			want        []string
		}{
			{"the rows of the new kinds", `select count(*)::text from outbox where kind in ('ci_news', 'beside_branches_delete')`, []string{"0"}},
			{"the update asks", `select count(*)::text from outbox where kind = 'update_ask'`, []string{"1"}},
			{"the deliveries of a fix", `select count(*)::text from pending_task_deliveries where claim_token = 'legion-LEGION-208-implementer'`, []string{"0"}},
			{"the saved bundles", `select issue from workspace_bundles`, []string{"LEGION-208"}},
		} {
			if got := rowsAsText(t, store, read.query); !reflect.DeepEqual(got, read.want) {
				t.Errorf("%s after the way back = %v, want %v", read.what, got, read.want)
			}
		}
		for _, row := range queuedRows(t, store) {
			if row.kind == "update_ask" {
				if err := testdata.DecodeBeforeFix(row.kind, []byte(row.payload)); err != nil {
					t.Errorf("the release before refuses the update ask left by the way back: %v\n%s", err, row.payload)
				}
			}
		}
	})

	t.Run("control: without its delete of the ci_news rows the way back leaves a row the release before refuses", func(t *testing.T) {
		block := wayBackOf(t, fixBesideStepsVersion, fixBesideStepsRelease)
		edited := strings.Replace(block, "kind in ('ci_news', 'beside_branches_delete')", "kind in ('beside_branches_delete')", 1)
		if edited == block {
			t.Fatalf("the way back holds no delete of the ci_news rows to take out:\n%s", block)
		}
		// The kind check the block sets back would refuse the row it leaves, and fail the block;
		// without it the row is left for the release before's decoder to meet.
		var kept []string
		for _, line := range strings.Split(edited, "\n") {
			if !strings.Contains(line, "outbox_kind_check") {
				kept = append(kept, line)
			}
		}
		store := fixBesideStepsStore(t)
		runFixBesideStepsWayBack(t, store, strings.Join(kept, "\n"))
		left := 0
		for _, row := range queuedRows(t, store) {
			if row.kind != "ci_news" {
				continue
			}
			left++
			if err := testdata.DecodeBeforeFix(row.kind, []byte(row.payload)); err == nil || !strings.Contains(err.Error(), `unknown kind "ci_news"`) {
				t.Errorf("the release before's decode of the ci_news row left = %v, want it refused as an unknown kind", err)
			}
		}
		if left != 1 {
			t.Errorf("ci_news rows left by the way back without their delete = %d, want the 1 queued", left)
		}
	})

	// A merge forward a fix owns names the fix on the ask and on an earlier landing; the release
	// before has no fix, so the way back takes both marks off and leaves every other member, each
	// ordinary landing in its order.
	ownership := `select (merge_forward ? 'fix' or jsonb_path_exists(merge_forward, '$.earlier[*].fix'))::text from pull_requests where issue = 'LEGION-209'`
	t.Run("the way back takes the fix's ownership off the merge forward", func(t *testing.T) {
		store := fixBesideStepsStore(t)
		if got := rowsAsText(t, store, ownership); !reflect.DeepEqual(got, []string{"true"}) {
			t.Fatalf("UNKNOWN: the planted merge forward names a fix = %v, want true", got)
		}
		runFixBesideStepsWayBack(t, store, wayBackOf(t, fixBesideStepsVersion, fixBesideStepsRelease))
		if got := rowsAsText(t, store, ownership); !reflect.DeepEqual(got, []string{"false"}) {
			t.Errorf("the merge forward after the way back names a fix = %v, want false", got)
		}
		if got := rowsAsText(t, store, `select (merge_forward = '`+fixBesideStepsOrdinaryMerge+`'::jsonb)::text from pull_requests where issue = 'LEGION-209'`); !reflect.DeepEqual(got, []string{"true"}) {
			t.Errorf("the merge forward after the way back = %v, want the ordinary record with its landings in order: %s", rowsAsText(t, store,
				`select merge_forward::text from pull_requests where issue = 'LEGION-209'`), fixBesideStepsOrdinaryMerge)
		}
	})
	t.Run("control: without its update of the merge forward the way back leaves the fix's marks", func(t *testing.T) {
		block := wayBackOf(t, fixBesideStepsVersion, fixBesideStepsRelease)
		var kept []string
		for _, line := range strings.Split(block, "\n") {
			if !strings.Contains(line, "merge_forward") {
				kept = append(kept, line)
			}
		}
		store := fixBesideStepsStore(t)
		runFixBesideStepsWayBack(t, store, strings.Join(kept, "\n"))
		if got := rowsAsText(t, store, ownership); !reflect.DeepEqual(got, []string{"true"}) {
			t.Errorf("the merge forward after the way back without its update names a fix = %v, want true: the marks the update takes off", got)
		}
	})
}
