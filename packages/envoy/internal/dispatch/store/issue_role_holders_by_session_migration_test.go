package store

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/sjawhar/envoy/internal/legionclaim"
	"github.com/sjawhar/envoy/internal/pgmigrate"
)

// roleHoldersBySessionVersion is the migration that keys issue_role_holders by session, so two
// sessions can hold one role of an issue (20261124213825_issue_role_holders_by_session).
const roleHoldersBySessionVersion = 20261124213825

// The statements the release before this migration sends to issue_role_holders
// (api/issue_role_holders.go before it): the read of an issue's set, the read of one session's
// role, and the insert of its PUT.
const (
	releaseBeforeSetRead = `
	select i.key, h.role, h.session, h.recorded_at
	from issues i
	left join issue_role_holders h on h.issue_key = i.key
	where i.key = $1
	order by array_position($2::text[], h.role) nulls last, h.role
`
	releaseBeforeHeldRole = `
		select role from issue_role_holders where issue_key = $1 and session = $2
	`
	releaseBeforeInsert = `
			insert into issue_role_holders (issue_key, role, session)
			select $1::text, published.role, published.session
			from unnest($2::text[], $3::text[]) as published(role, session)
			on conflict (issue_key, role) do nothing
		`
)

// wayBackOf is the transaction a migration's header closes with, from its `-- begin;` line to its
// `-- commit;` line, each line with its comment marker taken off: the way back an operator runs.
func wayBackOf(t *testing.T, file string) string {
	t.Helper()
	text, err := os.ReadFile("migrations/" + file)
	if err != nil {
		t.Fatalf("read %s: %v", file, err)
	}
	var lines []string
	in := false
	for _, line := range strings.Split(string(text), "\n") {
		if line == "-- begin;" {
			in = true
		}
		if in {
			lines = append(lines, strings.TrimPrefix(line, "-- "))
		}
		if in && line == "-- commit;" {
			return strings.Join(lines, "\n")
		}
	}
	t.Fatalf("%s holds no way back between `-- begin;` and `-- commit;`", file)
	return ""
}

