package graphcensus

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest/lockstep"
)

// TestTheIssueLinkBatchTakesTheStoreLockOrder holds writeIssueLinkBatch (dispatch://INFRA-1253) to
// the store's lock order, as TestEveryBackfillBatchTakesTheStoreLockOrder holds the other batches:
// the owner row the link stands under, its issue's row, first, then the needs tuple, then the
// issue_links row that names the issue's node. The tuple is stated at the sentinel, which no repeat
// key holds, so no hold stands on it; the projection row's hold reads that the run took no lock of
// a later step while it waited. The link is one a process of the release before wrote, naming no
// node, between two issues this build wrote, so the object pass reads no row of them. After the
// run the row names its issue's node and the issue states one needs tuple to its target, dated at
// the row's own created_at, and the backfill reports the link done.
func TestTheIssueLinkBatchTakesTheStoreLockOrder(t *testing.T) {
	database := storetest.Open(t)
	ctx := context.Background()
	seedLockProject(t, database)
	alice := model.Actor{Kind: "user", ID: "alice"}
	if err := pgx.BeginFunc(ctx, database.Pool, func(tx pgx.Tx) error {
		now := time.Now().UTC()
		if err := graphstore.Write(ctx, tx, []graphstore.Node{
			graphstore.IssueNode("LOCK-1", "LOCK", alice, now), graphstore.IssueNode("LOCK-2", "LOCK", alice, now),
		}, nil); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `
			insert into issues (key, project_key, number, title, created_by, rank, node_kind, node_address) values
				('LOCK-1', 'LOCK', 1, 'Waits', '{"kind":"user","id":"alice"}', 'U', 'issue', 'LOCK-1'),
				('LOCK-2', 'LOCK', 2, 'Waited on', '{"kind":"user","id":"alice"}', 'V', 'issue', 'LOCK-2')
		`)
		return err
	}); err != nil {
		t.Fatalf("seed the two issues: %v", err)
	}
	var createdAt time.Time
	if err := database.Pool.QueryRow(ctx, `
		insert into issue_links (issue_key, kind, target_key, created_at)
		values ('LOCK-1', 'blocked_by', 'LOCK-2', now() - interval '3 days')
		returning created_at
	`).Scan(&createdAt); err != nil {
		t.Fatalf("seed the link the release before wrote: %v", err)
	}

	owner, project := holdLockOwner(t, database)
	row := lockstep.HoldRow(t, database, &lockstep.Hold{
		Name: "the link's issue_links row", Step: lockstep.StepProjection, Taken: true,
		Statement: "update issue_links l set node_kind",
	}, `select 1 from issue_links where issue_key = $1 and target_key = $2 for no key update`, "LOCK-1", "LOCK-2")

	report := followTheBackfill(t, database, "writeIssueLinkBatch", []*lockstep.Hold{owner, project, row})
	if kind, address := rowMarker(t, database,
		`select node_kind, node_address from issue_links where issue_key = $1 and target_key = 'LOCK-2'`, "LOCK-1"); kind != graphstore.KindIssue || address != "LOCK-1" {
		t.Errorf("the link names node %q/%q, want %s/LOCK-1: the run the test followed is not one that committed", kind, address, graphstore.KindIssue)
	}
	needs, err := graphstore.CurrentOut(ctx, database.Pool, graphstore.KindIssue, "LOCK-1", []string{graphstore.TupleNeeds})
	if err != nil {
		t.Fatalf("read LOCK-1's needs: %v", err)
	}
	if len(needs) != 1 || needs[0].ToKind != graphstore.KindIssue || needs[0].ToID != "LOCK-2" || !needs[0].At.Equal(createdAt) {
		t.Errorf("LOCK-1 states needs %+v, want one tuple to issue:LOCK-2 dated at the row's created_at %s", needs, createdAt)
	}
	if done := reportedKind(t, report, "issue_link"); done.Rows != 1 || done.Tuples != 1 || !done.Complete {
		t.Errorf("the backfill reports %#v for issue links, want 1 row, 1 tuple and every row marked", done)
	}
}

