package store

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/sjawhar/legion/daemon/internal/record"
)

// approvalRouteVersion is the version of the migration this file tests, so the test can migrate
// through everything before it whatever lands beside it.
const approvalRouteVersion = 20261120210115

// approvalRouteRelease names the release whose way back the migration's header carries, as a
// failure to read that block names it.
const approvalRouteRelease = "the release that answers the approval route"

// 20261120210115 adds the two facts the approval route reads that no row held: the root a carried
// gate came from (handed_over_roots.from_root) and the spec approval a root's re-admission cleared
// (cleared_spec_approvals). A database the release before left holds neither, and one this
// migration took stores both and reads them back, a hand-over row the release before wrote taking
// the default ”. The way back, the last paragraph of the migration's header (migrations.WayBack),
// drops the table and the column and deletes the migration's own version row, and leaves every
// column of handed_over_roots the release before reads and writes as it was. The roll-forward runs
// the migration again: the column comes back at its default and the table comes back empty.
func TestTheApprovalRouteRecordsAndTheirWayBackLeaveWhatTheReleaseBeforeReads(t *testing.T) {
	ctx := context.Background()
	store := emptyStore(t)
	migrateThrough(t, store, approvalRouteVersion-1)
	refusedAs := func(err error, code string) bool {
		var pgErr *pgconn.PgError
		return errors.As(err, &pgErr) && pgErr.Code == code
	}
	const fromRoots = `select from_root from handed_over_roots`
	const clearedApprovals = `select issue from cleared_spec_approvals`
	wantNeither := func(when string) {
		t.Helper()
		if _, err := store.Pool().Exec(ctx, fromRoots); !refusedAs(err, "42703") {
			t.Fatalf("%s handed_over_roots.from_root = %v, want no such column (42703)", when, err)
		}
		if _, err := store.Pool().Exec(ctx, clearedApprovals); !refusedAs(err, "42P01") {
			t.Fatalf("%s cleared_spec_approvals = %v, want no such table (42P01)", when, err)
		}
	}
	wantNeither("the database the release before left reads")

	root := record.Issue{Key: "LEGION-208", Tree: "LEGION-208", Project: "LEGION", Title: "root", Phase: "admitted", Generation: 3,
		Status: "in_progress", Rank: "U", Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets"}
	carried := record.Issue{Key: "LEGION-209", Tree: "LEGION-209", Project: "LEGION", Title: "carried", Phase: "admitted", Generation: 2,
		Status: "in_progress", Rank: "V", Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets"}
	if err := store.Tx(ctx, func(tx pgx.Tx) error {
		for _, issue := range []record.Issue{root, carried} {
			if err := record.NewStore().PutIssue(ctx, tx, issue); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("record the issues: %v", err)
	}
	// The release before's own write of a hand-over, which names each column it knows.
	if _, err := store.Pool().Exec(ctx, `insert into handed_over_roots (issue, generation, gate_carried) values ('LEGION-208', 3, false)`); err != nil {
		t.Fatalf("write the release before's hand-over: %v", err)
	}

	if _, err := store.Migrate(ctx); err != nil {
		t.Fatalf("migrate through the approval route's records: %v", err)
	}
	handed := record.HandedOverRoot{Issue: "LEGION-209", Generation: 2, GateCarried: true, FromRoot: "LEGION-208"}
	cleared := record.ClearedSpecApproval{Issue: "LEGION-208", ArtifactID: "artifact-208", Version: 4, Mark: record.EventMark{Seq: 12}}
	if err := store.Tx(ctx, func(tx pgx.Tx) error {
		if err := record.NewStore().PutHandedOverRoot(ctx, tx, handed); err != nil {
			return err
		}
		return record.NewStore().PutClearedSpecApproval(ctx, tx, cleared)
	}); err != nil {
		t.Fatalf("record the carried root and the cleared approval: %v", err)
	}
	if err := store.Tx(ctx, func(tx pgx.Tx) error {
		before, err := record.NewStore().HandedOverRoot(ctx, tx, "LEGION-208")
		if err != nil {
			return err
		}
		if want := (record.HandedOverRoot{Issue: "LEGION-208", Generation: 3}); before == nil || *before != want {
			t.Fatalf("the release before's hand-over reads back %+v, want %+v: the column's default", before, want)
		}
		read, err := record.NewStore().HandedOverRoot(ctx, tx, "LEGION-209")
		if err != nil {
			return err
		}
		if read == nil || *read != handed {
			t.Fatalf("the carried root reads back %+v, want %+v", read, handed)
		}
		approval, err := record.NewStore().ClearedSpecApproval(ctx, tx, "LEGION-208")
		if err != nil {
			return err
		}
		if approval == nil || !reflect.DeepEqual(*approval, cleared) {
			t.Fatalf("the cleared approval reads back %+v, want %+v", approval, cleared)
		}
		return nil
	}); err != nil {
		t.Fatalf("read the records back: %v", err)
	}
	const releaseBeforesColumns = `select row(issue, generation, gate_carried)::text from handed_over_roots order by issue`
	kept := rowsAsText(t, store, releaseBeforesColumns)

	if _, err := store.Pool().Exec(ctx, wayBackOf(t, approvalRouteVersion, approvalRouteRelease)); err != nil {
		t.Fatalf("run the way back: %v", err)
	}

	wantNeither("after the way back the database reads")
	if after := rowsAsText(t, store, releaseBeforesColumns); !reflect.DeepEqual(after, kept) {
		t.Fatalf("after the way back the hand-overs read %v, want the columns the release before reads as they were, %v", after, kept)
	}
	var recorded bool
	if err := store.Pool().QueryRow(ctx, `select exists (select 1 from schema_version where version = $1)`,
		approvalRouteVersion).Scan(&recorded); err != nil || recorded {
		t.Fatalf("the way back left the version row recorded = %v (%v), want it deleted", recorded, err)
	}

	if _, err := store.Migrate(ctx); err != nil {
		t.Fatalf("migrate forward again: %v", err)
	}
	if got, want := rowsAsText(t, store, fromRoots+` order by issue`), []string{"", ""}; !reflect.DeepEqual(got, want) {
		t.Fatalf("after the roll-forward the hand-overs name the roots %q, want the default for each, %q", got, want)
	}
	if approvals := rowsAsText(t, store, clearedApprovals); len(approvals) != 0 {
		t.Fatalf("after the roll-forward cleared_spec_approvals holds %v, want it empty", approvals)
	}
}
