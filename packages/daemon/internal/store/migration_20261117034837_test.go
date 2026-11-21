package store

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// issuePlansVersion is the version of the migration this file tests, so a test can migrate through
// everything before it whatever lands beside it.
const issuePlansVersion = 20261117034837

// 20261117034837 holds the plan of an issue as rows. A step's role is a role's name as a plan
// writes it, lower case, so a role spelled another way is refused rather than stored as a role
// nobody defines; a task stands at no act, at a step, at its retro or at its merge, so an act of
// another kind is refused rather than stored as a place the walk cannot read; and a task is in one
// of the states the walk writes, left among them, the state of a task whose issue left the
// workflow before it was done. Each refusal is the table's own check (SQLSTATE 23514), named, and
// the same write with what the plan grammar and the walk take is stored: that pair is the control,
// which shows each refusal is the check's and not a row the write could not reach.
func TestMigrationIssuePlansRefusesABadRoleAndAPlace(t *testing.T) {
	ctx := context.Background()
	store := migratedStore(t)
	exec := func(sql string) error {
		_, err := store.Pool().Exec(ctx, sql)
		return err
	}
	for _, seed := range []string{
		`insert into issue_plans (issue, generation, version, source, artifact_id, given_by, given_at)
			values ('LEGION-208', 0, 1, 'owner', 'artifact-208', 'legion-legion-legion-208-architect', now())`,
		`insert into plan_tasks (issue, generation, task, plan_version, block_text, state)
			values ('LEGION-208', 0, 'a', 1, 'Build the widget.', 'waiting')`,
	} {
		if err := exec(seed); err != nil {
			t.Fatalf("seed the plan: %v", err)
		}
	}
	for _, tc := range []struct {
		name, refused, stored, constraint string
	}{
		{
			name: "a role spelled with a capital",
			refused: `insert into plan_steps (issue, generation, task, position, role, type)
				values ('LEGION-208', 0, 'a', 1, 'Implementer', 'implementation')`,
			stored: `insert into plan_steps (issue, generation, task, position, role, type)
				values ('LEGION-208', 0, 'a', 1, 'implementer', 'implementation')`,
			constraint: "plan_steps_role_check",
		},
		{
			name:       "a running act of a kind no plan has",
			refused:    `update plan_tasks set running_kind = 'review' where issue = 'LEGION-208' and task = 'a'`,
			stored:     `update plan_tasks set running_kind = 'step', position = 1 where issue = 'LEGION-208' and task = 'a'`,
			constraint: "plan_tasks_running_kind_check",
		},
		{
			name:       "a state no task is in",
			refused:    `update plan_tasks set state = 'parked' where issue = 'LEGION-208' and task = 'a'`,
			stored:     `update plan_tasks set state = 'left' where issue = 'LEGION-208' and task = 'a'`,
			constraint: "plan_tasks_state_check",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := exec(tc.refused)
			var pgErr *pgconn.PgError
			if !errors.As(err, &pgErr) || pgErr.Code != "23514" || pgErr.ConstraintName != tc.constraint {
				t.Fatalf("the refused write = %v, want the check %s to refuse it", err, tc.constraint)
			}
			if err := exec(tc.stored); err != nil {
				t.Fatalf("the same write with what a plan takes = %v, want it stored", err)
			}
		})
	}
}

// 20261117034837 also adds the step a phases row runs (record.PhaseRow.StepTask and StepPlace). A
// release before it ran no plan, so every row the upgrade finds is the row of an issue with no plan
// rows, which reads no task and place 0, and every other column of the row stays as that release
// left it. Both columns are unknown before the migration, which is what shows the fixture is the
// old shape.
func TestTheIssuePlansMigrationLeavesEveryRowItFindsWithNoStep(t *testing.T) {
	ctx := context.Background()
	store := emptyStore(t)
	migrateThrough(t, store, issuePlansVersion-1)
	for _, seed := range []string{
		`insert into issues (key, tree, project, title, phase, generation, status, rank, last_dispatch_seq)
			values ('LEGION-208', 'LEGION-208', 'LEGION', 'root', 'reviewing', 1, 'needs_review', 'A', 1)`,
		`insert into phases (issue, role, claim, handoff_commit, rounds, verdict, entry, entry_task)
			values ('LEGION-208', 'implementer', 'legion-LEGION-208-implementer', 'abc123', 2, '', 3, 'Continue root. Issue: LEGION-208. Phase: implementing.')`,
	} {
		if _, err := store.Pool().Exec(ctx, seed); err != nil {
			t.Fatalf("record before the migration: %v", err)
		}
	}
	for _, column := range []string{"phases set step_task = 'a'", "phases set step_place = 1"} {
		_, err := store.Pool().Exec(ctx, "update "+column)
		if err == nil || !strings.Contains(err.Error(), "does not exist") {
			t.Fatalf("update %s before the migration = %v, want the column unknown: the fixture is not the old shape", column, err)
		}
	}

	if _, err := store.Migrate(ctx); err != nil {
		t.Fatalf("migrate through the issue plans: %v", err)
	}
	var implementer record.PhaseRow
	if err := pgx.BeginFunc(ctx, store.Pool(), func(tx pgx.Tx) error {
		rows, err := record.NewStore().Phases(ctx, tx, "LEGION-208")
		if err != nil {
			return err
		}
		implementer = record.PhaseRowOf(rows, claim.RoleImplementer)
		return nil
	}); err != nil {
		t.Fatalf("read LEGION-208 after the migration: %v", err)
	}
	if implementer.HandoffCommit != "abc123" || implementer.Rounds != 2 || implementer.Entry != 3 {
		t.Fatalf("the implementer's row after the migration = %+v, want the row the release before left", implementer)
	}
	if implementer.StepTask != "" || implementer.StepPlace != 0 {
		t.Fatalf("the implementer's row after the migration runs step %d of task %q, want no step: no plan ran before it", implementer.StepPlace, implementer.StepTask)
	}
}