// TestTheObjectPassStatesTheParentAReparentMovedWhileItWaited holds the object pass to the parent an
// issue's row names under the issue's own row (dispatch://INFRA-1253 R9): a batch reads its rows
// outside its transaction, so a reparent can commit between that read and the issue's lock. A
// second transaction stands for the release before's PATCH: it takes the issue's row, as
// lockPatchRows does, and moves the issue, whose row names no node, from LOCK-1 to LOCK-2. That
// release is in the fleet, as it is wherever its PATCH runs, so the batch's owner lock takes the
// issue's row beside its advisory lock (store.LockOwners), as that release serialises on the row.
// The backfill starts while that transaction is open, so its batch reads LOCK-1; the test reads
// the run waiting at the issue's row, and only then lets the reparent commit. The issue's row then
// names its node, which states one child_of, to LOCK-2, the parent its row names, and graph_edges
// answers LOCK-2 once and LOCK-1 nowhere. A pass that built child_of from its first read would
// hang the issue under LOCK-1 in the store while its row names LOCK-2, and graph_edges would
// answer neither.
// A second issue, LOCK-4, whose marked row the release before moved in place from LOCK-1 to LOCK-2
// before the run, leaving its node's child_of at LOCK-1, is read as unmarked by the same pass: its
// node then states LOCK-2 alone, the claim retracting LOCK-1.
func TestTheObjectPassStatesTheParentAReparentMovedWhileItWaited(t *testing.T) {
	database := storetest.Open(t)
	ctx := context.Background()
	seedLockProject(t, database)
	alice := model.Actor{Kind: "user", ID: "alice"}
	if err := pgx.BeginFunc(ctx, database.Pool, func(tx pgx.Tx) error {
		now := time.Now().UTC()
		if err := graphstore.Write(ctx, tx, []graphstore.Node{
			graphstore.IssueNode("LOCK-1", "LOCK", alice, now), graphstore.IssueNode("LOCK-2", "LOCK", alice, now),
			graphstore.IssueNode("LOCK-4", "LOCK", alice, now),
		}, []graphstore.Tuple{graphstore.IssueChildOf("LOCK-4", "LOCK-1", alice, now)}); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `
			insert into issues (key, project_key, number, title, created_by, rank, parent_key, node_kind, node_address) values
				('LOCK-1', 'LOCK', 1, 'The first parent', '{"kind":"user","id":"alice"}', 'U', null, 'issue', 'LOCK-1'),
				('LOCK-2', 'LOCK', 2, 'The second parent', '{"kind":"user","id":"alice"}', 'V', null, 'issue', 'LOCK-2'),
				('LOCK-3', 'LOCK', 3, 'Moved while the pass waited', '{"kind":"user","id":"alice"}', 'W', 'LOCK-1', null, null),
				('LOCK-4', 'LOCK', 4, 'Moved in place before the pass', '{"kind":"user","id":"alice"}', 'X', 'LOCK-2', 'issue', 'LOCK-4')
		`)
		return err
	}); err != nil {
		t.Fatalf("seed the issues: %v", err)
	}
	// A process of the release before, registered as that release registers: no
	// owner_lock_protocol, so the column's default 0, which keeps it in the owner lock's fleet.
	if _, err := database.Pool.Exec(ctx, `
		insert into server_processes (instance, commit, signing_keys)
		values (gen_random_uuid(), 'earlier0build', '{fingerprint}')`); err != nil {
		t.Fatalf("register a process of the release before: %v", err)
	}

	reparent, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the reparent: %v", err)
	}
	defer reparent.Rollback(ctx)
	for _, statement := range []struct{ what, sql string }{
		{"take the issue's row", `select parent_key from issues where key = 'LOCK-3' for no key update`},
		{"move the issue", `update issues set parent_key = 'LOCK-2' where key = 'LOCK-3'`},
	} {
		if _, err := reparent.Exec(ctx, statement.sql); err != nil {
			t.Fatalf("the reparent could not %s: %v", statement.what, err)
		}
	}

	pid, answered := startBackfill(t, database)
	if early, done := storetest.WaitForQueuedBehind(t, reparent, pid, answered); done {
		t.Fatalf("the backfill answered (report %#v, err %v) without waiting on the reparent's row", early.report, early.err)
	}
	t.Logf("the backfill waits behind the reparent at: %s", lockstep.WaitingStatement(t, database, pid))
	if err := reparent.Commit(ctx); err != nil {
		t.Fatalf("commit the reparent: %v", err)
	}
	report := awaitBackfill(t, answered)

	for _, issue := range []string{"LOCK-3", "LOCK-4"} {
		if kind, address := rowMarker(t, database, `select node_kind, node_address from issues where key = $1`, issue); kind != graphstore.KindIssue || address != issue {
			t.Errorf("%s's row names node %q/%q, want %s/%s", issue, kind, address, graphstore.KindIssue, issue)
		}
		parents, err := graphstore.CurrentOut(ctx, database.Pool, graphstore.KindIssue, issue, []string{graphstore.TupleChildOf})
		if err != nil {
			t.Fatalf("read %s's child_of: %v", issue, err)
		}
		if len(parents) != 1 || parents[0].ToKind != graphstore.KindIssue || parents[0].ToID != "LOCK-2" {
			t.Errorf("%s states child_of %+v, want one tuple to issue:LOCK-2, the parent its row names", issue, parents)
		}
		for parent, want := range map[string]int{"LOCK-2": 1, "LOCK-1": 0} {
			var rows int
			if err := database.Pool.QueryRow(ctx, `
				select count(*) from graph_edges
				where from_kind = 'issue' and from_id = $1 and kind = 'child_of' and to_kind = 'issue' and to_id = $2
			`, issue, parent).Scan(&rows); err != nil {
				t.Fatalf("read %s's parent %s out of graph_edges: %v", issue, parent, err)
			}
			if rows != want {
				t.Errorf("graph_edges answers %s's parent %s %d times, want %d", issue, parent, rows, want)
			}
		}
	}
	if done := reportedKind(t, report, graphstore.KindIssue); done.Rows != 2 || !done.Complete {
		t.Errorf("the backfill reports %#v for issues, want its 2 rows and every row marked", done)
	}
}
