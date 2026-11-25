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

// clearedLatestVersion is the version of the migration this file tests, so the test can migrate
// through everything before it whatever lands beside it.
const clearedLatestVersion = 20261122072401

// clearedLatestRelease names the release whose way back the migration's header carries, as a
// failure to read that block names it.
const clearedLatestRelease = "the release that keeps the latest version of a cleared spec approval"

// clearedApprovalsWayBackFirst runs the way back of 20261122072401 over store, which the header
// of the migration says to run before the way back of 20261120210115_approval_route_records.
func clearedApprovalsWayBackFirst(t *testing.T, store *Store) {
	t.Helper()
	if _, err := store.Pool().Exec(context.Background(), wayBackOf(t, clearedLatestVersion, clearedLatestRelease)); err != nil {
		t.Fatalf("run the way back of %d: %v", clearedLatestVersion, err)
	}
}

// 20261122072401 adds cleared_spec_approvals.latest_version, the newest version of the document
// that the record of a spec approval a root's re-admission cleared knows of
// (record.ClearedSpecApproval.LatestVersion). A record the release before wrote takes its approved
// version, one this migration took stores its latest version and reads it back, and the table
// refuses a latest version below the approved one (23514). The way back, the last paragraph of the
// migration's header (migrations.WayBack), drops the column and deletes the migration's own
// version row, and leaves every column the release before reads and writes as it was, so that
// release's write, which names no latest version, works again. The roll-forward runs the migration
// again, and each record takes its approved version as its latest version once more. The issues
// the records are of are written before the migration with SQL that names the columns of the
// schema the migration finds, as the release before writes them; today's store writes an issue
// only once every migration has run.
func TestTheClearedSpecApprovalsLatestVersionAndItsWayBackLeaveWhatTheReleaseBeforeReads(t *testing.T) {
	ctx := context.Background()
	store := emptyStore(t)
	migrateThrough(t, store, clearedLatestVersion-1)
	refusedAs := func(err error, code string) bool {
		var pgErr *pgconn.PgError
		return errors.As(err, &pgErr) && pgErr.Code == code
	}
	const latest = `select latest_version from cleared_spec_approvals`
	if _, err := store.Pool().Exec(ctx, latest); !refusedAs(err, "42703") {
		t.Fatalf("the database the release before left read cleared_spec_approvals.latest_version = %v, want no such column (42703): the migration would prove nothing", err)
	}
	records := record.NewStore()
	// The release before's own write of the two issues, which names each column it knows.
	for _, key := range []string{"LEGION-208", "LEGION-300"} {
		if _, err := store.Pool().Exec(ctx, `insert into issues (key, tree, project, title, phase, generation, status, rank, last_dispatch_seq,
			repo, default_branch, github_repo)
			values ($1, $1, 'LEGION', $1, 'admitted', 3, 'in_progress', 'U', 0, 'acme/widgets', 'main', 'acme/widgets')`, key); err != nil {
			t.Fatalf("record the issue %s: %v", key, err)
		}
	}
	// The release before's own write of a record, which names each column it knows.
	const releaseBeforesWrite = `insert into cleared_spec_approvals (issue, artifact_id, approved_version, approved_seq, approved_at) values ($1, $2, $3, $4, null)
		on conflict (issue) do update set artifact_id = excluded.artifact_id, approved_version = excluded.approved_version,
		approved_seq = excluded.approved_seq, approved_at = excluded.approved_at`
	if _, err := store.Pool().Exec(ctx, releaseBeforesWrite, "LEGION-208", "artifact-208", 2, 12); err != nil {
		t.Fatalf("write the release before's record: %v", err)
	}

	if _, err := store.Migrate(ctx); err != nil {
		t.Fatalf("migrate through the latest version of the cleared spec approvals: %v", err)
	}
	backfilled := record.ClearedSpecApproval{Issue: "LEGION-208", ArtifactID: "artifact-208", Version: 2, LatestVersion: 2, Mark: record.EventMark{Seq: 12}}
	written := record.ClearedSpecApproval{Issue: "LEGION-300", ArtifactID: "artifact-300", Version: 2, LatestVersion: 4, Mark: record.EventMark{Seq: 30}}
	if err := Transact(ctx, store.Pool(), func(ctx context.Context, tx pgx.Tx) error { return records.PutClearedSpecApproval(ctx, tx, written) }); err != nil {
		t.Fatalf("record the cleared approval with its latest version: %v", err)
	}
	readBack := func(when string, want ...record.ClearedSpecApproval) {
		t.Helper()
		if err := Transact(ctx, store.Pool(), func(ctx context.Context, tx pgx.Tx) error {
			for _, approval := range want {
				got, err := records.ClearedSpecApproval(ctx, tx, approval.Issue)
				if err != nil {
					return err
				}
				if got == nil || !reflect.DeepEqual(*got, approval) {
					t.Fatalf("%s the record of %s reads back %+v, want %+v", when, approval.Issue, got, approval)
				}
			}
			return nil
		}); err != nil {
			t.Fatalf("%s read the records back: %v", when, err)
		}
	}
	readBack("after the migration", backfilled, written)
	if _, err := store.Pool().Exec(ctx, `update cleared_spec_approvals set latest_version = 1 where issue = 'LEGION-208'`); !refusedAs(err, "23514") {
		t.Fatalf("a latest version below the approved one = %v, want the table's check to refuse it (23514)", err)
	}
	// The release before's write names no latest version, so the table refuses it until the way
	// back has run, which is why the header says to run the way back before that release starts.
	if _, err := store.Pool().Exec(ctx, releaseBeforesWrite, "LEGION-300", "artifact-300", 3, 31); !refusedAs(err, "23502") {
		t.Fatalf("before the way back the release before's write = %v, want the column's not null to refuse it (23502)", err)
	}
	const releaseBeforesColumns = `select row(issue, artifact_id, approved_version, approved_seq, approved_at)::text from cleared_spec_approvals order by issue`
	kept := rowsAsText(t, store, releaseBeforesColumns)

	clearedApprovalsWayBackFirst(t, store)

	if _, err := store.Pool().Exec(ctx, latest); !refusedAs(err, "42703") {
		t.Fatalf("after the way back cleared_spec_approvals.latest_version = %v, want no such column (42703)", err)
	}
	if after := rowsAsText(t, store, releaseBeforesColumns); !reflect.DeepEqual(after, kept) {
		t.Fatalf("after the way back the records read %v, want the columns the release before reads as they were, %v", after, kept)
	}
	var recorded bool
	if err := store.Pool().QueryRow(ctx, `select exists (select 1 from schema_version where version = $1)`,
		clearedLatestVersion).Scan(&recorded); err != nil || recorded {
		t.Fatalf("the way back left the version row recorded = %v (%v), want it deleted", recorded, err)
	}
	if _, err := store.Pool().Exec(ctx, releaseBeforesWrite, "LEGION-300", "artifact-300", 3, 31); err != nil {
		t.Fatalf("after the way back the release before's write = %v, want it to write", err)
	}

	if _, err := store.Migrate(ctx); err != nil {
		t.Fatalf("migrate forward again: %v", err)
	}
	readBack("after the roll-forward", backfilled,
		record.ClearedSpecApproval{Issue: "LEGION-300", ArtifactID: "artifact-300", Version: 3, LatestVersion: 3, Mark: record.EventMark{Seq: 31}})
}
