package store

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/sjawhar/envoy/internal/pgmigrate"
)

// anyRoleHoldersVersion is the migration that widens issue_role_holders.role from the six Legion
// roles to the role grammar (20261124122030_issue_role_holders_any_role).
const anyRoleHoldersVersion = 20261124122030

// The migration keeps every row the table holds and takes a role the deployment defines from then
// on: a database migrated through the version before it holds a tester's row, which the migration
// leaves as it is; after it the table stores a checker and a name of 32 characters, and its check
// still refuses the controller, a name with a capital and a name of 33 characters. The check is
// added not valid, so the migration reads no stored row: the catalog records it unvalidated.
func TestTheAnyRoleMigrationKeepsTheStoredHoldersAndTakesTheRoleGrammar(t *testing.T) {
	ctx := context.Background()
	store := openEmptyTestStore(t)
	migrateThrough(t, store, anyRoleHoldersVersion-1)
	for _, statement := range []string{
		`insert into projects (key, name, default_assignee) values ('INFRA', 'Infra', 'alice@d.example')`,
		`insert into issues (key, project_key, number, title, created_by, rank) values ('INFRA-1', 'INFRA', 1, 'One', '{"kind":"user","id":"alice"}', 'U')`,
		`insert into issue_role_holders (issue_key, role, session) values ('INFRA-1', 'tester', 'ses-t')`,
	} {
		if _, err := store.Pool.Exec(ctx, statement); err != nil {
			t.Fatalf("seed %q: %v", statement, err)
		}
	}
	insert := func(role, session string) error {
		_, err := store.Pool.Exec(ctx, `insert into issue_role_holders (issue_key, role, session) values ('INFRA-1', $1, $2)`, role, session)
		return err
	}
	// The control: before the migration the table refuses a role the deployment defines.
	var refusal *pgconn.PgError
	if err := insert("checker", "ses-c"); !errors.As(err, &refusal) || refusal.Code != "23514" {
		t.Fatalf("before the migration the table answered the role checker with %v, want its check refusing it (23514)", err)
	}

	migrateThrough(t, store, anyRoleHoldersVersion)

	var stored string
	if err := store.Pool.QueryRow(ctx, `select string_agg(role || '=' || session, ',' order by role) from issue_role_holders`).Scan(&stored); err != nil {
		t.Fatalf("read the stored holders: %v", err)
	}
	if stored != "tester=ses-t" {
		t.Fatalf("after the migration the table holds %q, want the tester's row it held, %q", stored, "tester=ses-t")
	}
	for role, session := range map[string]string{"checker": "ses-c", "a" + strings.Repeat("b", 31): "ses-32"} {
		if err := insert(role, session); err != nil {
			t.Errorf("after the migration the table refused the role %q: %v", role, err)
		}
	}
	for _, role := range []string{"controller", "Checker", "c" + strings.Repeat("h", 32)} {
		if err := insert(role, "ses-x"); !errors.As(err, &refusal) || refusal.Code != "23514" {
			t.Errorf("after the migration the table answered the role %q with %v, want its check refusing it (23514)", role, err)
		}
	}
	var validated bool
	if err := store.Pool.QueryRow(ctx, `
		select convalidated from pg_constraint
		where conrelid = 'issue_role_holders'::regclass and conname = 'issue_role_holders_role_check'
	`).Scan(&validated); err != nil {
		t.Fatalf("read the role check of issue_role_holders: %v", err)
	}
	if validated {
		t.Error("the role check is recorded validated, want it added not valid, which reads no stored row")
	}
}

// The migration drops the role check without cascade and adds the grammar's back not valid: an
// alter table of catalog-only subcommands, which the census exempts from the table size limit at any
// size, and whose own census answers 0. The limit is one byte, so a migration the census did not
// exempt would be refused on issue_role_holders, which holds its two indexes' pages even empty.
func TestCensusExemptsTheAnyRoleMigrationAtAnyTableSize(t *testing.T) {
	store := openEmptyTestStore(t)
	migrateThrough(t, store, anyRoleHoldersVersion-1)
	report, err := census(context.Background(), store.Pool.Config().ConnString(), migrationsThrough(t, anyRoleHoldersVersion),
		pgmigrate.CensusOptions{TableLimit: 1})
	if err != nil {
		t.Fatalf("census: %v", err)
	}
	for _, entry := range report.Pending {
		if entry.Migration.Version != anyRoleHoldersVersion {
			continue
		}
		if len(entry.Refusals) != 0 {
			t.Fatalf("%s refused: %v", entry.Migration.Name, entry.Refusals)
		}
		if entry.TableSizeExemption != "alter table" {
			t.Errorf("TableSizeExemption = %q, want %q", entry.TableSizeExemption, "alter table")
		}
		if entry.Count == nil || *entry.Count != 0 {
			t.Errorf("the census of %s answered %v, want 0", entry.Migration.Name, entry.Count)
		}
		if len(entry.Tables) != 1 || entry.Tables[0].Name != "issue_role_holders" || entry.Tables[0].Bytes <= 1 {
			t.Errorf("tables = %+v, want issue_role_holders alone, over the one-byte limit", entry.Tables)
		}
		return
	}
	t.Fatalf("%d is not pending; report:\n%s", anyRoleHoldersVersion, reportText(report))
}
