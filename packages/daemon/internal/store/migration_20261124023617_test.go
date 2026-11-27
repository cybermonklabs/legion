package store

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/sjawhar/legion/daemon/internal/record"
)

// subOwnersVersion is the version of the migration this file tests, so a test can migrate through
// everything before it whatever lands beside it.
const subOwnersVersion = 20261124023617

// refusedByCheck says whether err is the refusal of the check constraint named constraint (SQLSTATE
// 23514): a write the check refuses, not one that failed for another reason. A check added over a
// row it refuses names the constraint in its message, so the message counts too.
func refusedByCheck(err error, constraint string) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23514" &&
		(pgErr.ConstraintName == constraint || strings.Contains(pgErr.Message, `"`+constraint+`"`))
}

// subOwnersRows is the writes both tests of the migration make on store, for the issue LEGION-208
// at generation 1: a task standing at an act, and an outbox row of a kind.
type subOwnersRows struct {
	t     *testing.T
	store *Store
}

// exec runs sql on the store.
func (r subOwnersRows) exec(sql string, args ...any) error {
	_, err := r.store.Pool().Exec(context.Background(), sql, args...)
	return err
}

// plan records version 1 of the plan of LEGION-208 at generation 1, which each task row names.
func (r subOwnersRows) plan() {
	r.t.Helper()
	if err := r.exec(`insert into issue_plans (issue, generation, version, source, artifact_id, given_by, given_at)
		values ('LEGION-208', 1, 1, 'owner', 'artifact-208', 'legion-legion-legion-208-architect', now())`); err != nil {
		r.t.Fatalf("record the plan of LEGION-208: %v", err)
	}
}

// standAt writes task, a task with a sub-owner, running at the act of kind at position.
func (r subOwnersRows) standAt(task, kind string, position int) error {
	return r.exec(`insert into plan_tasks (issue, generation, task, plan_version, block_text, owner_role, state, running_kind, position)
		values ('LEGION-208', 1, $1, 1, 'Build the widget.', 'architect', 'running', $2, $3)`, task, kind, position)
}

// queue writes an outbox row of kind whose payload is payload.
func (r subOwnersRows) queue(kind, payload string) error {
	return r.exec(`insert into outbox (kind, issue, payload, attempts, next_at, last_error)
		values ($1, 'LEGION-208', $2, 0, now(), '')`, kind, payload)
}

