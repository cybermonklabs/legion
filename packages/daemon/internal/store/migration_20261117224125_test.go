package store

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

// taskHeldVersion is the version of the migration this file tests.
const taskHeldVersion = 20261117224125

// 20261117224125 lets a task be held for the reason of its hold and lets the step at its place
// wait. A task the upgrade finds keeps its row whole, with no hold reason and no step wait, and
// the three columns are unknown before the migration, which is what shows the fixture is the old
// shape. The running task of an issue held at the upgrade reads held with the issue's hold reason,
// so the hold is one fact of the task: a hold for a failed claim (no reason) and an escalated hold
// alike. A task of another generation, a task of an issue that is not held, and the stopped task
// of an issue held because it was stopped keep their state. The store refuses a hold reason on a
// task that is not held and a step wait with no moment. The way back the migration's header gives
// writes each held task running again and drops the three columns, and Migrate then holds the task
// of each held issue again.
func TestMigrationTaskHeldHoldsTheTaskOfEachHeldIssue(t *testing.T) {
	ctx := context.Background()
	st := emptyStore(t)
	migrateThrough(t, st, taskHeldVersion-1)
	exec := func(sql string, args ...any) error {
		_, err := st.Pool().Exec(ctx, sql, args...)
		return err
	}
	issue := func(key, phase string, generation int, heldFrom, reason any) {
		t.Helper()
		if err := exec(`insert into issues (key, tree, project, title, phase, generation, status, rank, last_dispatch_seq, held_from, hold_reason)
			values ($1, $1, 'LEGION', 'root', $2, $3, 'in_progress', 'A', 1, $4, $5)`, key, phase, generation, heldFrom, reason); err != nil {
			t.Fatalf("record %s before the migration: %v", key, err)
		}
	}
	task := func(key string, generation int, state string) {
		t.Helper()
		for _, seed := range []string{
			`insert into issue_plans (issue, generation, version, source, artifact_id, given_by, given_at)
				values ($1, $2, 1, 'owner', 'artifact', 'legion-legion-legion-208-architect', now())`,
			`insert into plan_tasks (issue, generation, task, plan_version, block_text, state, running_kind, position, pass)
				values ($1, $2, 'a', 1, 'Build the widget.', '` + state + `', 'step', 2, 3)`,
		} {
			if err := exec(seed, key, generation); err != nil {
				t.Fatalf("record the plan of %s at generation %d before the migration: %v", key, generation, err)
			}
		}
	}
	issue("LEGION-208", "held", 1, "implementing", "escalated")
	task("LEGION-208", 1, "running")
	issue("LEGION-209", "held", 2, "reviewing", nil)
	task("LEGION-209", 2, "running")
	task("LEGION-209", 1, "running")
	issue("LEGION-210", "held", 1, "implementing", "task-stopped")
	task("LEGION-210", 1, "stopped")
	issue("LEGION-211", "implementing", 1, nil, nil)
	task("LEGION-211", 1, "running")
	for _, column := range []string{"hold_reason = ''", "step_wait = ''", "step_wait_since = null"} {
		if err := exec("update plan_tasks set " + column); err == nil || !strings.Contains(err.Error(), "does not exist") {
			t.Fatalf("update plan_tasks set %s before the migration = %v, want the column unknown: the fixture is not the old shape", column, err)
		}
	}

	if _, err := st.Migrate(ctx); err != nil {
		t.Fatalf("migrate through the held task: %v", err)
	}
	type stands struct {
		state, reason, wait string
		waitSet             bool
		kind                string
		position, pass      int
	}
	read := func(key string, generation int) stands {
		t.Helper()
		var s stands
		if err := st.Pool().QueryRow(ctx, `select state, hold_reason, step_wait, step_wait_since is not null, running_kind, position, pass
			from plan_tasks where issue = $1 and generation = $2 and task = 'a'`, key, generation).Scan(
			&s.state, &s.reason, &s.wait, &s.waitSet, &s.kind, &s.position, &s.pass); err != nil {
			t.Fatalf("read task a of %s at generation %d: %v", key, generation, err)
		}
		return s
	}
	wants := []struct {
		key        string
		generation int
		want       stands
	}{
		{"LEGION-208", 1, stands{state: "held", reason: "escalated", kind: "step", position: 2, pass: 3}},
		{"LEGION-209", 2, stands{state: "held", reason: "", kind: "step", position: 2, pass: 3}},
		{"LEGION-209", 1, stands{state: "running", kind: "step", position: 2, pass: 3}},
		{"LEGION-210", 1, stands{state: "stopped", kind: "step", position: 2, pass: 3}},
		{"LEGION-211", 1, stands{state: "running", kind: "step", position: 2, pass: 3}},
	}
	for _, w := range wants {
		if got := read(w.key, w.generation); got != w.want {
			t.Errorf("task a of %s at generation %d after the migration = %+v, want %+v", w.key, w.generation, got, w.want)
		}
	}
	refusedBy := func(err error, constraint string) bool {
		var pgErr *pgconn.PgError
		return errors.As(err, &pgErr) && pgErr.Code == "23514" && pgErr.ConstraintName == constraint
	}
	for sql, constraint := range map[string]string{
		`update plan_tasks set hold_reason = 'escalated' where issue = 'LEGION-211'`:                     "plan_tasks_hold_reason_held_check",
		`update plan_tasks set hold_reason = 'task-stopped' where issue = 'LEGION-208'`:                  "plan_tasks_hold_reason_check",
		`update plan_tasks set step_wait = 'agent' where issue = 'LEGION-211'`:                           "plan_tasks_step_wait_since_check",
		`update plan_tasks set step_wait = 'review', step_wait_since = now() where issue = 'LEGION-211'`: "plan_tasks_step_wait_check",
		`update plan_tasks set state = 'paused' where issue = 'LEGION-211'`:                              "plan_tasks_state_check",
	} {
		if err := exec(sql); !refusedBy(err, constraint) {
			t.Errorf("%s = %v, want the check %s to refuse it (23514)", sql, err, constraint)
		}
	}
	if err := exec(`update plan_tasks set step_wait = 'join', step_wait_since = now() where issue = 'LEGION-211'`); err != nil {
		t.Fatalf("a step wait with its moment: %v", err)
	}

	if err := exec(`update plan_tasks set state = 'running', hold_reason = '' where state = 'held';
		alter table plan_tasks drop column if exists step_wait_since, drop column if exists step_wait, drop column if exists hold_reason;
		alter table plan_tasks drop constraint if exists plan_tasks_state_check;
		alter table plan_tasks add constraint plan_tasks_state_check check (state in ('waiting', 'running', 'done', 'stopped', 'left'));
		delete from schema_version where version = 20261117224125`); err != nil {
		t.Fatalf("run the migration's way back: %v", err)
	}
	var state string
	if err := st.Pool().QueryRow(ctx, `select state from plan_tasks where issue = 'LEGION-208' and generation = 1`).Scan(&state); err != nil || state != "running" {
		t.Fatalf("task a of LEGION-208 after the way back = %q (%v), want running, the state the release before reads", state, err)
	}
	if err := exec(`update plan_tasks set hold_reason = ''`); err == nil || !strings.Contains(err.Error(), "does not exist") {
		t.Fatalf("a write of hold_reason after the way back = %v, want the column gone", err)
	}
	if _, err := st.Migrate(ctx); err != nil {
		t.Fatalf("migrate again after the way back: %v", err)
	}
	if got := read("LEGION-208", 1); got.state != "held" || got.reason != "escalated" {
		t.Fatalf("task a of LEGION-208 after the roll forward = %+v, want held for escalated again", got)
	}
}