// The migration keeps every stored holder, as a holder with no claim that works no step, and from
// then on takes two sessions of one role: the agent of a key on the tester's step beside the
// tester's own claim. A session still holds one role of an issue. The release before reads the
// rekeyed table as it did, both its read of an issue's set and its read of one session's role,
// and its PUT's insert is refused (42P10), the 500 the daemon retries until a server of this
// release takes the publication. The control is the table before the migration, which refuses
// the second session of the tester (23505).
//
// The way back folds each role two sessions hold back to one row, the session whose claim works
// the role's step, else the newest, puts the key on (issue_key, role) back and deletes the
// version row, so the roll forward runs the migration again.
func TestTheBySessionMigrationTakesTwoSessionsOfOneRoleAndItsWayBackFoldsThem(t *testing.T) {
	ctx := context.Background()
	store := openEmptyTestStore(t)
	migrateThrough(t, store, roleHoldersBySessionVersion-1)
	for _, statement := range []string{
		`insert into projects (key, name, default_assignee) values ('INFRA', 'Infra', 'alice@d.example')`,
		`insert into issues (key, project_key, number, title, created_by, rank) values ('INFRA-1', 'INFRA', 1, 'One', '{"kind":"user","id":"alice"}', 'U')`,
		`insert into issue_role_holders (issue_key, role, session, recorded_at) values ('INFRA-1', 'tester', 'ses-own', now() - interval '1 hour')`,
	} {
		if _, err := store.Pool.Exec(ctx, statement); err != nil {
			t.Fatalf("seed %q: %v", statement, err)
		}
	}
	insert := func(role, session, claim string, worksStep bool, age string) error {
		_, err := store.Pool.Exec(ctx, `insert into issue_role_holders (issue_key, role, session, claim, works_step, recorded_at)
			values ('INFRA-1', $1, $2, $3, $4, now() - $5::interval)`, role, session, claim, worksStep, age)
		return err
	}
	var refusal *pgconn.PgError
	if _, err := store.Pool.Exec(ctx, `insert into issue_role_holders (issue_key, role, session) values ('INFRA-1', 'tester', 'ses-keyed')`); !errors.As(err, &refusal) || refusal.Code != "23505" {
		t.Fatalf("before the migration the table answered a second session of the tester with %v, want its key refusing it (23505)", err)
	}

	migrateThrough(t, store, roleHoldersBySessionVersion)

	var stored string
	if err := store.Pool.QueryRow(ctx, `select string_agg(role || '=' || session || ':' || claim || ':' || works_step, ',' order by session) from issue_role_holders`).Scan(&stored); err != nil {
		t.Fatalf("read the stored holders: %v", err)
	}
	if want := "tester=ses-own::false"; stored != want {
		t.Fatalf("after the migration the table holds %q, want the tester's row with no claim and no step, %q", stored, want)
	}
	if err := insert("tester", "ses-keyed", "legion-infra-infra-1-implementer", true, "0 seconds"); err != nil {
		t.Fatalf("the table refused the keyed agent's session of the tester: %v", err)
	}
	if err := insert("reviewer", "ses-keyed", "", false, "0 seconds"); !errors.As(err, &refusal) || refusal.Code != "23505" {
		t.Fatalf("the table answered a session of a second role with %v, want its key refusing it (23505)", err)
	}

	rows, err := store.Pool.Query(ctx, releaseBeforeSetRead, "INFRA-1", legionclaim.Roles)
	if err != nil {
		t.Fatalf("the release before's read of the set: %v", err)
	}
	var sessions []string
	for rows.Next() {
		var key, role, session string
		var recordedAt any
		if err := rows.Scan(&key, &role, &session, &recordedAt); err != nil {
			t.Fatalf("the release before's read of the set: %v", err)
		}
		sessions = append(sessions, role+"="+session)
	}
	if err := rows.Err(); err != nil || len(sessions) != 2 || sessions[0] != "tester=ses-keyed" && sessions[1] != "tester=ses-keyed" {
		t.Fatalf("the release before read the set %v, %v; want both sessions of the tester", sessions, err)
	}
	var role string
	if err := store.Pool.QueryRow(ctx, releaseBeforeHeldRole, "INFRA-1", "ses-keyed").Scan(&role); err != nil || role != "tester" {
		t.Fatalf("the release before read the keyed session's role as %q, %v; want tester", role, err)
	}
	_, err = store.Pool.Exec(ctx, releaseBeforeInsert, "INFRA-1", []string{"tester"}, []string{"ses-other"})
	if !errors.As(err, &refusal) || refusal.Code != "42P10" {
		t.Fatalf("the release before's insert answered %v, want Postgres refusing its conflict target (42P10), a 500 the daemon retries", err)
	}

	if err := insert("reviewer", "ses-r-old", "", false, "2 hours"); err != nil {
		t.Fatal(err)
	}
	if err := insert("reviewer", "ses-r-new", "", false, "1 minute"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Pool.Exec(ctx, wayBackOf(t, "20261124213825_issue_role_holders_by_session.up.sql")); err != nil {
		t.Fatalf("the way back: %v", err)
	}
	if err := store.Pool.QueryRow(ctx, `select string_agg(role || '=' || session, ',' order by role) from issue_role_holders`).Scan(&stored); err != nil {
		t.Fatalf("read the holders after the way back: %v", err)
	}
	if want := "reviewer=ses-r-new,tester=ses-keyed"; stored != want {
		t.Fatalf("after the way back the table holds %q, want %q: the session of the step's claim, else the newest", stored, want)
	}
	if _, err := store.Pool.Exec(ctx, `insert into issue_role_holders (issue_key, role, session) values ('INFRA-1', 'tester', 'ses-third')`); !errors.As(err, &refusal) || refusal.Code != "23505" {
		t.Fatalf("after the way back the table answered a second session of the tester with %v, want the key on (issue_key, role) refusing it (23505)", err)
	}
	if _, err := store.Pool.Exec(ctx, releaseBeforeInsert, "INFRA-1", []string{"planner"}, []string{"ses-p"}); err != nil {
		t.Fatalf("after the way back the release before's insert answered %v, want it taken", err)
	}
	var recorded int
	if err := store.Pool.QueryRow(ctx, `select count(*) from schema_migrations where version = $1`, roleHoldersBySessionVersion).Scan(&recorded); err != nil || recorded != 0 {
		t.Fatalf("after the way back the version is recorded %d times, %v; want none", recorded, err)
	}

	migrateThrough(t, store, roleHoldersBySessionVersion)
	if err := insert("tester", "ses-own-again", "legion-infra-infra-1-tester", false, "0 seconds"); err != nil {
		t.Fatalf("after the roll forward the table refused a second session of the tester: %v", err)
	}
}

// The migration drops the primary key without cascade and adds two columns at constant defaults:
// an alter table of catalog-only subcommands, which the census exempts from the table size limit at
// any size, and whose own census answers 0. The limit is one byte, so a migration the census did not
// exempt would be refused on issue_role_holders, which holds its two indexes' pages even empty.
func TestCensusExemptsTheBySessionMigrationAtAnyTableSize(t *testing.T) {
	store := openEmptyTestStore(t)
	migrateThrough(t, store, roleHoldersBySessionVersion-1)
	report, err := census(context.Background(), store.Pool.Config().ConnString(), migrationsThrough(t, roleHoldersBySessionVersion),
		pgmigrate.CensusOptions{TableLimit: 1})
	if err != nil {
		t.Fatalf("census: %v", err)
	}
	for _, entry := range report.Pending {
		if entry.Migration.Version != roleHoldersBySessionVersion {
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
	t.Fatalf("%d is not pending; report:\n%s", roleHoldersBySessionVersion, reportText(report))
}
