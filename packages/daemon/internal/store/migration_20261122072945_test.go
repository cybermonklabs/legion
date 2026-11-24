package store

import (
	"context"
	"strings"
	"testing"
)

// podWakeBackstopClaimVersion is the version of the migration this file tests, so the test can
// migrate through everything before it whatever lands beside it.
const podWakeBackstopClaimVersion = 20261122072945

// 20261122072945 keys an open backstop by the claim it wakes (pod_wake_backstops.claim). A
// backstop the release before armed names no claim and reads back as it was, and a second one of
// its role is still refused, as that release's one open backstop per role refused it; two claims of
// one role each hold an open backstop of their own; and one claim holds one. The way back the
// header gives keeps the newest open backstop of each role, puts the one index per role back over
// every row and drops the column, so the release before's second arm of a role is refused again,
// and the roll-forward then runs the migration again.
func TestThePodWakeBackstopClaimMigrationKeysAnOpenBackstopByItsClaim(t *testing.T) {
	ctx := context.Background()
	store := emptyStore(t)
	migrateThrough(t, store, podWakeBackstopClaimVersion-1)
	inserted := func(sql string) int64 {
		t.Helper()
		tag, err := store.Pool().Exec(ctx, sql)
		if err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
		return tag.RowsAffected()
	}
	const releaseBefore = `insert into pod_wake_backstops (project, issue, role, due_at)
		values ('legion', 'LEGION-208', 'tester', now()) on conflict do nothing`
	if n := inserted(releaseBefore); n != 1 {
		t.Fatalf("the release before's arm of the tester = %d rows, want 1", n)
	}
	if _, err := store.Pool().Exec(ctx, `update pod_wake_backstops set claim = 'x'`); err == nil || !strings.Contains(err.Error(), "claim") {
		t.Fatalf("writing claim before the migration = %v, want the column to be unknown: the fixture is not the old shape", err)
	}

	if _, err := store.Migrate(ctx); err != nil {
		t.Fatalf("migrate through the claim of each backstop: %v", err)
	}
	var legacy string
	if err := store.Pool().QueryRow(ctx, `select claim from pod_wake_backstops where role = 'tester'`).Scan(&legacy); err != nil || legacy != "" {
		t.Fatalf("the backstop the release before armed names %q, %v; want no claim", legacy, err)
	}
	if n := inserted(releaseBefore); n != 0 {
		t.Fatalf("the release before's second arm of the tester = %d rows, want none: one open backstop per role among rows that name no claim", n)
	}
	for _, token := range []string{"legion-legion-legion-208-implementer", "legion-legion-legion-208-tester"} {
		arm := `insert into pod_wake_backstops (project, issue, role, claim, due_at)
			values ('legion', 'LEGION-208', 'reviewer', '` + token + `', now()) on conflict do nothing`
		if n := inserted(arm); n != 1 {
			t.Fatalf("the arm of %s on the reviewer's role = %d rows, want 1: each claim holds its own", token, n)
		}
		if n := inserted(arm); n != 0 {
			t.Fatalf("the second arm of %s = %d rows, want none: one open backstop per claim", token, n)
		}
	}

	if _, err := store.Pool().Exec(ctx, wayBackOf(t, podWakeBackstopClaimVersion, "the claim of each backstop")); err != nil {
		t.Fatalf("run the way back: %v", err)
	}
	if got := rowsAsText(t, store, `select role || ' ' || id from pod_wake_backstops where not fired order by role, id`); len(got) != 2 ||
		!strings.HasPrefix(got[0], "reviewer ") || !strings.HasPrefix(got[1], "tester ") {
		t.Fatalf("the open backstops after the way back = %v, want one of the reviewer and one of the tester", got)
	}
	if _, err := store.Pool().Exec(ctx, `select claim from pod_wake_backstops`); err == nil {
		t.Fatal("pod_wake_backstops.claim still reads after the way back, want the column gone")
	}
	if n := inserted(`insert into pod_wake_backstops (project, issue, role, due_at)
		values ('legion', 'LEGION-208', 'reviewer', now()) on conflict do nothing`); n != 0 {
		t.Fatalf("the release before's second arm of the reviewer after the way back = %d rows, want none", n)
	}
	var version int
	if err := store.Pool().QueryRow(ctx, `select count(*) from schema_version where version = $1`, podWakeBackstopClaimVersion).Scan(&version); err != nil || version != 0 {
		t.Fatalf("schema_version rows of %d after the way back = %d, %v; want none", podWakeBackstopClaimVersion, version, err)
	}
	if _, err := store.Migrate(ctx); err != nil {
		t.Fatalf("roll forward after the way back: %v", err)
	}
	if got := rowsAsText(t, store, `select claim from pod_wake_backstops where not fired order by role`); len(got) != 2 || got[0] != "" || got[1] != "" {
		t.Fatalf("the open backstops' claims after the roll-forward = %q, want both naming none", got)
	}
}