// 20261124023617 lets a task stand at its parts and at its report (record.PlaceParts,
// record.PlaceReported), each at position 0, and keeps the parts whose join GitHub refused
// (record.PlanTask.PartsConflicts). Before it the store refuses a task at its parts, by the
// running kind's check (SQLSTATE 23514), named. After it such a task is taken, while a task at its
// parts at position 1 is still refused, by the place check, and so is an act of no kind. The
// outbox check is still the one 20261123072431 wrote: every kind it named is queued, and a row of
// parts_join, a kind no executor runs, is refused, as is a row of no kind. A task the upgrade
// finds holds no part whose join conflicted, and one that holds one reads back with that part
// alone, beside no need whose join conflicted. A carried plan keeps it with the task's place, and a
// plan started from its start holds none. The record moves a task to either act at position 0
// (MoveTask) and refuses either at a position from 1. The control is the store before the
// migration: there the test fails on its second write, the task at its parts.
func TestTheStoreHoldsATaskAtItsParts(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := emptyStore(t)
	migrateThrough(t, store, subOwnersVersion-1)
	rows := subOwnersRows{t: t, store: store}
	rows.plan()
	if err := rows.standAt("f", record.PlaceParts, 0); !refusedByCheck(err, "plan_tasks_running_kind_check") {
		t.Fatalf("a task at its parts before the migration = %v, want the running kind's check to refuse it (23514)", err)
	}

	if _, err := store.Migrate(ctx); err != nil {
		t.Fatalf("migrate through the sub-owners: %v", err)
	}
	if err := rows.standAt("f", record.PlaceParts, 0); err != nil {
		t.Fatalf("a task at its parts after the migration: %v", err)
	}
	if err := rows.standAt("g", record.PlaceReported, 0); err != nil {
		t.Fatalf("a task at its report after the migration: %v", err)
	}
	if err := rows.standAt("h", record.PlaceParts, 1); !refusedByCheck(err, "plan_tasks_place_check") {
		t.Fatalf("a task at its parts at position 1 = %v, want the place check to refuse it (23514): only a step stands at a position from 1", err)
	}
	if err := rows.standAt("h", "review", 0); !refusedByCheck(err, "plan_tasks_running_kind_check") {
		t.Fatalf("a task at an act of no kind = %v, want the running kind's check to refuse it (23514)", err)
	}
	for _, kind := range append(append([]string{}, taskCopiesKinds...), "task_pull_requests_close", "task_copy_remove") {
		if err := rows.queue(kind, "{}"); err != nil {
			t.Fatalf("queue a %s row after the migration: %v", kind, err)
		}
	}
	for _, kind := range []string{"parts_join", "nope"} {
		if err := rows.queue(kind, "{}"); !refusedByCheck(err, "outbox_kind_check") {
			t.Fatalf("a %s row after the migration = %v, want the kind check to refuse it (23514): no executor runs it", kind, err)
		}
	}

	var found []string
	if err := store.Pool().QueryRow(ctx, `select parts_conflicts from plan_tasks
		where issue = 'LEGION-208' and generation = 1 and task = 'f'`).Scan(&found); err != nil {
		t.Fatalf("read the parts of f whose join conflicted: %v", err)
	}
	if found == nil || len(found) != 0 {
		t.Fatalf("the parts of f whose join conflicted = %#v, want '{}': no release before gave a task parts", found)
	}
	if err := rows.exec(`update plan_tasks set parts_conflicts = '{f2}' where issue = 'LEGION-208' and generation = 1 and task = 'f'`); err != nil {
		t.Fatalf("write the parts of f whose join conflicted: %v", err)
	}
	records := record.NewStore()
	taskAt := func(generation uint64, id string) record.PlanTask {
		t.Helper()
		var stored *record.StoredPlan
		inTx(t, store, func(tx pgx.Tx) error {
			var err error
			stored, err = records.Plan(ctx, tx, "LEGION-208", generation)
			return err
		})
		task, ok := stored.TaskOf(id)
		if !ok {
			t.Fatalf("the plan of LEGION-208 at generation %d holds no task %s", generation, id)
		}
		return task
	}
	atParts, atReport := record.TaskPlace{Kind: record.PlaceParts}, record.TaskPlace{Kind: record.PlaceReported}
	if f := taskAt(1, "f"); !slices.Equal(f.PartsConflicts, []string{"f2"}) || f.JoinConflicts != nil || f.Place != atParts {
		t.Fatalf("task f holds the parts %v and the needs %v whose join conflicted, at %+v; want [f2], none, at its parts", f.PartsConflicts, f.JoinConflicts, f.Place)
	}
	if g := taskAt(1, "g"); g.PartsConflicts != nil || g.Place != atReport {
		t.Fatalf("task g holds the parts %#v whose join conflicted, at %+v; want none, at its report", g.PartsConflicts, g.Place)
	}

	inTx(t, store, func(tx pgx.Tx) error { return records.CarryPlan(ctx, tx, "LEGION-208", 1, 2, true) })
	if carried := taskAt(2, "f"); !slices.Equal(carried.PartsConflicts, []string{"f2"}) || carried.Place != atParts {
		t.Fatalf("the carried task f holds the parts %v whose join conflicted, at %+v; want [f2], at its parts: a carried run goes on where it stood",
			carried.PartsConflicts, carried.Place)
	}
	inTx(t, store, func(tx pgx.Tx) error { return records.CarryPlan(ctx, tx, "LEGION-208", 1, 3, false) })
	if fresh := taskAt(3, "f"); fresh.PartsConflicts != nil || fresh.State != record.TaskWaiting || fresh.Place.Started() {
		t.Fatalf("task f of the plan started from its start holds the parts %v whose join conflicted, %s at %+v; want none, waiting at no act",
			fresh.PartsConflicts, fresh.State, fresh.Place)
	}

	for _, place := range []record.TaskPlace{atParts, atReport} {
		inTx(t, store, func(tx pgx.Tx) error {
			return records.MoveTask(ctx, tx, "LEGION-208", 3, "f", place, record.TaskRunning)
		})
		if moved := taskAt(3, "f"); moved.Place != place || moved.State != record.TaskRunning {
			t.Fatalf("task f moved to %+v stands %s at %+v, want running there", place, moved.State, moved.Place)
		}
	}
	for _, place := range []record.TaskPlace{{Kind: record.PlaceParts, Position: 1}, {Kind: record.PlaceReported, Position: 1}} {
		err := Transact(ctx, store.Pool(), func(ctx context.Context, tx pgx.Tx) error {
			return records.MoveTask(ctx, tx, "LEGION-208", 3, "f", place, record.TaskRunning)
		})
		if err == nil || !strings.Contains(err.Error(), "no task stands at that place") {
			t.Fatalf("a move of task f to %+v = %v, want it refused: a task stands at its parts and its report at position 0 alone", place, err)
		}
	}
}

