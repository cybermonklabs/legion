package store

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

// 20261125114700 keeps the roles an owner's plan defines, each with the version of the plan that
// wrote it. A role's name is a name as a plan writes it, lower case and at most 32 characters, so a
// role spelled another way or longer is refused by the table's own check (SQLSTATE 23514, named)
// rather than stored as a role no step can name; a role of a version the issue's plans do not hold
// is refused by the foreign key (SQLSTATE 23503); and the delete of a version deletes the roles it
// wrote. The insert of the same role with what a plan takes is stored: that is the control, which
// shows each refusal is the constraint's and not a row the write could not reach.
func TestMigrationPlanRolesRefusesABadRoleAndGoesWithItsVersion(t *testing.T) {
	ctx := context.Background()
	store := migratedStore(t)
	exec := func(sql string) error {
		_, err := store.Pool().Exec(ctx, sql)
		return err
	}
	if err := exec(`insert into issue_plans (issue, generation, version, source, artifact_id, given_by, given_at)
		values ('LEGION-208', 0, 1, 'owner', 'artifact-208', 'legion-legion-legion-208-architect', now())`); err != nil {
		t.Fatalf("seed the plan: %v", err)
	}
	insert := func(role string, version int) string {
		return `insert into plan_roles (issue, generation, role, plan_version, type, model, prompt)
			values ('LEGION-208', 0, '` + role + `', ` + strconv.Itoa(version) + `, 'testing', 'listed-model', 'Probe the change.')`
	}
	for _, tc := range []struct {
		name, refused, code, constraint string
	}{
		{name: "a role spelled with a capital", refused: insert("Bad", 1), code: "23514", constraint: "plan_roles_role_check"},
		{name: "a role of 33 characters", refused: insert("p"+strings.Repeat("r", 32), 1), code: "23514", constraint: "plan_roles_role_check"},
		{name: "a role of a version the plans do not hold", refused: insert("prober", 2), code: "23503", constraint: "plan_roles_issue_generation_plan_version_fkey"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := exec(tc.refused)
			var pgErr *pgconn.PgError
			if !errors.As(err, &pgErr) || pgErr.Code != tc.code || pgErr.ConstraintName != tc.constraint {
				t.Fatalf("the refused insert = %v, want %s to refuse it (SQLSTATE %s)", err, tc.constraint, tc.code)
			}
		})
	}
	if err := exec(insert("p"+strings.Repeat("r", 31), 1)); err != nil {
		t.Fatalf("a role of 32 characters at the plan's version = %v, want it stored", err)
	}
	if err := exec(insert("prober", 1)); err != nil {
		t.Fatalf("the role prober at the plan's version = %v, want it stored", err)
	}
	if err := exec(`delete from issue_plans where issue = 'LEGION-208' and generation = 0 and version = 1`); err != nil {
		t.Fatalf("delete the plan's version: %v", err)
	}
	var left int
	if err := store.Pool().QueryRow(ctx, `select count(*) from plan_roles where issue = 'LEGION-208'`).Scan(&left); err != nil {
		t.Fatalf("count the roles left: %v", err)
	}
	if left != 0 {
		t.Fatalf("plan_roles holds %d roles of a deleted version, want none: they go with the version that wrote them", left)
	}
}
