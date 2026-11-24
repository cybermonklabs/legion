package store

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/plan"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// taskBaseVersion is the version of the migration this file tests, so a test can migrate through
// everything before it whatever lands beside it.
const taskBaseVersion = 20261123221142

// taskBaseRelease names the release whose way back the migration's header carries, as a failure to
// read that block names it.
const taskBaseRelease = "the release that keeps the base a task's start fixed"

// taskBaseIssue is the issue the tests of the migration seed.
const taskBaseIssue = "LEGION-230"

// seedTaskBaseRows writes, on a store migrated up to the migration before, taskBaseIssue at
// generation 2 and its plan as the release before ran it, each task with one implementer's step but
// r, whose one step is a reviewer's:
//   - s, the task no task needs, running, needs a and b, which are done, its pull request #30 open
//     on its branch legion/LEGION-230.s against main;
//   - p, running, needs q, which is done, with no pull request;
//   - j, running, needs a and b, with no pull request;
//   - h, held, needs q, its pull request #31 open on legion/LEGION-230.h against main;
//   - w, waiting, needs a;
//   - r, running, needs a and b, a reviewer's step alone.
//
// s, p, j, h and r are each needed by a task that is not done, so only s is a task no task needs.
func seedTaskBaseRows(t *testing.T, store *Store) {
	t.Helper()
	ctx := context.Background()
	at := time.Date(2026, 11, 23, 22, 0, 0, 0, time.UTC)
	key := taskBaseIssue
	v := record.PlanVersion{Issue: key, Generation: 2, Version: 1, ArtifactID: "plan-" + key, Source: record.PlanSourceOwner, GivenBy: "owner", GivenAt: at}
	type row struct {
		id, branch, role string
		needs            []string
	}
	rows := []row{
		{"a", "legion/LEGION-230", "implementer", nil}, {"b", "legion/LEGION-230.b", "implementer", nil},
		{"q", "legion/LEGION-230.q", "implementer", nil},
		{"s", "legion/LEGION-230.s", "implementer", []string{"a", "b"}}, {"p", "legion/LEGION-230.p", "implementer", []string{"q"}},
		{"j", "legion/LEGION-230.j", "implementer", []string{"a", "b"}}, {"h", "legion/LEGION-230.h", "implementer", []string{"q"}},
		{"w", "", "implementer", []string{"a"}}, {"r", "legion/LEGION-230.r", "reviewer", []string{"a", "b"}},
		{"z", "", "tester", []string{"p", "j", "h", "w", "r"}},
	}
	var tasks []record.PlanTask
	var steps []record.PlanStep
	for _, r := range rows {
		tasks = append(tasks, record.PlanTask{Issue: key, Generation: 2, Task: r.id, PlanVersion: 1, BlockText: "Task " + r.id + ".", Needs: r.needs, State: record.TaskWaiting})
		steps = append(steps, record.PlanStep{Issue: key, Generation: 2, Task: r.id, Position: 1, Role: r.role, Type: plan.BuiltinRoles()[r.role].Type, Agent: claim.Agent("agent-" + r.id)})
	}
	records := record.NewStore()
	if err := pgx.BeginFunc(ctx, store.Pool(), func(tx pgx.Tx) error {
		if err := records.PutIssue(ctx, tx, record.Issue{Key: key, Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion", Tree: key,
			Project: "LEGION", Title: key, Phase: "implementing", Generation: 2, Status: "in_progress", Rank: "U"}); err != nil {
			return err
		}
		return records.PutPlan(ctx, tx, v, tasks, steps)
	}); err != nil {
		t.Fatalf("seed the plan of %s: %v", key, err)
	}
	for _, r := range rows {
		state := map[string]string{"a": "done", "b": "done", "q": "done", "w": "waiting", "z": "waiting", "h": "held"}[r.id]
		if state == "" {
			state = "running"
		}
		place, position := "step", 1
		if state == "waiting" {
			place, position = "", 0
		}
		if _, err := store.Pool().Exec(ctx, `update plan_tasks set state = $3, running_kind = $4, position = $5, branch = $6
			where issue = $1 and task = $2`, key, r.id, state, place, position, r.branch); err != nil {
			t.Fatalf("stand task %s of %s %s: %v", r.id, key, state, err)
		}
	}
	if err := pgx.BeginFunc(ctx, store.Pool(), func(tx pgx.Tx) error {
		for _, pr := range []struct {
			number int
			branch string
		}{{30, "legion/LEGION-230.s"}, {31, "legion/LEGION-230.h"}} {
			if err := records.PutPullRequest(ctx, tx, record.PullRequest{Issue: key, Repo: "sjawhar/legion", Number: pr.number, Branch: pr.branch, Base: "main",
				HeadSHA: "head", State: record.PullRequestOpen, Failing: []string{}}); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("record the pull requests of %s: %v", key, err)
	}
}

// otherColumns reads every plan_tasks column of taskBaseIssue but base, each row as text, in task
// order: what the migration must leave as it found it.
const otherColumns = `select row(issue, generation, task, state, running_kind, position, branch, needs, block_text, hold_reason, step_wait)::text
	from plan_tasks where issue = 'LEGION-230' order by task`

// 20261123221142 adds plan_tasks.base, the base a task's start fixed, and writes it for each task
// that runs at the upgrade: the base of its open pull request where one is recorded, else the base
// the release before gave it. s reads main, its pull request's; p, q's branch; j, its join branch;
// h, main, its pull request's, where the plan would give it q's branch (a version under the release
// before moved its base within its run); w, which waits, ”; and r, a reviewer's task, main. For s,
// p, j and r that is the base the plan gives over the rows (record.StoredPlan.PlannedBase). Each
// other column of each row is as the migration found it.
//
// The fault it catches is a task that runs at the upgrade with no stored base, whose run a later
// version can then move to a base its start never made (its control: the migration without its
// update, under which each base reads ”).
func TestTheTaskBaseMigrationWritesTheBaseOfEachRunningTask(t *testing.T) {
	ctx := context.Background()
	store := emptyStore(t)
	migrateThrough(t, store, taskBaseVersion-1)
	seedTaskBaseRows(t, store)
	before := rowsAsText(t, store, otherColumns)

	if _, err := store.Migrate(ctx); err != nil {
		t.Fatalf("migrate through the task base: %v", err)
	}
	want := []string{"a ", "b ", "h main", "j legion/LEGION-230.j.join", "p legion/LEGION-230.q", "q ", "r main", "s main", "w ", "z "}
	if got := rowsAsText(t, store, `select task || ' ' || base from plan_tasks where issue = 'LEGION-230' order by task`); !slices.Equal(got, want) {
		t.Fatalf("the bases after the migration = %q, want %q", got, want)
	}
	if after := rowsAsText(t, store, otherColumns); !slices.Equal(after, before) {
		t.Fatalf("the other columns after the migration = %q, want them as before, %q", after, before)
	}
	var stored *record.StoredPlan
	if err := pgx.BeginFunc(ctx, store.Pool(), func(tx pgx.Tx) error {
		var err error
		stored, err = record.NewStore().Plan(ctx, tx, taskBaseIssue, 2)
		return err
	}); err != nil || stored == nil {
		t.Fatalf("read the plan of %s after the migration: %v", taskBaseIssue, err)
	}
	issue := record.Issue{Key: taskBaseIssue, DefaultBranch: "main"}
	for i := range stored.Tasks {
		stored.Tasks[i].Base = ""
	}
	for _, task := range []string{"s", "p", "j", "r"} {
		planned, _, err := stored.PlannedBase(issue, task)
		got := rowsAsText(t, store, `select base from plan_tasks where issue = 'LEGION-230' and task = '`+task+`'`)
		if err != nil || len(got) != 1 || got[0] != planned {
			t.Fatalf("the base of %s after the migration = %q, want the base the plan gives over the rows, %q (%v)", task, got, planned, err)
		}
	}
}

// The way back of the task base, the last paragraph of the migration's header
// (migrations.WayBack), run as its one transaction, drops plan_tasks.base and the migration's
// version row, so the roll forward's Migrate applies the file again and writes each running task's
// base again. It also deletes each unsent pull-request-close-refused notice, a kind no earlier
// release decodes, and keeps a task-join-failed one, whose own migration's way back deletes it.
//
// The fault it catches is a notice an earlier release cannot decode left queued at a rollback,
// which fails every try of its row and the state of its project (its control: the way back without
// its delete, under which the row stays).
func TestTheWayBackOfTheTaskBaseDropsTheColumn(t *testing.T) {
	ctx := context.Background()
	store := emptyStore(t)
	migrateThrough(t, store, taskBaseVersion-1)
	seedTaskBaseRows(t, store)
	if _, err := store.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if err := pgx.BeginFunc(ctx, store.Pool(), func(tx pgx.Tx) error {
		for _, kind := range []string{"pull-request-close-refused", "task-join-failed"} {
			payload := `{"kind":"` + kind + `","role":"architect","reason":"a notice of ` + kind + `"}`
			if err := record.NewStore().Enqueue(ctx, tx, record.OutboxRow{Kind: record.OutboxKindNotice, Issue: taskBaseIssue, Payload: []byte(payload),
				NextAt: time.Date(2026, 11, 24, 9, 0, 0, 0, time.UTC)}); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("queue the two notices: %v", err)
	}

	if _, err := store.Pool().Exec(ctx, wayBackOf(t, taskBaseVersion, taskBaseRelease), pgx.QueryExecModeSimpleProtocol); err != nil {
		t.Fatalf("run the way back of the task base as its one transaction: %v", err)
	}
	const baseColumn = `select column_name::text from information_schema.columns
		where table_schema = current_schema() and table_name = 'plan_tasks' and column_name = 'base'`
	if got := rowsAsText(t, store, baseColumn); len(got) != 0 {
		t.Fatalf("plan_tasks after the way back holds %v, want no base", got)
	}
	var recorded bool
	if err := store.Pool().QueryRow(ctx, `select exists (select 1 from schema_version where version = $1)`, taskBaseVersion).Scan(&recorded); err != nil {
		t.Fatalf("read the version row after the way back: %v", err)
	}
	if recorded {
		t.Fatalf("schema_version still holds %d after the way back, so the roll forward would not apply it again", taskBaseVersion)
	}
	if got := rowsAsText(t, store, `select payload->>'kind' from outbox where kind = 'notice' order by id`); !slices.Equal(got, []string{"task-join-failed"}) {
		t.Fatalf("the notices queued after the way back are of the kinds %q, want the task-join-failed one alone", got)
	}

	applied, err := store.Migrate(ctx)
	if err != nil || applied != 1 {
		t.Fatalf("the roll forward applied %d migrations, %v; want 1, the task base again", applied, err)
	}
	if got := rowsAsText(t, store, `select base from plan_tasks where issue = 'LEGION-230' and task = 'j'`); !slices.Equal(got, []string{"legion/LEGION-230.j.join"}) {
		t.Fatalf("the base of j after the roll forward = %q, want its join branch again", got)
	}
}
