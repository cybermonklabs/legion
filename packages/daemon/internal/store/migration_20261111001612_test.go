package store

import (
	"context"
	"strings"
	"testing"
)

// releaseKindVersion is the version of the migration this file tests, so the test can migrate
// through everything before it whatever lands beside it.
const releaseKindVersion = 20261111001612

// A release's mark says which of the two rules wrote it. The idle rule releases a pod whose
// roles are all quiet, saves the workspace's unpushed work from the shim of the role its mark
// names and suspends one claim per live role; the second rule (workflow.UnownedPodRelease)
// releases a pod whose every claim is already failed, retired or suspended, which suspends no
// role and saves no bundle, so its mark has no role to name. role is not null under a check
// naming the six workflow roles, so that mark could not be written at all before this
// migration: the negative control is that refusal, read before it.
//
// After the migration each kind's own shape is the only one the table takes. An idle mark names
// a role and an unowned mark leaves it empty, the two wrong pairs are refused by the same
// constraint — an idle release nobody could ask for a bundle, and an unowned release that claims
// a role it never stopped — and a kind that is neither rule is refused as well.
//
// The migration clears nothing, unlike the three earlier migrations of this table, so a release
// in flight when the daemon restarts into this schema still has its mark, and the default says
// what that release is: the idle rule's. A mark deleted here would leave that release's rows
// with nothing to read, its pod never torn down and the roles it had suspended `released` with
// nothing to resume them.
func TestAReleaseMarkNamesItsRuleWith20261111001612(t *testing.T) {
	ctx := context.Background()
	store := emptyStore(t)
	migrateThrough(t, store, releaseKindVersion-1)
	mark := func(issue, role string) error {
		_, err := store.Pool().Exec(ctx, `insert into issue_releases
			(project, issue, generation, first_row, boundary_row, last_row, started_row, role)
			values ('legion', $1, 1, 18, 20, 22, 0, $2)`, issue, role)
		return err
	}
	markOfKind := func(issue, role, kind string) error {
		_, err := store.Pool().Exec(ctx, `insert into issue_releases
			(project, issue, generation, first_row, boundary_row, last_row, started_row, role, kind)
			values ('legion', $1, 1, 18, 20, 22, 0, $2, $3)`, issue, role, kind)
		return err
	}

	err := mark("LEGION-208", "")
	if err == nil || !strings.Contains(err.Error(), "issue_releases_role_check") {
		t.Fatalf("marking a release that names no role before the migration = %v, want the role constraint to refuse it: it is the mark the migration exists for", err)
	}
	t.Logf("before the migration the mark that names no role is refused: %v", err)

	// The release in flight across the migration, which it must keep.
	if err := mark("LEGION-209", "implementer"); err != nil {
		t.Fatalf("mark the idle release standing before the migration: %v", err)
	}

	if _, err := store.Migrate(ctx); err != nil {
		t.Fatalf("migrate through the release's kind: %v", err)
	}

	var kind string
	if err := store.Pool().QueryRow(ctx, `select kind from issue_releases where issue = 'LEGION-209'`).Scan(&kind); err != nil {
		t.Fatalf("read the mark the migration found standing: %v", err)
	}
	if kind != "idle" {
		t.Fatalf("the mark standing across the migration = kind %q, want idle: the release in flight is the idle rule's", kind)
	}
	t.Logf("after the migration the release in flight still has its mark, and its kind is %q", kind)

	for _, row := range []struct{ issue, role, kind string }{
		{issue: "LEGION-210", role: "architect", kind: "idle"},
		{issue: "LEGION-211", role: "", kind: "unowned"},
	} {
		if err := markOfKind(row.issue, row.role, row.kind); err != nil {
			t.Fatalf("mark a %s release with role %q: %v", row.kind, row.role, err)
		}
	}
	t.Logf("after the migration both rules write their mark: an idle release naming its role, an unowned one naming none")

	for _, row := range []struct {
		name, issue, role, kind, constraint string
	}{
		{name: "an idle release that names no role", issue: "LEGION-212", role: "", kind: "idle",
			constraint: "issue_releases_role_check"},
		{name: "an unowned release that names a role", issue: "LEGION-213", role: "tester", kind: "unowned",
			constraint: "issue_releases_role_check"},
		{name: "a release of neither rule", issue: "LEGION-214", role: "tester", kind: "evicted",
			constraint: "issue_releases_kind_check"},
	} {
		err := markOfKind(row.issue, row.role, row.kind)
		if err == nil || !strings.Contains(err.Error(), row.constraint) {
			t.Fatalf("marking %s = %v, want %s to refuse it", row.name, err, row.constraint)
		}
		t.Logf("%s is refused: %v", row.name, err)
	}
}
