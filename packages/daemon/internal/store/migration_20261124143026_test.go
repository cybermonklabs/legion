package store

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/pods"
)

// releaseTeardownVersion is the version of the migration this file tests, so the test can migrate
// through everything before it whatever lands beside it.
const releaseTeardownVersion = 20261124143026

// 20261124143026 adds issue_releases.teardown, whether the teardown of the release of a pod no role
// can use may have taken the pod (ReleaseTeardown), and clears nothing: a mark standing across it is
// a release in flight, which keeps its mark. The column defaults to `reached`, so every mark this
// release did not write reads `reached`: one standing across the migration, of either kind, and one
// a release before it writes afterwards, which names its columns and leaves teardown to its
// default — that daemon, and the one the migration replaced, record a release whatever its teardown
// found, so this release records it whatever the runtime answers. This release's own insert
// (Store.ReleaseIdleIssue) writes the empty note. An idle mark's note is read by nothing. The column
// takes the empty note and `reached` and no other word.
//
// It also adds pod_releases.issue_release_id, the mark a release's record stands for, with one
// record per mark at most. A record standing across the migration names no mark, nor does one the
// release before it writes afterwards, its six columns named, and the index holds none of them; a
// second record of one mark is refused.
//
// The negative control is the read of the column before the migration, which the database refuses
// as a column it does not know. The control of the default is the migration with the empty note as
// its default: the marks main's insert writes then read the empty note, and this test fails, as it
// does where this release's insert leaves teardown to the default. The control of the check and of
// the index is the migration without them: the write of another word, or a second record of one
// mark, then succeeds, and this test fails.
func TestAReleaseMarkNotesItsTeardownWith20261124143026(t *testing.T) {
	ctx := context.Background()
	store := emptyStore(t)
	migrateThrough(t, store, releaseTeardownVersion-1)
	// mark is the insert of a mark as the release before the migration writes it, its twelve
	// columns named (main's insert, Store.ReleaseIdleIssue).
	mark := func(issue, role, kind string) error {
		_, err := store.Pool().Exec(ctx, `insert into issue_releases
			(project, issue, generation, first_row, boundary_row, last_row, started_row, wake_row, role, kind, marked_at, turn_start)
			values ('legion', $1, 1, 18, 20, 22, 0, 0, $2, $3, now(), now())`, issue, role, kind)
		return err
	}
	// The releases in flight across the migration, which it must keep.
	for _, standing := range []struct{ issue, role, kind string }{
		{"LEGION-208", "", "unowned"},
		{"LEGION-210", "implementer", "idle"},
	} {
		if err := mark(standing.issue, standing.role, standing.kind); err != nil {
			t.Fatalf("mark the %s release standing before the migration: %v", standing.kind, err)
		}
	}
	var teardown string
	err := store.Pool().QueryRow(ctx, `select teardown from issue_releases where issue = 'LEGION-208'`).Scan(&teardown)
	if err == nil || !strings.Contains(err.Error(), "teardown") {
		t.Fatalf("reading teardown before the migration = %v, want the column to be unknown", err)
	}
	// A release's record standing across the migration.
	if _, err := store.Pool().Exec(ctx, `insert into pod_releases (project, issue, generation, roles, at, bundle_bytes)
		values ('legion', 'LEGION-207', 1, '', now(), 0)`); err != nil {
		t.Fatalf("record a release before the migration: %v", err)
	}

	if _, err := store.Migrate(ctx); err != nil {
		t.Fatalf("migrate through the teardown's note: %v", err)
	}

	// The release before the migration writes its mark as it did, and the mark reads `reached`; its
	// update of a mark's suspended roles, the one other write it makes, succeeds too.
	if err := mark("LEGION-209", "", "unowned"); err != nil {
		t.Fatalf("mark a release with the columns a release before the migration names: %v", err)
	}
	if _, err := store.Pool().Exec(ctx, `update issue_releases set suspended = suspended || 'implementer'::text
		where project = 'legion' and issue = 'LEGION-210' and not ('implementer' = any(suspended))`); err != nil {
		t.Fatalf("append a suspended role as the release before the migration does: %v", err)
	}
	got := rowsAsText(t, store, `select row(issue, kind, teardown)::text from issue_releases order by issue`)
	want := []string{"(LEGION-208,unowned,reached)", "(LEGION-209,unowned,reached)", "(LEGION-210,idle,reached)"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("the marks after the migration (issue, kind, teardown) = %v, want %v: every mark this release did not write at reached", got, want)
	}
	// This release's own insert writes the empty note, on a store of its own, since a mark of
	// rootIssue stands above.
	fresh := migratedStore(t)
	openRunningTree(t, fresh)
	if marked, err := fresh.ReleaseIdleIssue(ctx, "legion", rootIssue, func(view pods.IdleView) (IdleReleaseMark, error) {
		return IdleReleaseMark{Generation: view.Generation, SuspendsNoRole: true, Rows: []IdleReleaseRow{{Kind: "issue_release",
			Payload: []byte(`{"tree":"LEGION-208","generation":1,"treeGeneration":1}`), Boundary: true}}}, nil
	}); err != nil || !marked {
		t.Fatalf("ReleaseIdleIssue = %t, %v; want the release of the pod no role can use marked", marked, err)
	}
	if got := rowsAsText(t, fresh, `select row(issue, kind, teardown)::text from issue_releases`); !reflect.DeepEqual(got, []string{"(LEGION-208,unowned,\"\")"}) {
		t.Fatalf("the mark this release writes (issue, kind, teardown) = %v, want the empty note", got)
	}
	for _, note := range []ReleaseTeardown{TeardownReached, TeardownNotReached} {
		if _, err := store.Pool().Exec(ctx, `update issue_releases set teardown = $1 where issue = 'LEGION-209'`, string(note)); err != nil {
			t.Fatalf("note %q on a mark: %v", note, err)
		}
	}
	_, err = store.Pool().Exec(ctx, `update issue_releases set teardown = 'took_nothing' where issue = 'LEGION-209'`)
	if err == nil || !strings.Contains(err.Error(), "issue_releases_teardown_check") {
		t.Fatalf("noting a word the check does not name = %v, want the teardown check to refuse it", err)
	}

	// pod_releases: a record standing across the migration and one the release before it writes
	// afterwards — its six columns named (main's insert, Store.RecordPodRelease) — name no
	// mark, and the one-record-per-mark index holds neither, however many there are; a record that
	// names a mark is taken once.
	podRelease := func() error {
		_, err := store.Pool().Exec(ctx, `insert into pod_releases (project, issue, generation, roles, at, bundle_bytes)
			values ('legion', 'LEGION-208', 1, '', now(), 0)`)
		return err
	}
	for i := range 2 {
		if err := podRelease(); err != nil {
			t.Fatalf("record a release as the release before the migration does, the %d time: %v", i+1, err)
		}
	}
	named := func() error {
		_, err := store.Pool().Exec(ctx, `insert into pod_releases (project, issue, generation, roles, at, bundle_bytes, issue_release_id)
			values ('legion', 'LEGION-208', 1, '', now(), 0, 7)`)
		return err
	}
	if err := named(); err != nil {
		t.Fatalf("record a release that names its mark: %v", err)
	}
	if err := named(); err == nil || !strings.Contains(err.Error(), "pod_releases_one_per_issue_release") {
		t.Fatalf("a second record of the same mark = %v, want the one-record-per-mark index to refuse it", err)
	}
	if got := rowsAsText(t, store, `select coalesce(issue_release_id, 0)::text from pod_releases order by id`); !reflect.DeepEqual(got, []string{"0", "0", "0", "7"}) {
		t.Fatalf("the records' marks = %v, want the three of the earlier release naming none and the one naming mark 7", got)
	}
}
