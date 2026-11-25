package store

import (
	"context"
	"reflect"
	"testing"
)

// keepsNoWorkspaceVersion is the version of the migration this file tests, so the test can migrate
// through everything before it whatever lands beside it.
const keepsNoWorkspaceVersion = 20261123050533

// keepsNoWorkspaceRelease names the release whose way back the migration's header carries, as a
// failure to read that block names it.
const keepsNoWorkspaceRelease = "the release that keeps no saved workspace"

// The release before's two writes of workspace_bundles, as its store made them: the save's insert
// (PutWorkspaceBundle), which names every column the save wrote, and the take's record of an
// attempt (TakeWorkspaceBundleForRestore), which writes the three attempt columns together.
const (
	bundleInsertBefore = `insert into workspace_bundles (issue, release_id, source_pod_uid, bundle, size_bytes, at, head, tips, bookmarks)
		values ('LEGION-1', 7, 'pod-uid-1', '\x5041434b'::bytea, 4, now(), 'f2f134541b30dfa5160a5d1ce5e17b76c967c661',
			'["f2f134541b30dfa5160a5d1ce5e17b76c967c661"]'::jsonb, '{"legion/LEGION-1":"f2f134541b30dfa5160a5d1ce5e17b76c967c661"}'::jsonb)`
	bundleTakeBefore = `update workspace_bundles set attempt_pod_uid = 'pod-uid-2', attempt_at = now(), attempt_outcome = 'sent'
		where issue = 'LEGION-1'`
)

// workspaceBundlesShape is workspace_bundles as the catalog reads it: each column with its type,
// whether it takes null and its default, and each constraint with its definition, in name order.
// A table the way back makes again is the release before's table when the two read alike.
const workspaceBundlesShape = `select c.column_name || ' ' || c.data_type || ' ' || c.is_nullable || ' ' || coalesce(c.column_default, '-')
	from information_schema.columns c where c.table_schema = 'public' and c.table_name = 'workspace_bundles'
	union all
	select k.conname || ' ' || pg_get_constraintdef(k.oid) from pg_constraint k
	where k.conrelid = 'public.workspace_bundles'::regclass
	order by 1`

