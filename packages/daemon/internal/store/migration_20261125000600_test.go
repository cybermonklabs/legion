package store

import (
	"context"
	"reflect"
	"testing"
)

// failedLooksVersion is the version of the migration this file tests, so the test can migrate
// through everything before it whatever lands beside it.
const failedLooksVersion = 20261125000600

// failedLooksRelease names the release whose way back the migration's header carries, as a
// failure to read that block names it.
const failedLooksRelease = "the release that backs a production check off a checking role that keeps failing"

// 20261125000600 adds production_checks.failed_looks, the looks in a row whose checking role's
// claim failed, at its default, 0, on every check the table holds, and the release before's
// insert, which names none of it, still lands at 0, while its upsert of a check this release
// counted leaves the count as it stands. Its way back, the last paragraph of the migration's
// header (migrations.WayBack), takes `wakeAt` off every queued check_wake row, which this release
// writes and the release before refuses (DisallowUnknownFields), and leaves every other key and
// every other kind's row as it stands; it drops the column and deletes the migration's version
// row, so the roll-forward's Migrate applies the file again and adds the column again at 0. The
// negative control is the store before the migration, whose table holds no such column.
func TestTheFailedLooksComeAtZeroAndTheirWayBackLetsThemComeAgain(t *testing.T) {
	ctx := context.Background()
	store := emptyStore(t)
	migrateThrough(t, store, failedLooksVersion-1)
	exec := func(statement string) {
		t.Helper()
		if _, err := store.Pool().Exec(ctx, statement); err != nil {
			t.Fatalf("%s: %v", statement, err)
		}
	}
	for _, key := range []string{"LEGION-208", "LEGION-209"} {
		exec(`insert into issues (key, tree, project, title, phase, generation, status, rank, last_dispatch_seq)
			values ('` + key + `', 'LEGION-208', 'LEGION', 'done', 'done', 1, 'done', 'A', 1)`)
	}
	// The release before's upsert names these columns and no other (record's PutProductionCheck).
	// A check is keyed by its issue and the moment it opened (20261123154051, below this version, so
	// that each merge keeps its own check), so the upsert's conflict names both, and each upsert of
	// one issue's check names one opening, so the second meets the first's row.
	const releaseBeforesUpsert = `insert into production_checks (issue, project, tree, repo, pr_number, merge_sha, opened_at, bound_at,
		role, looks, look_open, look_wake_at, last_wake_kind, last_wake_detail, last_wake_at, wake_at, last, closed_at, outcome,
		generation, task)
		values ($1, 'LEGION', 'LEGION-208', 'acme/widgets', 42, 'merge', '2026-11-24T12:00:00Z', '2026-12-08T12:00:00Z',
		'tester', $2, false, null, 'named', '', null, null, false, null, null, 1, '')
		on conflict (issue, opened_at) do update set looks = excluded.looks, look_open = excluded.look_open`
	upsert := func(issue string, looks int) {
		t.Helper()
		if _, err := store.Pool().Exec(ctx, releaseBeforesUpsert, issue, looks); err != nil {
			t.Fatalf("the release before's upsert of %s's check: %v", issue, err)
		}
	}
	upsert("LEGION-208", 2)
	const column = `select count(*)::text from information_schema.columns where table_name = 'production_checks' and column_name = 'failed_looks'`
	if got := rowsAsText(t, store, column); !reflect.DeepEqual(got, []string{"0"}) {
		t.Fatalf("the column before the migration = %v, want none", got)
	}

	if _, err := store.Migrate(ctx); err != nil {
		t.Fatalf("migrate through the failed looks: %v", err)
	}
	const counted = `select row(issue, looks, failed_looks)::text from production_checks order by issue`
	if got, want := rowsAsText(t, store, counted), []string{"(LEGION-208,2,0)"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("the checks after the migration (issue, looks, failed_looks) = %v, want %v: the check it found at 0", got, want)
	}
	exec(`update production_checks set failed_looks = 3 where issue = 'LEGION-208'`)
	upsert("LEGION-208", 3)
	upsert("LEGION-209", 1)
	if got, want := rowsAsText(t, store, counted), []string{"(LEGION-208,3,3)", "(LEGION-209,1,0)"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("the checks after the release before's upserts = %v, want %v: the count it left alone, and its new check at 0", got, want)
	}
	if _, err := store.Pool().Exec(ctx, `update production_checks set failed_looks = -1 where issue = 'LEGION-209'`); err == nil {
		t.Fatal("a negative count of failed looks was stored, want the column's check to refuse it")
	}

	queue := func(kind, payload string) {
		t.Helper()
		if _, err := store.Pool().Exec(ctx, `insert into outbox (kind, issue, payload, attempts, next_at, last_error)
			values ($1, 'LEGION-208', $2::jsonb, 0, now(), '')`, kind, payload); err != nil {
			t.Fatalf("queue a %s row: %v", kind, err)
		}
	}
	queue("check_wake", `{"look": 2, "openedAt": "2026-11-24T06:37:06Z", "wakeAt": "2026-11-24T12:47:06Z", "reason": "moved"}`)
	queue("check_wake", `{"look": 1, "reason": "an earlier release's"}`)
	queue("check_look", `{"look": 2, "wakeAt": "kept"}`)

	exec(wayBackOf(t, failedLooksVersion, failedLooksRelease))
	const version = `select count(*)::text from schema_version where version = 20261125000600`
	if got := append(rowsAsText(t, store, column), rowsAsText(t, store, version)...); !reflect.DeepEqual(got, []string{"0", "0"}) {
		t.Fatalf("after the way back the column and the version row = %v, want neither", got)
	}
	const queued = `select kind || ' ' || payload::text from outbox where kind in ('check_wake', 'check_look') order by id`
	if got, want := rowsAsText(t, store, queued), []string{
		`check_wake {"look": 2, "reason": "moved", "openedAt": "2026-11-24T06:37:06Z"}`,
		`check_wake {"look": 1, "reason": "an earlier release's"}`,
		`check_look {"look": 2, "wakeAt": "kept"}`,
	}; !reflect.DeepEqual(got, want) {
		t.Fatalf("the queued rows after the way back = %q, want %q: wakeAt off each check_wake row, every other key and row kept", got, want)
	}
	upsert("LEGION-209", 2)
	if _, err := store.Migrate(ctx); err != nil {
		t.Fatalf("roll forward after the way back: %v", err)
	}
	if got, want := rowsAsText(t, store, counted), []string{"(LEGION-208,3,0)", "(LEGION-209,2,0)"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("the checks after the roll-forward = %v, want %v: the column again, at 0 on every check", got, want)
	}
}
