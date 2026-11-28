package store

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

// checkResultsNoRoleVersion is the migration that lets check_results.role hold the empty string,
// the role of a result whose session holds no role of its recording issue (INFRA-3994).
const checkResultsNoRoleVersion = 20261128161114

// insertCheckResultRole writes one result of ACME-1's check c1 holding role, straight into
// check_results, as a write of any release would.
func insertCheckResultRole(ctx context.Context, store *Store, role string) error {
	_, err := store.Pool.Exec(ctx, `
		insert into check_results (issue_key, check_issue_key, check_artifact_id, check_block_id, check_version,
			check_sha256, role, commit_sha, verdict, actor)
		values ('ACME-1', 'ACME-1', '7c1e8a52-3f4b-4d6e-9a0b-1c2d3e4f5a6b', 'c1', 1, 'digest', $1, repeat('a', 40), 'pass',
			'{"kind":"user","id":"alice"}')
	`, role)
	return err
}

// TestACheckResultTakesNoRoleOnlyAfter20261128161114: before the migration the role check refuses
// a result with the empty role; the census counts no row; after it the empty role is taken beside
// every role the check took, and a role outside a-z and - is still refused, by the check's own
// name. The row stored before the migration is unchanged.
func TestACheckResultTakesNoRoleOnlyAfter20261128161114(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := openMigratedTestStore(t, checkResultsNoRoleVersion-1)
	if _, err := store.Pool.Exec(ctx, `
		insert into projects (key, name, default_assignee) values ('ACME', 'Acme', 'alice@example.com');
		insert into issues (key, project_key, number, title, created_by, rank) values
			('ACME-1', 'ACME', 1, 'One', '{"kind":"user","id":"alice"}', 'U1');
		insert into artifacts (id, issue_key, project_key, slug, name, kind, created_by) values
			('7c1e8a52-3f4b-4d6e-9a0b-1c2d3e4f5a6b', 'ACME-1', 'ACME', 'verification', 'verification.md', 'doc', '{"kind":"user","id":"alice"}');
	`); err != nil {
		t.Fatalf("seed an issue and its Verification document: %v", err)
	}
	if err := insertCheckResultRole(ctx, store, "tester"); err != nil {
		t.Fatalf("a tester's result before the migration: %v", err)
	}
	if err := insertCheckResultRole(ctx, store, ""); !isSQLState(err, "23514") {
		t.Fatalf("a result with no role before the migration: %v, want SQLSTATE 23514 from the role check", err)
	}

	if got := censusCount(t, store, checkResultsNoRoleVersion); got != 0 {
		t.Fatalf("census = %d, want 0: the new check is wider, so no stored result breaks it", got)
	}
	if err := store.migrate(ctx, migrationsThrough(t, checkResultsNoRoleVersion)); err != nil {
		t.Fatalf("migrate to %d: %v", checkResultsNoRoleVersion, err)
	}

	if err := insertCheckResultRole(ctx, store, ""); err != nil {
		t.Fatalf("a result with no role after the migration: %v", err)
	}
	if err := insertCheckResultRole(ctx, store, "implementer"); err != nil {
		t.Fatalf("an implementer's result after the migration: %v", err)
	}
	for _, role := range []string{"Tester", "tester!", "a-very-long-role-name-of-thirty-three"} {
		var refusal *pgconn.PgError
		err := insertCheckResultRole(ctx, store, role)
		if !errors.As(err, &refusal) || refusal.Code != "23514" || refusal.ConstraintName != "check_results_role_check" {
			t.Errorf("a result with role %q after the migration: %v, want SQLSTATE 23514 from check_results_role_check", role, err)
		}
	}
	var roles []string
	rows, err := store.Pool.Query(ctx, `select role from check_results order by created_at, role`)
	if err != nil {
		t.Fatalf("read the stored roles: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var role string
		if err := rows.Scan(&role); err != nil {
			t.Fatalf("scan a stored role: %v", err)
		}
		roles = append(roles, role)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read the stored roles: %v", err)
	}
	if len(roles) != 3 || roles[0] != "tester" {
		t.Fatalf("the stored roles are %q, want the tester's row stored before the migration first, then two more", roles)
	}
}