// 20261123050533 drops workspace_bundles, where the release before saved an idle release's
// unpushed work, makes idle_release_declines, where this release records the release its check
// declined, and adds issue_releases.turn_start, the newest turn a release's mark was taken at. The
// way back, the last paragraph of the migration's header (migrations.WayBack), is what an operator
// runs with every daemon stopped before pinning the release before: it makes workspace_bundles
// again, empty, in the shape that release reads and writes, drops idle_release_declines and
// turn_start and deletes the migration's version row. So the release before's save and take land
// again with every column and constraint they had, and a roll forward runs the migration once
// more: the table goes and idle_release_declines and turn_start come back.
func TestTheWayBackOfTheReleaseThatKeepsNoSavedWorkspace(t *testing.T) {
	ctx := context.Background()
	store := emptyStore(t)
	migrateThrough(t, store, keepsNoWorkspaceVersion-1)
	exec := func(statement string) {
		t.Helper()
		if _, err := store.Pool().Exec(ctx, statement); err != nil {
			t.Fatalf("%s: %v", statement, err)
		}
	}
	exists := func(table string) bool {
		t.Helper()
		var found bool
		if err := store.Pool().QueryRow(ctx, `select to_regclass($1) is not null`, "public."+table).Scan(&found); err != nil {
			t.Fatalf("read whether %s exists: %v", table, err)
		}
		return found
	}
	versionApplied := func() bool {
		t.Helper()
		var applied bool
		if err := store.Pool().QueryRow(ctx, `select exists (select 1 from schema_version where version = $1)`,
			keepsNoWorkspaceVersion).Scan(&applied); err != nil {
			t.Fatalf("read whether %d is applied: %v", keepsNoWorkspaceVersion, err)
		}
		return applied
	}
	turnStart := func() bool {
		t.Helper()
		var found bool
		if err := store.Pool().QueryRow(ctx, `select exists (select 1 from information_schema.columns
			where table_schema = 'public' and table_name = 'issue_releases' and column_name = 'turn_start')`).Scan(&found); err != nil {
			t.Fatalf("read whether issue_releases.turn_start exists: %v", err)
		}
		return found
	}
	if turnStart() {
		t.Fatal("the release before's issue_releases already has turn_start; the migration has nothing to add")
	}

	before := rowsAsText(t, store, workspaceBundlesShape)
	if len(before) == 0 {
		t.Fatal("the release before's database holds no workspace_bundles to compare the way back with")
	}
	exec(bundleInsertBefore)
	exec(bundleTakeBefore)

	if _, err := store.Migrate(ctx); err != nil {
		t.Fatalf("migrate through the release that keeps no saved workspace: %v", err)
	}
	if exists("workspace_bundles") || !exists("idle_release_declines") || !turnStart() || !versionApplied() {
		t.Fatalf("after the migration workspace_bundles exists = %t, idle_release_declines exists = %t, turn_start exists = %t, version applied = %t; want the first gone and the other three there",
			exists("workspace_bundles"), exists("idle_release_declines"), turnStart(), versionApplied())
	}
	exec(`insert into idle_release_declines (project, issue, declined_at, checked_at, role, reason, head, commits, changed, started_row, pod_uid)
		values ('legion', 'LEGION-1', now() - interval '1 minute', now(), 'implementer', 'unpushed_work', 'abc', 1, false, 3, 'pod-uid-2')`)
	exec(`insert into issue_releases (project, issue, generation, first_row, boundary_row, last_row, started_row, role, marked_at, turn_start)
		values ('legion', 'LEGION-1', 1, 18, 20, 22, 3, 'implementer', now(), now() - interval '1 hour')`)
	t.Logf("the migration dropped workspace_bundles with its row and made idle_release_declines")

	exec(wayBackOf(t, keepsNoWorkspaceVersion, keepsNoWorkspaceRelease))
	if !exists("workspace_bundles") || exists("idle_release_declines") || turnStart() || versionApplied() {
		t.Fatalf("after the way back workspace_bundles exists = %t, idle_release_declines exists = %t, turn_start exists = %t, version applied = %t; want the first back and the other three gone",
			exists("workspace_bundles"), exists("idle_release_declines"), turnStart(), versionApplied())
	}
	// The mark a daemon of this release wrote stays, in the shape the release before reads.
	if rows := rowsAsText(t, store, `select issue from issue_releases`); !reflect.DeepEqual(rows, []string{"LEGION-1"}) {
		t.Fatalf("issue_releases after the way back holds %v, want the one mark written before it", rows)
	}
	if after := rowsAsText(t, store, workspaceBundlesShape); !reflect.DeepEqual(after, before) {
		t.Fatalf("workspace_bundles after the way back reads\n%v\nwant the release before's\n%v", after, before)
	}
	if rows := rowsAsText(t, store, `select issue from workspace_bundles`); len(rows) != 0 {
		t.Fatalf("workspace_bundles after the way back holds %v, want it empty", rows)
	}
	exec(bundleInsertBefore)
	exec(bundleTakeBefore)
	if got := rowsAsText(t, store, `select row(issue, release_id, size_bytes, attempt_pod_uid, attempt_outcome)::text from workspace_bundles`); !reflect.DeepEqual(got,
		[]string{"(LEGION-1,7,4,pod-uid-2,sent)"}) {
		t.Fatalf("the release before's save and take after the way back left %v, want its one row with the attempt it recorded", got)
	}
	for _, refused := range []string{
		`insert into workspace_bundles (issue, release_id, source_pod_uid, bundle, size_bytes, at, head)
			values ('LEGION-2', 0, 'pod-uid-1', '\x00'::bytea, 1, now(), 'abc')`,
		`insert into workspace_bundles (issue, release_id, source_pod_uid, bundle, size_bytes, at, head)
			values ('LEGION-2', 1, '', '\x00'::bytea, 1, now(), 'abc')`,
		`update workspace_bundles set attempt_at = null where issue = 'LEGION-1'`,
		`update workspace_bundles set attempt_outcome = 'restored' where issue = 'LEGION-1'`,
	} {
		if _, err := store.Pool().Exec(ctx, refused); err == nil {
			t.Fatalf("%s was taken after the way back; the release before's table refuses it", refused)
		}
	}
	t.Logf("the way back made workspace_bundles again in the release before's shape, dropped idle_release_declines and turn_start and deleted the version row")

	applied, err := store.Migrate(ctx)
	if err != nil || applied != 1 {
		t.Fatalf("the roll forward's Migrate applied %d migrations, %v; want the one the way back re-armed", applied, err)
	}
	if exists("workspace_bundles") || !exists("idle_release_declines") || !turnStart() || !versionApplied() {
		t.Fatalf("after the roll forward workspace_bundles exists = %t, idle_release_declines exists = %t, turn_start exists = %t, version applied = %t; want the first gone and the other three there",
			exists("workspace_bundles"), exists("idle_release_declines"), turnStart(), versionApplied())
	}
}
