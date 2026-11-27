package graphcensus

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest/lockstep"
)

// resultLockDocument is the Verification document of LOCK-1 the results of these tests record a
// check of, and resultLockCheck the address of that check's node.
const (
	resultLockDocument = "LOCK-1/verification-md"
	resultLockCheck    = resultLockDocument + "#c1"
)

// seedLockResult writes issue LOCK-1 and its Verification document, each with its node, and one
// result an earlier release recorded against the document's check c1 three days ago, its row and
// no node, and answers the result's id and its row's created_at.
func seedLockResult(t *testing.T, database *store.Store) (string, time.Time) {
	t.Helper()
	ctx := context.Background()
	seedLockProject(t, database)
	alice := model.Actor{Kind: "user", ID: "alice"}
	var id string
	var createdAt time.Time
	drivenTx(t, database, "seed the issue, its Verification document and the earlier release's result", func(tx pgx.Tx) error {
		now := time.Now().UTC()
		if err := graphstore.Write(ctx, tx, []graphstore.Node{
			graphstore.IssueNode("LOCK-1", "LOCK", alice, now),
			graphstore.ArtifactNode(resultLockDocument, "LOCK", "", alice, now),
		}, []graphstore.Tuple{graphstore.ArtifactPartOf(resultLockDocument, "LOCK-1", alice, now)}); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `
			insert into issues (key, project_key, number, title, created_by, rank, node_kind, node_address)
			values ('LOCK-1', 'LOCK', 1, 'One', '{"kind":"user","id":"alice"}', 'U', 'issue', 'LOCK-1')
		`); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `
			with document as (
				insert into artifacts (issue_key, project_key, slug, name, kind, created_by, node_kind, node_address)
				values ('LOCK-1', 'LOCK', 'verification-md', 'verification.md', 'doc', '{"kind":"user","id":"alice"}', 'artifact', $1)
				returning id
			)
			insert into check_results (issue_key, check_issue_key, check_artifact_id, check_block_id, check_version,
				check_sha256, role, commit_sha, verdict, failed, actor, created_at)
			select 'LOCK-1', 'LOCK-1', document.id, 'c1', 1, 'lock-check-sum', 'implementer',
			       '1111111111111111111111111111111111111111', 'fail', 1, '{"kind":"user","id":"alice"}', now() - interval '3 days'
			from document
			returning id::text, created_at
		`, resultLockDocument).Scan(&id, &createdAt)
	})
	return id, createdAt
}

// TestTheResultBatchTakesTheStoreLockOrder holds writeResultBatch (T11 of the issue-tree plan) to
// the store's lock order, as TestEveryBackfillBatchTakesTheStoreLockOrder holds the other batches:
// the owner row the result stands under, its check's issue's row, first, then the result's node
// row, with the recorder's and the tuples in the same write. The result is one an earlier release
// recorded, a check_results row with no node, against the check of a document this build wrote,
// so no other pass reads a row of it. After the run the result's node stands, made at the row's
// own created_at, with its result_of to its check, and the backfill reports the result done.
func TestTheResultBatchTakesTheStoreLockOrder(t *testing.T) {
	database := storetest.Open(t)
	ctx := context.Background()
	id, createdAt := seedLockResult(t, database)
	alice := model.Actor{Kind: "user", ID: "alice"}
	nodes, _, ok := graphstore.ResultFacts(graphstore.Result{ID: id, Project: "LOCK", Check: resultLockCheck, Recorder: alice, At: createdAt})
	if !ok {
		t.Fatalf("the result %s states no node", id)
	}

	owner, project := holdLockOwner(t, database)
	node := holdWrite(t, database, &lockstep.Hold{
		Name: "the result's node row", Step: lockstep.StepNode, Taken: true, Kind: graphstore.KindResult,
		ID: id, Statement: "insert into graph_nodes (",
	}, nodes[:1], nil)

	report := followTheBackfill(t, database, "writeResultBatch", []*lockstep.Hold{owner, project, node})
	var madeAt time.Time
	if err := database.Pool.QueryRow(ctx, `select made_at from graph_nodes where kind = $1 and id = $2`, graphstore.KindResult, id).Scan(&madeAt); err != nil {
		t.Fatalf("read the result's node: %v", err)
	}
	checks, err := graphstore.CurrentOut(ctx, database.Pool, graphstore.KindResult, id, []string{graphstore.TupleResultOf})
	if err != nil {
		t.Fatalf("read the result's check: %v", err)
	}
	if !madeAt.Equal(createdAt) || len(checks) != 1 || checks[0].ToKind != refs.BlockNodeKind || checks[0].ToID != resultLockCheck {
		t.Errorf("the result's node is made at %s and states result_of %+v, want made at the row's %s and one result_of to block:%s",
			madeAt, checks, createdAt, resultLockCheck)
	}
	if done := reportedKind(t, report, resultProjectionKind); done.Rows != 1 || !done.Complete {
		t.Errorf("the backfill reports %#v for results, want 1 row and none remaining", done)
	}
}

