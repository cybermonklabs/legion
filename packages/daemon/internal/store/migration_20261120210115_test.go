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
// run after the way back of 20261122072401, which a later migration of the same table adds, drops
// the table and the column and deletes the migration's own version row, and leaves every column
// of handed_over_roots the release before reads and writes as it was. The roll-forward runs both
// migrations again: the column comes back at its default and the table comes back empty. The
// issues the rows are of are written before the migration with SQL that names the columns of the
// schema the migration finds, as the release before writes them; today's store writes an issue
// only once every migration has run.
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

	// The release before's own write of the two issues, which names each column it knows.
	for _, issue := range []struct {
		key, title string
		generation int
		rank       string
	}{{"LEGION-208", "root", 3, "U"}, {"LEGION-209", "carried", 2, "V"}} {
		if _, err := store.Pool().Exec(ctx, `insert into issues (key, tree, project, title, phase, generation, status, rank, last_dispatch_seq,
			repo, default_branch, github_repo)
			values ($1, $1, 'LEGION', $2, 'admitted', $3, 'in_progress', $4, 0, 'acme/widgets', 'main', 'acme/widgets')`,
			issue.key, issue.title, issue.generation, issue.rank); err != nil {
			t.Fatalf("record the issue %s: %v", issue.key, err)
		}
	}
	// The release before's own write of a hand-over, which names each column it knows.
	if _, err := store.Pool().Exec(ctx, `insert into handed_over_roots (issue, generation, gate_carried) values ('LEGION-208', 3, false)`); err != nil {
		t.Fatalf("write the release before's hand-over: %v", err)
	}

	if _, err := store.Migrate(ctx); err != nil {
		t.Fatalf("migrate through the approval route's records: %v", err)
	}
	handed := record.HandedOverRoot{Issue: "LEGION-209", Generation: 2, GateCarried: true, FromRoot: "LEGION-208"}
	cleared := record.ClearedSpecApproval{Issue: "LEGION-208", ArtifactID: "artifact-208", Version: 4, LatestVersion: 5, Mark: record.EventMark{Seq: 12}}
	if err := Transact(ctx, store.Pool(), func(ctx context.Context, tx pgx.Tx) error {
		if err := record.NewStore().PutHandedOverRoot(ctx, tx, handed); err != nil {
			return err
		}
		return record.NewStore().PutClearedSpecApproval(ctx, tx, cleared)
	}); err != nil {
		t.Fatalf("record the carried root and the cleared approval: %v", err)
	}
	if err := Transact(ctx, store.Pool(), func(ctx context.Context, tx pgx.Tx) error {
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

	clearedApprovalsWayBackFirst(t, store)
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
	if _, err := store.Pool().Exec(ctx, `select latest_version from cleared_spec_approvals`); err != nil {
		t.Fatalf("after the roll-forward cleared_spec_approvals.latest_version = %v, want the column back", err)
	}
}
