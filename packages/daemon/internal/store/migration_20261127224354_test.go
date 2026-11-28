package store

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/record"
)

// observedStepRoleVersion is the version of the migration this file tests, so the test can migrate
// through everything before it whatever lands beside it.
const observedStepRoleVersion = 20261127224354

// observedStepRoleRelease names the release whose way back the migration's header carries, as a
// failure to read that block names it.
const observedStepRoleRelease = "the release whose state events name the role of an issue's running step"

// 20261127224354 adds issues.observed_step_role, the role of the running step that Dispatch last
// observed of an issue, and fills it as the issue's next write would: an issue at step takes the
// role of its task's running step, and so does an issue held from step, whose task keeps its
// place. An issue at a phase of Legion's own takes none whatever its plan rows say runs there, and
// so does an issue at step whose plan rows are of another generation. Its way back, the last
// paragraph of the migration's header, drops the column and deletes the migration's version row,
// and the roll-forward fills the same values again.
func TestTheObservedStepRoleIsFilledFromTheRunningStepOfAnIssueAtStep(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := emptyStore(t)
	migrateThrough(t, store, observedStepRoleVersion-1)
	for _, seed := range []string{
		`insert into issues (key, tree, project, title, phase, generation, status, rank, last_dispatch_seq)
			values ('LEGION-1', 'LEGION-1', 'LEGION', 'at the checker''s step', 'step', 1, 'in_progress', 'A', 1)`,
		`insert into issues (key, tree, project, title, phase, generation, status, rank, last_dispatch_seq, held_from, hold_reason)
			values ('LEGION-2', 'LEGION-2', 'LEGION', 'held from the checker''s step', 'held', 1, 'in_progress', 'B', 1, 'step', 'escalated')`,
		`insert into issues (key, tree, project, title, phase, generation, status, rank, last_dispatch_seq)
			values ('LEGION-3', 'LEGION-3', 'LEGION', 'at the tester''s step', 'testing', 1, 'in_progress', 'C', 1)`,
		`insert into issues (key, tree, project, title, phase, generation, status, rank, last_dispatch_seq)
			values ('LEGION-4', 'LEGION-4', 'LEGION', 'at step, its plan a generation back', 'step', 2, 'in_progress', 'D', 1)`,
	} {
		if _, err := store.Pool().Exec(ctx, seed); err != nil {
			t.Fatalf("seed the release before's issues: %v", err)
		}
	}
	// Each plan is `implementer <role>`, its one task at step 2: the checker's for LEGION-1, LEGION-2
	// (stopped there, as a hold from step keeps it) and LEGION-4 (at generation 1, which LEGION-4 has
	// left), the tester's for LEGION-3.
	given := time.Date(2026, 11, 27, 9, 0, 0, 0, time.UTC)
	records := record.NewStore()
	if err := Transact(ctx, store.pool, func(ctx context.Context, tx pgx.Tx) error {
		for _, plan := range []struct {
			key   string
			role  string
			state record.TaskState
		}{
			{"LEGION-1", "checker", record.TaskRunning},
			{"LEGION-2", "checker", record.TaskStopped},
			{"LEGION-3", "tester", record.TaskRunning},
			{"LEGION-4", "checker", record.TaskRunning},
		} {
			version := record.PlanVersion{Issue: plan.key, Generation: 1, Version: 1, ArtifactID: "artifact-plan", Source: record.PlanSourceOwner,
				GivenBy: "legion-legion-legion-1-architect", GivenAt: given}
			task := record.PlanTask{Issue: plan.key, Generation: 1, Task: "a", PlanVersion: 1, BlockText: "Check the widget.", State: plan.state,
				Place: record.TaskPlace{Kind: record.PlaceStep, Position: 2}}
			steps := []record.PlanStep{
				{Issue: plan.key, Generation: 1, Task: "a", Position: 1, Role: "implementer", Type: "implementation"},
				{Issue: plan.key, Generation: 1, Task: "a", Position: 2, Role: plan.role, Type: "testing"},
			}
			if err := records.PutPlan(ctx, tx, version, []record.PlanTask{task}, steps); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("seed the release before's plans: %v", err)
	}

	if _, err := store.Migrate(ctx); err != nil {
		t.Fatalf("migrate through the observed step role: %v", err)
	}
	const observed = `select key || ' ' || observed_step_role from issues order by key`
	want := []string{"LEGION-1 checker", "LEGION-2 checker", "LEGION-3 ", "LEGION-4 "}
	if got := rowsAsText(t, store, observed); !reflect.DeepEqual(got, want) {
		t.Fatalf("the observed step roles after the migration = %q, want %q", got, want)
	}

	if _, err := store.Pool().Exec(ctx, wayBackOf(t, observedStepRoleVersion, observedStepRoleRelease)); err != nil {
		t.Fatalf("run the way back: %v", err)
	}
	const column = `select count(*)::text from information_schema.columns where table_name = 'issues' and column_name = 'observed_step_role'`
	const version = `select count(*)::text from schema_version where version = 20261127224354`
	if got := append(rowsAsText(t, store, column), rowsAsText(t, store, version)...); !reflect.DeepEqual(got, []string{"0", "0"}) {
		t.Fatalf("after the way back the column and the version row = %v, want neither", got)
	}
	if _, err := store.Migrate(ctx); err != nil {
		t.Fatalf("roll forward after the way back: %v", err)
	}
	if got := rowsAsText(t, store, observed); !reflect.DeepEqual(got, want) {
		t.Fatalf("the observed step roles after the roll-forward = %q, want %q", got, want)
	}
}