// subOwnersWayBackUpdate is the first and the last words of the way back's update of the tasks at
// parts or at reported, the statement the control takes off.
const (
	subOwnersWayBackUpdate    = "update plan_tasks set running_kind = ''"
	subOwnersWayBackUpdateEnd = "where running_kind in ('parts', 'reported');\n"
)

// The way back the header of 20261124023617 gives lets the release before read the store again. It
// deletes each notice and controller notice of the three kinds that release decodes no row of
// (parts-done, report-refused, merge-returned), and no other row. It stands each task at its parts
// or at its report at no act: a running or a held one stopped for the reason the header names,
// with no hold and no wait of its step, and a stopped or a done one in its state, with its reason;
// a task at a step keeps its place. Then the store refuses a task at its parts again, the column is
// gone, and the version row with it, so the roll-forward runs the migration again, which takes
// such a task. The control is the block without its update of the tasks: the check it writes again
// then refuses the tasks at their parts.
func TestMigrationSubOwnersWayBackStandsEachTaskAtItsPartsAtNoAct(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	block := wayBackOf(t, subOwnersVersion, "the sub-owners")
	from, to := strings.Index(block, subOwnersWayBackUpdate), strings.Index(block, subOwnersWayBackUpdateEnd)
	if from < 0 || to < from {
		t.Fatalf("the way back of %d holds no update from %q to %q, the statement the control takes off", subOwnersVersion, subOwnersWayBackUpdate,
			subOwnersWayBackUpdateEnd)
	}
	withoutUpdate := block[:from] + block[to+len(subOwnersWayBackUpdateEnd):]
	for _, tc := range []struct {
		name  string
		block string
		ran   bool
	}{
		{name: "control: the block without its update fails on the running kind's check", block: withoutUpdate},
		{name: "the block", block: block, ran: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := emptyStore(t)
			if _, err := store.Migrate(ctx); err != nil {
				t.Fatalf("migrate: %v", err)
			}
			rows := subOwnersRows{t: t, store: store}
			rows.plan()
			for _, seed := range []string{
				`insert into plan_tasks (issue, generation, task, plan_version, block_text, state, running_kind, position)
					values ('LEGION-208', 1, 'a', 1, 'Build the widget.', 'running', 'step', 1)`,
				`insert into plan_tasks (issue, generation, task, plan_version, block_text, owner_role, state, running_kind, step_wait, step_wait_since, parts_conflicts)
					values ('LEGION-208', 1, 'f', 1, 'Build the widget.', 'architect', 'running', 'parts', 'join', now(), '{f2}')`,
				`insert into plan_tasks (issue, generation, task, plan_version, block_text, owner_role, state, running_kind, hold_reason)
					values ('LEGION-208', 1, 'g', 1, 'Build the widget.', 'architect', 'held', 'reported', 'escalated')`,
				`insert into plan_tasks (issue, generation, task, plan_version, block_text, owner_role, state, running_kind)
					values ('LEGION-208', 1, 'h', 1, 'Build the widget.', 'architect', 'done', 'reported')`,
				`insert into plan_tasks (issue, generation, task, plan_version, block_text, owner_role, state, running_kind, stopped_reason)
					values ('LEGION-208', 1, 's', 1, 'Build the widget.', 'architect', 'stopped', 'parts', 'stopped by its owner')`,
			} {
				if err := rows.exec(seed); err != nil {
					t.Fatalf("seed the tasks: %v", err)
				}
			}
			for _, row := range []struct{ kind, payload string }{
				{"notice", `{"kind": "parts-done"}`},
				{"notice", `{"kind": "report-refused"}`},
				{"controller_notice", `{"kind": "merge-returned"}`},
				{"notice", `{"kind": "phase-finished"}`},
				{"task_branch", `{"generation": 1, "task": "a"}`},
			} {
				if err := rows.queue(row.kind, row.payload); err != nil {
					t.Fatalf("queue a %s row: %v", row.kind, err)
				}
			}

			err := rows.exec(tc.block, pgx.QueryExecModeSimpleProtocol)
			if !tc.ran {
				if !refusedByCheck(err, "plan_tasks_running_kind_check") {
					t.Fatalf("the way back without its update = %v, want the running kind's check to refuse the tasks at their parts (23514)", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("run the way back: %v", err)
			}
			// A boolean cast to text is its word (true, false); concat_ws alone would write t or f.
			tasks := rowsAsText(t, store, `select concat_ws(' | ', task, state, running_kind, position, stopped_reason, hold_reason, step_wait, (step_wait_since is null)::text)
				from plan_tasks order by task`)
			want := []string{
				"a | running | step | 1 |  |  |  | true",
				"f | stopped |  | 0 | rolled back below sub-owners |  |  | true",
				"g | stopped |  | 0 | rolled back below sub-owners |  |  | true",
				"h | done |  | 0 |  |  |  | true",
				"s | stopped |  | 0 | stopped by its owner |  |  | true",
			}
			if !slices.Equal(tasks, want) {
				t.Fatalf("the tasks after the way back =\n%s\nwant\n%s", strings.Join(tasks, "\n"), strings.Join(want, "\n"))
			}
			if left := rowsAsText(t, store, `select concat_ws(' ', kind, payload->>'kind') from outbox order by id`); !slices.Equal(left, []string{"notice phase-finished", "task_branch"}) {
				t.Fatalf("the outbox after the way back holds %q, want the phase-finished notice and the task_branch row alone", left)
			}
			if err := rows.standAt("p", record.PlaceParts, 0); !refusedByCheck(err, "plan_tasks_running_kind_check") {
				t.Fatalf("a task at its parts after the way back = %v, want the running kind's check to refuse it (23514)", err)
			}
			if err := rows.exec(`update plan_tasks set parts_conflicts = '{}'`); err == nil || !strings.Contains(err.Error(), "does not exist") {
				t.Fatalf("a write of plan_tasks.parts_conflicts after the way back = %v, want the column gone", err)
			}
			if got := rowsAsText(t, store, `select count(*)::text from schema_version where version = 20261124023617`); got[0] != "0" {
				t.Fatalf("schema_version rows of %d after the way back = %s, want none", subOwnersVersion, got[0])
			}

			if _, err := store.Migrate(ctx); err != nil {
				t.Fatalf("roll forward after the way back: %v", err)
			}
			if err := rows.standAt("p", record.PlaceParts, 0); err != nil {
				t.Fatalf("a task at its parts after the roll-forward: %v", err)
			}
		})
	}
}
