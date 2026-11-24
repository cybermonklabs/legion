package store

import (
	"context"
	"strings"
	"testing"
)

// oneOpenResumePerClaimVersion is the version of the migration this file tests, so the test can
// migrate through everything before it whatever lands beside it.
const oneOpenResumePerClaimVersion = 20261122074350

// 20261122074350 makes the open resume one per claim. A resume the release before wrote names no
// claim and still holds its role: a second one of that role is refused, as before. Two claims of
// one role each hold an open resume of their own, and one claim holds one. The way back the header
// gives closes all but the newest open resume of each role, puts the one open resume per role back
// over every row, and deletes its version row, so the roll-forward runs the migration again.
func TestTheOneOpenResumePerClaimMigrationKeysAnOpenResumeByItsClaim(t *testing.T) {
	ctx := context.Background()
	store := emptyStore(t)
	migrateThrough(t, store, oneOpenResumePerClaimVersion-1)
	inserted := func(sql string) int64 {
		t.Helper()
		tag, err := store.Pool().Exec(ctx, sql)
		if err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
		return tag.RowsAffected()
	}
	resume := func(role, token string) string {
		return `insert into pod_resumes (release_id, project, issue, role, claim, kind, at)
			values (0, 'legion', 'LEGION-208', '` + role + `', '` + token + `', 'delivery', now()) on conflict do nothing`
	}
	if n := inserted(resume("tester", "")); n != 1 {
		t.Fatalf("the release before's resume of the tester = %d rows, want 1", n)
	}
	if n := inserted(resume("reviewer", "legion-legion-legion-208-reviewer")); n != 1 {
		t.Fatalf("a resume of the reviewer = %d rows, want 1", n)
	}
	if n := inserted(resume("reviewer", "legion-legion-legion-208-implementer")); n != 0 {
		t.Fatalf("a second open resume of the reviewer before the migration = %d rows, want none: the fixture is not the old shape", n)
	}

	if _, err := store.Migrate(ctx); err != nil {
		t.Fatalf("migrate through one open resume per claim: %v", err)
	}
	if n := inserted(resume("tester", "")); n != 0 {
		t.Fatalf("a second resume of the tester naming no claim = %d rows, want none: such a resume holds its role", n)
	}
	if n := inserted(resume("reviewer", "legion-legion-legion-208-implementer")); n != 1 {
		t.Fatalf("key a's resume beside the reviewer's own = %d rows, want 1: each claim holds its own", n)
	}
	if n := inserted(resume("tester", "legion-legion-legion-208-implementer")); n != 0 {
		t.Fatalf("a second open resume of key a's claim = %d rows, want none: one open resume per claim", n)
	}

	if _, err := store.Pool().Exec(ctx, wayBackOf(t, oneOpenResumePerClaimVersion, "one open resume per claim")); err != nil {
		t.Fatalf("run the way back: %v", err)
	}
	if got := rowsAsText(t, store, `select role || ' ' || claim from pod_resumes where first_turn_at is null and closed_at is null order by role, id`); len(got) != 2 ||
		got[0] != "reviewer legion-legion-legion-208-implementer" || !strings.HasPrefix(got[1], "tester ") {
		t.Fatalf("the open resumes after the way back = %q, want the newest of the reviewer and the tester's", got)
	}
	if n := inserted(resume("reviewer", "legion-legion-legion-208-reviewer")); n != 0 {
		t.Fatalf("a second open resume of the reviewer after the way back = %d rows, want none: one open resume per role", n)
	}
	var version int
	if err := store.Pool().QueryRow(ctx, `select count(*) from schema_version where version = $1`, oneOpenResumePerClaimVersion).Scan(&version); err != nil || version != 0 {
		t.Fatalf("schema_version rows of %d after the way back = %d, %v; want none", oneOpenResumePerClaimVersion, version, err)
	}
	if _, err := store.Migrate(ctx); err != nil {
		t.Fatalf("roll forward after the way back: %v", err)
	}
	if n := inserted(resume("reviewer", "legion-legion-legion-208-reviewer")); n != 1 {
		t.Fatalf("the reviewer's own resume after the roll-forward = %d rows, want 1: each claim holds its own again", n)
	}
}
