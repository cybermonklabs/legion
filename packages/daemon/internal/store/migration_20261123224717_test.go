package store

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store/migrations"
	"github.com/sjawhar/legion/daemon/internal/treelifecycle"
)

// membersOwedVersion is the version of the migration this file tests, so the test can migrate
// through everything before it whatever lands beside it.
const membersOwedVersion = 20261123224717

// issuePriorityVersion is the migration that adds issues.priority, which PutIssue writes.
const issuePriorityVersion = 20261124060500

// membersOwedRelease names the release whose way back the migration's header carries, as a
// failure to read that block names it.
const membersOwedRelease = "the release that brings a re-admitted tree's members back the first time the tree runs"

// 20261123224717 adds issues.members_owed and marks the roots whose re-admitted generation no
// promotion has opened (dispatch://INFRA-5471): a root standing in todo at admitted on a
// generation a re-admission started, holding no slot, released for no wait and with no linger. A
// root the release before promoted (in progress, holding its slot, or released for a wait), a root
// that never ran, a root parked again, a root whose first phase started and a child all stay
// unmarked, so the first pass after the upgrade brings nothing of a promoted generation back. The
// mark reads no design gate, which only the deployment's steps can judge: a root still queued
// behind its gate, a root whose gate opened before its promotion while its tree's lifecycle stood
// open, one whose spec opened while its Verification document waits, and one whose gate opened
// while its tree's lifecycle was not open are all marked, since no promotion has brought their
// members back. Its way back, the last paragraph of the migration's header, drops the column and
// deletes the migration's version row, and the roll-forward marks the same roots again.
func TestTheMembersOwedMarkComesOnlyOnRootsWhoseReadmissionHasStillToRun(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := emptyStore(t)
	migrateThrough(t, store, membersOwedVersion-1)
	// The seed goes through this release's PutIssue, which writes issues.priority, a column the
	// waiting line's issue priority (20261124060500) adds above this migration, and the close
	// wait's columns, which the done-by-checks migration (20261125004932) adds. Those two are
	// applied with the seed's schema, and Migrate, which decides each migration on its own row,
	// then applies this one; none of them reads or writes another's column.
	all, err := migrations.All()
	if err != nil {
		t.Fatalf("read the embedded migrations: %v", err)
	}
	for _, migration := range all {
		if migration.Version != issuePriorityVersion && migration.Version != doneByChecksVersion {
			continue
		}
		if _, err := store.apply(ctx, migration); err != nil {
			t.Fatalf("apply %s: %v", migration.Name, err)
		}
	}
	records := record.NewStore()
	root := func(key, status string, generation uint64, at phase.Phase, linger *time.Time) record.Issue {
		return record.Issue{Key: key, Tree: key, Project: "LEGION", Title: key, Phase: at, Generation: generation, Status: status,
			DispatchStatus: status, Rank: "A", HandedOver: true, LingerUntil: linger, Repo: "acme/widgets", DefaultBranch: "main",
			GitHubRepo: "acme/widgets"}
	}
	parent := "LEGION-1"
	lingers := time.Date(2026, 11, 23, 18, 0, 0, 0, time.UTC)
	approved := 3
	seed := []record.Issue{
		root("LEGION-1", "todo", 2, phase.Admitted, nil),         // queued after its re-admission: owed
		root("LEGION-2", "in_progress", 2, phase.Admitted, nil),  // promoted by the release before
		root("LEGION-3", "todo", 2, phase.Admitted, nil),         // gate opened first, its tree's lifecycle open: owed
		root("LEGION-31", "todo", 2, phase.Admitted, nil),        // spec opened, criteria not yet: owed
		root("LEGION-4", "todo", 2, phase.Admitted, nil),         // gate opened first, its tree's cleanup reserved: owed
		root("LEGION-5", "todo", 1, phase.Admitted, nil),         // a root that never ran
		root("LEGION-6", "todo", 2, phase.Admitted, nil),         // promoted, then set back to todo, holding its slot
		root("LEGION-62", "todo", 2, phase.Admitted, nil),        // promoted, then released for a wait and set back to todo
		root("LEGION-7", "backlog", 2, phase.Admitted, &lingers), // parked again, lingering
		root("LEGION-8", "todo", 2, phase.Planning, nil),         // its first phase started
		{Key: "LEGION-9", Tree: "LEGION-1", Project: "LEGION", Title: "child", Parent: &parent, Phase: phase.Admitted, Generation: 1,
			Status: "todo", DispatchStatus: "todo", Rank: "B", Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets"},
	}
	if err := Transact(ctx, store.pool, func(ctx context.Context, tx pgx.Tx) error {
		for _, issue := range seed {
			if err := records.PutIssue(ctx, tx, issue); err != nil {
				return err
			}
		}
		for _, key := range []string{"LEGION-2", "LEGION-3", "LEGION-31", "LEGION-4", "LEGION-6"} {
			if _, err := treelifecycle.Open(ctx, tx, "legion", key, treelifecycle.AuthorityWorkflow); err != nil {
				return err
			}
		}
		if _, err := treelifecycle.ReserveCleanup(ctx, tx, "legion", "LEGION-4", treelifecycle.AuthorityWorkflow); err != nil {
			return err
		}
		for _, key := range []string{"LEGION-2", "LEGION-6"} {
			slots, err := records.Slots(ctx, tx)
			if err != nil {
				return err
			}
			if err := records.PutSlot(ctx, tx, record.Slot{Issue: key, Index: len(slots), AdmittedAt: lingers}); err != nil {
				return err
			}
		}
		for _, key := range []string{"LEGION-3", "LEGION-31", "LEGION-4"} {
			if err := records.PutGate(ctx, tx, record.DesignGate{Issue: key, Step: record.DesignStepSpec, ArtifactID: "artifact-" + key, LatestVersion: 3,
				ApprovedVersion: &approved, EverOpened: true}); err != nil {
				return err
			}
		}
		if err := records.PutGate(ctx, tx, record.DesignGate{Issue: "LEGION-31", Step: record.DesignStepCriteria, ArtifactID: "criteria-LEGION-31",
			LatestVersion: 1}); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `update issues set released_for_wait_at = $2 where key = $1`, "LEGION-62", lingers); err != nil {
			return err
		}
		return nil
	}); err != nil {
		t.Fatalf("seed the release before's records: %v", err)
	}

	if _, err := store.Migrate(ctx); err != nil {
		t.Fatalf("migrate through the members' mark: %v", err)
	}
	const owed = `select key || ' ' || members_owed::text from issues order by key`
	want := []string{"LEGION-1 true", "LEGION-2 false", "LEGION-3 true", "LEGION-31 true", "LEGION-4 true", "LEGION-5 false",
		"LEGION-6 false", "LEGION-62 false", "LEGION-7 false", "LEGION-8 false", "LEGION-9 false"}
	if got := rowsAsText(t, store, owed); !reflect.DeepEqual(got, want) {
		t.Fatalf("the marks after the migration = %v, want %v", got, want)
	}

	if _, err := store.Pool().Exec(ctx, wayBackOf(t, membersOwedVersion, membersOwedRelease)); err != nil {
		t.Fatalf("run the way back: %v", err)
	}
	const column = `select count(*)::text from information_schema.columns where table_name = 'issues' and column_name = 'members_owed'`
	const version = `select count(*)::text from schema_version where version = 20261123224717`
	if got := append(rowsAsText(t, store, column), rowsAsText(t, store, version)...); !reflect.DeepEqual(got, []string{"0", "0"}) {
		t.Fatalf("after the way back the column and the version row = %v, want neither", got)
	}
	if _, err := store.Migrate(ctx); err != nil {
		t.Fatalf("roll forward after the way back: %v", err)
	}
	if got := rowsAsText(t, store, owed); !reflect.DeepEqual(got, want) {
		t.Fatalf("the marks after the roll-forward = %v, want %v", got, want)
	}
}