// TestTheResultRetractionBatchTakesTheStoreLockOrder holds writeResultRetractionBatch (T11 of this
// branch's plan) to the store's lock order: the owner row the result stands under, its check's
// issue's row, first, then the retracted value, which writes no node. The value is stated at the
// sentinel, which no repeat key holds, so no hold stands on it; the owner row's hold reads that the
// run took no lock of a later step while it waited. The retraction is one an earlier release wrote
// of a result whose node stands. After the run the result carries its retracted value, the row's
// reason, dated at the row's created_at, and the backfill reports the retraction done.
func TestTheResultRetractionBatchTakesTheStoreLockOrder(t *testing.T) {
	database := storetest.Open(t)
	ctx := context.Background()
	id, createdAt := seedLockResult(t, database)
	alice := model.Actor{Kind: "user", ID: "alice"}
	nodes, tuples, _ := graphstore.ResultFacts(graphstore.Result{
		ID: id, Project: "LOCK", Check: resultLockCheck, Verdict: "fail", Role: "implementer",
		Commit: "1111111111111111111111111111111111111111", Failed: 1, CheckVersion: 1, CheckSHA256: "lock-check-sum",
		Recorder: alice, At: createdAt,
	})
	var retractedAt time.Time
	drivenTx(t, database, "seed the result's node and the earlier release's retraction", func(tx pgx.Tx) error {
		if err := graphstore.Write(ctx, tx, nodes, tuples); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `
			insert into check_result_retractions (result_id, reason, actor, created_at)
			values ($1::uuid, 'The run measured the wrong build.', '{"kind":"user","id":"alice"}', now() - interval '1 day')
			returning created_at
		`, id).Scan(&retractedAt)
	})

	owner, project := holdLockOwner(t, database)
	report := followTheBackfill(t, database, "writeResultRetractionBatch", []*lockstep.Hold{owner, project})
	var reason string
	var at time.Time
	if err := database.Pool.QueryRow(ctx, `
		select to_id, at from graph_tuples
		where from_kind = $1 and from_id = $2 and kind = $3 and to_kind = 'value' and asserted
		order by seq desc limit 1`, graphstore.KindResult, id, graphstore.AttributeRetracted).Scan(&reason, &at); err != nil {
		t.Fatalf("read the result's retracted value: %v", err)
	}
	if reason != "The run measured the wrong build." || !at.Equal(retractedAt) {
		t.Errorf("the result carries retracted %q at %s, want the row's reason at its created_at %s", reason, at, retractedAt)
	}
	if done := reportedKind(t, report, resultRetractionProjectionKind); done.Rows != 1 || !done.Complete {
		t.Errorf("the backfill reports %#v for retractions, want 1 row and none remaining", done)
	}
}
