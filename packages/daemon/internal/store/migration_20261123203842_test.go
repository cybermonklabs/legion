package store

import (
	"context"
	"slices"
	"strings"
	"testing"
)

// podResumeRepairVersion is the version of the migration this file tests.
const podResumeRepairVersion = 20261123203842

// claimTables is what 20261123203838 and 20261123203839 shape, read as text: each index and
// constraint of pod_resumes and pod_wake_backstops, and each column's default and nullability.
func claimTables(t *testing.T, store *Store) []string {
	t.Helper()
	return slices.Concat(
		rowsAsText(t, store, `select indexname || ' ' || indexdef from pg_indexes
			where tablename in ('pod_resumes', 'pod_wake_backstops') order by 1`),
		rowsAsText(t, store, `select conrelid::regclass::text || ' ' || conname || ' ' || pg_get_constraintdef(oid) from pg_constraint
			where conrelid in ('pod_resumes'::regclass, 'pod_wake_backstops'::regclass) order by 1`),
		rowsAsText(t, store, `select table_name || '.' || column_name || ' ' || coalesce(column_default, 'no default') || ' ' || is_nullable
			from information_schema.columns where table_name in ('pod_resumes', 'pod_wake_backstops') order by 1`))
}

// A database can have run the pod_resume_claim and pod_wake_backstop_claim migrations in a shape
// each had before any release carried it, and recorded both, so it never runs their present
// shapes. Three such shapes stood: pod_resume_claim naming each resume's claim and keeping one open
// resume per role; the same beside 20261122074350, which made the open resume one per claim and is
// no migration of this release; and the head 4caa2ff1, where pod_resume_claim gave every resume the
// empty string as its claim and named none, 20261122074350 kept the one open resume per role for
// the resumes of no claim beside one open resume per claim for the others, and
// pod_wake_backstop_claim did the same to the backstops. Each case writes its shape over a database
// migrated to just before 20261123203838 and records each migration it stands for under the
// version this release gives it, so Migrate does not run that migration again. 20261123203842
// brings each to the present shapes, those a database that ran neither is given: every resume and
// backstop names a claim, the column refuses the empty string and has no default, one open resume
// and one open backstop per claim over every row, none per role, and no version row of
// 20261122074350, whose file no release carries. Of two open resumes, or two open backstops, of
// one claim, the newest stands. Its way back deletes its own version row alone, and the
// roll-forward runs it again, which changes nothing.
func TestTheOpenResumeRepairGivesADatabaseOfAnEarlierShapeOneOpenResumePerClaim(t *testing.T) {
	ctx := context.Background()
	fresh := emptyStore(t)
	if _, err := fresh.Migrate(ctx); err != nil {
		t.Fatalf("migrate a fresh database: %v", err)
	}
	present := claimTables(t, fresh)
	for _, tc := range []struct {
		name    string
		earlier []string
	}{
		{name: "pod_resume_claim naming each resume's claim alone", earlier: []string{
			`alter table pod_resumes add column claim text not null default ''`,
			`update pod_resumes set claim = 'legion-' || project || '-' || lower(issue) || '-' || role`,
			`alter table pod_resumes alter column claim drop default, add constraint pod_resumes_claim_check check (claim <> '')`,
			`insert into schema_version (version) values (20261123203838)`,
		}},
		{name: "pod_resume_claim and 20261122074350", earlier: []string{
			`alter table pod_resumes add column claim text not null default ''`,
			`update pod_resumes set claim = 'legion-' || project || '-' || lower(issue) || '-' || role`,
			`alter table pod_resumes alter column claim drop default, add constraint pod_resumes_claim_check check (claim <> '')`,
			`insert into schema_version (version) values (20261123203838)`,
			`drop index pod_resumes_one_open_per_role`,
			`create unique index pod_resumes_one_open_per_claim on pod_resumes (project, issue, claim) where first_turn_at is null and closed_at is null`,
			`insert into schema_version (version) values (20261122074350)`,
		}},
		{name: "the head 4caa2ff1, whose resumes and backstops name no claim", earlier: []string{
			`alter table pod_resumes add column claim text not null default ''`,
			`insert into schema_version (version) values (20261123203838)`,
			`drop index pod_resumes_one_open_per_role`,
			`create unique index pod_resumes_one_open_per_role on pod_resumes (project, issue, role) where first_turn_at is null and closed_at is null and claim = ''`,
			`create unique index pod_resumes_one_open_per_claim on pod_resumes (project, issue, claim) where first_turn_at is null and closed_at is null and claim <> ''`,
			`insert into schema_version (version) values (20261122074350)`,
			`alter table pod_wake_backstops add column claim text not null default ''`,
			`drop index pod_wake_backstops_open`,
			`create unique index pod_wake_backstops_open on pod_wake_backstops (project, issue, role) where not fired and claim = ''`,
			`create unique index pod_wake_backstops_open_claim on pod_wake_backstops (project, issue, claim) where not fired and claim <> ''`,
			`insert into schema_version (version) values (20261123203839)`,
			// A resume and a backstop the release before wrote, or one of that head's for a claim
			// it did not name: each names no claim, and is older than the tester's own below.
			`insert into pod_resumes (release_id, project, issue, role, kind, at) values (0, 'legion', 'LEGION-208', 'tester', 'delivery', now() - interval '1 hour')`,
			`insert into pod_wake_backstops (project, issue, role, due_at) values ('legion', 'LEGION-208', 'tester', now())`,
			`insert into pod_wake_backstops (project, issue, role, claim, due_at) values ('legion', 'LEGION-208', 'tester', 'legion-legion-legion-208-tester', now())`,
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := emptyStore(t)
			migrateThrough(t, store, podResumeClaimVersion-1)
			exec := func(sql string) int64 {
				t.Helper()
				tag, err := store.Pool().Exec(ctx, sql)
				if err != nil {
					t.Fatalf("%s: %v", sql, err)
				}
				return tag.RowsAffected()
			}
			refused := func(sql, constraint string) {
				t.Helper()
				if _, err := store.Pool().Exec(ctx, sql); err == nil || !strings.Contains(err.Error(), constraint) {
					t.Fatalf("%s = %v, want it refused by %s", sql, err, constraint)
				}
			}
			for _, statement := range tc.earlier {
				exec(statement)
			}
			exec(`insert into pod_resumes (release_id, project, issue, role, claim, kind, at)
				values (0, 'legion', 'LEGION-208', 'tester', 'legion-legion-legion-208-tester', 'delivery', now())`)

			if _, err := store.Migrate(ctx); err != nil {
				t.Fatalf("migrate the database of the earlier shape: %v", err)
			}
			if got := claimTables(t, store); !slices.Equal(got, present) {
				t.Fatalf("pod_resumes and pod_wake_backstops after the repair are\n%s\nwant the shape a fresh database is given\n%s",
					strings.Join(got, "\n"), strings.Join(present, "\n"))
			}
			if got := rowsAsText(t, store, `select 'resume ' || id from pod_resumes where claim = ''
				union all select 'backstop ' || id from pod_wake_backstops where claim = ''`); len(got) != 0 {
				t.Fatalf("rows naming no claim after the repair: %q, want each named its role's own claim", got)
			}
			if got := rowsAsText(t, store, `select claim from pod_resumes where first_turn_at is null and closed_at is null`); !slices.Equal(got, []string{"legion-legion-legion-208-tester"}) {
				t.Fatalf("the open resumes after the repair name %q, want the tester's newest alone", got)
			}
			if got := rowsAsText(t, store, `select claim from pod_wake_backstops where not fired`); len(got) > 1 || (len(got) == 1 && got[0] != "legion-legion-legion-208-tester") {
				t.Fatalf("the open backstops after the repair name %q, want at most the tester's newest", got)
			}
			resume := func(token string) string {
				return `insert into pod_resumes (release_id, project, issue, role, claim, kind, at)
					values (0, 'legion', 'LEGION-208', 'tester', '` + token + `', 'delivery', now()) on conflict do nothing`
			}
			arm := func(token string) string {
				return `insert into pod_wake_backstops (project, issue, role, claim, due_at)
					values ('legion', 'LEGION-208', 'tester', '` + token + `', now()) on conflict do nothing`
			}
			if n := exec(resume("legion-legion-legion-208-implementer")); n != 1 {
				t.Fatalf("an open resume of key a's claim on the tester's role, beside the tester's own = %d rows, want 1: one open resume per claim", n)
			}
			if n := exec(resume("legion-legion-legion-208-tester")); n != 0 {
				t.Fatalf("a second open resume of the tester's claim = %d rows, want none: one open resume per claim", n)
			}
			exec(arm("legion-legion-legion-208-tester"))
			if n := exec(arm("legion-legion-legion-208-implementer")); n != 1 {
				t.Fatalf("an open backstop of key a's claim on the tester's role, beside the tester's own = %d rows, want 1: one open backstop per claim", n)
			}
			if n := exec(arm("legion-legion-legion-208-tester")); n != 0 {
				t.Fatalf("a second open backstop of the tester's claim = %d rows, want none: one open backstop per claim", n)
			}
			refused(`insert into pod_resumes (release_id, project, issue, role, claim, kind, at) values (0, 'legion', 'LEGION-208', 'reviewer', '', 'delivery', now())`,
				"pod_resumes_claim_check")
			refused(`insert into pod_wake_backstops (project, issue, role, claim, due_at) values ('legion', 'LEGION-208', 'reviewer', '', now())`,
				"pod_wake_backstops_claim_check")
			versions := func() []string {
				return rowsAsText(t, store, `select version::text from schema_version where version in (20261122074350, 20261123203842) order by version`)
			}
			if got := versions(); len(got) != 1 || got[0] != "20261123203842" {
				t.Fatalf("the version rows of 20261122074350 and the repair are %q, want the repair's alone", got)
			}

			exec(wayBackOf(t, podResumeRepairVersion, "the repair of the open resume per claim"))
			if got := versions(); len(got) != 0 {
				t.Fatalf("the version rows after the way back are %q, want none", got)
			}
			if _, err := store.Migrate(ctx); err != nil {
				t.Fatalf("roll forward after the way back: %v", err)
			}
			if got := versions(); len(got) != 1 || got[0] != "20261123203842" {
				t.Fatalf("the version rows after the roll-forward are %q, want the repair's alone", got)
			}
			if got := claimTables(t, store); !slices.Equal(got, present) {
				t.Fatalf("pod_resumes and pod_wake_backstops after the roll-forward are\n%s\nwant the shape a fresh database is given\n%s",
					strings.Join(got, "\n"), strings.Join(present, "\n"))
			}
			if n := exec(resume("legion-legion-legion-208-reviewer")); n != 1 {
				t.Fatalf("an open resume of a third claim on the tester's role after the roll-forward = %d rows, want 1", n)
			}
		})
	}
}
