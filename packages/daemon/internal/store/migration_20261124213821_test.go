package store

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
)

// podResumeClaimVersion is the version of the migration this file tests, so the test can migrate
// through everything before it whatever lands beside it.
const podResumeClaimVersion = 20261124213821

// 20261124213821 names on each resume the claim its wake brought back (pod_resumes.claim) and
// makes the open resume one per claim, not one per role. A resume the release before wrote was the
// wake of its role's own claim, the claim whose token the release before builds from the role, so
// the migration names that token on it: the token claim.NewToken gives the resume's project, issue
// and role, for each project of the database. After it the column refuses a resume that names no
// claim, so no reader has a resume of no claim to read; two claims of one role each hold an open
// resume of their own, and one claim holds one. The way back the header gives closes all but the
// newest open resume of each role, puts the one open resume per role back over every row, drops
// the column and deletes its version row, so the release before's second open resume of a role is
// refused again and the roll-forward runs the migration again, which names each resume again.
func TestThePodResumeClaimMigrationNamesTheRolesOwnClaimOnEachResume(t *testing.T) {
	ctx := context.Background()
	store := emptyStore(t)
	migrateThrough(t, store, podResumeClaimVersion-1)
	inserted := func(sql string) int64 {
		t.Helper()
		tag, err := store.Pool().Exec(ctx, sql)
		if err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
		return tag.RowsAffected()
	}
	for _, seed := range []string{
		`insert into pod_resumes (release_id, project, issue, role, kind, at) values (0, 'legion', 'LEGION-208', 'tester', 'delivery', now())`,
		`insert into pod_resumes (release_id, project, issue, role, kind, at, first_turn_at) values (0, 'legion', 'LEGION-208', 'implementer', 'delivery', now(), now())`,
		`insert into pod_resumes (release_id, project, issue, role, kind, at) values (0, 'infra', 'INFRA-5', 'security-reviewer', 'backstop', now())`,
	} {
		if n := inserted(seed); n != 1 {
			t.Fatalf("record before the migration %s = %d rows, want 1", seed, n)
		}
	}
	if n := inserted(`insert into pod_resumes (release_id, project, issue, role, kind, at)
		values (0, 'legion', 'LEGION-208', 'tester', 'delivery', now()) on conflict do nothing`); n != 0 {
		t.Fatalf("a second open resume of the tester before the migration = %d rows, want none: the fixture is not the old shape", n)
	}
	if _, err := store.Pool().Exec(ctx, `update pod_resumes set claim = 'x'`); err == nil || !strings.Contains(err.Error(), "claim") {
		t.Fatalf("writing claim before the migration = %v, want the column to be unknown: the fixture is not the old shape", err)
	}

	if _, err := store.Migrate(ctx); err != nil {
		t.Fatalf("migrate through the claim of each resume: %v", err)
	}
	var want []string
	for _, r := range []struct{ project, issue, role string }{
		{"legion", "LEGION-208", "tester"}, {"legion", "LEGION-208", "implementer"}, {"infra", "INFRA-5", "security-reviewer"},
	} {
		token, err := claim.NewToken(r.project, r.issue, claim.Role(r.role))
		if err != nil {
			t.Fatal(err)
		}
		want = append(want, r.issue+" "+r.role+" "+string(token))
	}
	if got := rowsAsText(t, store, `select issue || ' ' || role || ' ' || claim from pod_resumes order by id`); !slices.Equal(got, want) {
		t.Fatalf("the resumes after the migration = %q, want %q: each naming its role's own claim", got, want)
	}
	for _, unnamed := range []string{
		`insert into pod_resumes (release_id, project, issue, role, kind, at) values (0, 'legion', 'LEGION-208', 'reviewer', 'delivery', now())`,
		`insert into pod_resumes (release_id, project, issue, role, claim, kind, at) values (0, 'legion', 'LEGION-208', 'reviewer', '', 'delivery', now())`,
	} {
		if _, err := store.Pool().Exec(ctx, unnamed); err == nil || !strings.Contains(err.Error(), "claim") {
			t.Fatalf("%s = %v, want the resume that names no claim refused", unnamed, err)
		}
	}
	resume := func(role, token string) string {
		return `insert into pod_resumes (release_id, project, issue, role, claim, kind, at)
			values (0, 'legion', 'LEGION-208', '` + role + `', '` + token + `', 'delivery', now()) on conflict do nothing`
	}
	if n := inserted(resume("tester", "legion-legion-legion-208-tester")); n != 0 {
		t.Fatalf("a second open resume of the tester's claim = %d rows, want none: one open resume per claim", n)
	}
	for _, token := range []string{"legion-legion-legion-208-reviewer", "legion-legion-legion-208-implementer"} {
		if n := inserted(resume("reviewer", token)); n != 1 {
			t.Fatalf("an open resume of %s on the reviewer's role = %d rows, want 1: each claim holds its own", token, n)
		}
	}
	if n := inserted(resume("tester", "legion-legion-legion-208-implementer")); n != 0 {
		t.Fatalf("a second open resume of key a's claim = %d rows, want none: one open resume per claim", n)
	}

	if _, err := store.Pool().Exec(ctx, wayBackOf(t, podResumeClaimVersion, "the claim of each resume")); err != nil {
		t.Fatalf("run the way back: %v", err)
	}
	if got := rowsAsText(t, store, `select issue || ' ' || role from pod_resumes where first_turn_at is null and closed_at is null order by issue, role`); !slices.Equal(got,
		[]string{"INFRA-5 security-reviewer", "LEGION-208 reviewer", "LEGION-208 tester"}) {
		t.Fatalf("the open resumes after the way back = %q, want one of each role", got)
	}
	if _, err := store.Pool().Exec(ctx, `select claim from pod_resumes`); err == nil {
		t.Fatal("pod_resumes.claim still reads after the way back, want the column gone")
	}
	if n := inserted(`insert into pod_resumes (release_id, project, issue, role, kind, at)
		values (0, 'legion', 'LEGION-208', 'reviewer', 'delivery', now()) on conflict do nothing`); n != 0 {
		t.Fatalf("the release before's second open resume of the reviewer after the way back = %d rows, want none: one open resume per role", n)
	}
	var version int
	if err := store.Pool().QueryRow(ctx, `select count(*) from schema_version where version = $1`, podResumeClaimVersion).Scan(&version); err != nil || version != 0 {
		t.Fatalf("schema_version rows of %d after the way back = %d, %v; want none", podResumeClaimVersion, version, err)
	}
	if _, err := store.Migrate(ctx); err != nil {
		t.Fatalf("roll forward after the way back: %v", err)
	}
	if got, want := rowsAsText(t, store, `select claim from pod_resumes where first_turn_at is null and closed_at is null order by issue, role`),
		[]string{"legion-infra-infra-5-security-reviewer", "legion-legion-legion-208-reviewer", "legion-legion-legion-208-tester"}; !slices.Equal(got, want) {
		t.Fatalf("the open resumes' claims after the roll-forward = %q, want %q: each its role's own claim", got, want)
	}
}
