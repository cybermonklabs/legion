package store

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/record"
)

// workHoldOpeningsVersion is the version of the migration this file tests, so the test can
// migrate through everything before it whatever lands beside it.
const workHoldOpeningsVersion = 20261122023421

// workHoldOpeningsRelease names the release whose way back the migration's header carries, as a
// failure to read that block names it.
const workHoldOpeningsRelease = "the release that records the last opening of each tree's work hold"

// 20261122023421 adds work_hold_openings, the record of the last opening of each tree's work hold
// (record.WorkHold.OpenedAt, Opening): a database the release before left has no such table, and
// one this migration took keeps, at the opening, what the tree's work_holds row named and what
// ended the hold, while work_holds holds no row of the tree. The way back, the last paragraph of
// the migration's header (migrations.WayBack), drops the table and deletes the migration's own
// version row and leaves work_holds, which the release before reads, as it was. The roll-forward
// runs the migration again and finds the table empty.
func TestTheWorkHoldOpeningsRecordAndItsWayBackLeaveWhatTheReleaseBeforeReads(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := emptyStore(t)
	migrateThrough(t, store, workHoldOpeningsVersion-1)
	if _, err := store.Pool().Exec(ctx, `select 1 from work_hold_openings`); !undefinedTable(err) {
		t.Fatalf("the database the release before left read work_hold_openings = %v, want no such table (42P01): the migration would prove nothing", err)
	}
	if _, err := store.Migrate(ctx); err != nil {
		t.Fatalf("migrate through the record of the work hold's openings: %v", err)
	}
	records := record.NewStore()
	root := record.Issue{Key: "LEGION-208", Tree: "LEGION-208", Project: "LEGION", Title: "root", Phase: "admitted", Generation: 4,
		Status: "in_progress", Rank: "U", Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets"}
	other := root
	other.Key, other.Tree = "LEGION-300", "LEGION-300"
	closed := record.WorkHold{Project: "LEGION", Tree: "LEGION-208", RootGeneration: 4, Step: record.DesignStepSpec,
		ArtifactID: "artifact-208", Version: 4, ClosedAt: time.Date(2026, 11, 22, 2, 30, 0, 0, time.UTC)}
	held := closed
	held.Tree = "LEGION-300"
	opened := closed
	opened.OpenedAt, opened.Opening = time.Date(2026, 11, 22, 2, 34, 21, 0, time.UTC), "The design gate of LEGION-208 opened."
	if err := Transact(ctx, store.Pool(), func(ctx context.Context, tx pgx.Tx) error {
		for _, issue := range []record.Issue{root, other} {
			if err := records.PutIssue(ctx, tx, issue); err != nil {
				return err
			}
		}
		for _, hold := range []record.WorkHold{closed, held} {
			if err := records.PutWorkHold(ctx, tx, hold); err != nil {
				return err
			}
		}
		return records.OpenWorkHold(ctx, tx, "LEGION-208", opened.OpenedAt, opened.Opening)
	}); err != nil {
		t.Fatalf("record the hold and its opening: %v", err)
	}
	if err := Transact(ctx, store.Pool(), func(ctx context.Context, tx pgx.Tx) error {
		read, err := records.TreeWorkHold(ctx, tx, "LEGION-208")
		if err == nil && (read == nil || !reflect.DeepEqual(*read, opened)) {
			t.Fatalf("the opened hold reads back %+v, want %+v", read, opened)
		}
		return err
	}); err != nil {
		t.Fatalf("read the opened hold: %v", err)
	}

	if _, err := store.Pool().Exec(ctx, wayBackOf(t, workHoldOpeningsVersion, workHoldOpeningsRelease)); err != nil {
		t.Fatalf("run the way back: %v", err)
	}

	var table, recorded bool
	if err := store.Pool().QueryRow(ctx, `select to_regclass('work_hold_openings') is not null,
		exists (select 1 from schema_version where version = $1)`, workHoldOpeningsVersion).Scan(&table, &recorded); err != nil || table || recorded {
		t.Fatalf("after the way back the table stands = %v and the version row = %v (%v), want neither", table, recorded, err)
	}
	if got := rowsAsText(t, store, `select tree from work_holds order by tree`); !reflect.DeepEqual(got, []string{"LEGION-300"}) {
		t.Fatalf("after the way back work_holds holds %v, want the hold that stands, LEGION-300's, as it was", got)
	}
	if _, err := store.Migrate(ctx); err != nil {
		t.Fatalf("migrate forward again: %v", err)
	}
	if openings := rowsAsText(t, store, `select tree from work_hold_openings`); len(openings) != 0 {
		t.Fatalf("after the roll-forward work_hold_openings holds %v, want it empty", openings)
	}
}
