package graphcensus

import (
	"context"
	"io"
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

// currentAnchoredIn is every anchored_in comment's node states now.
func currentAnchoredIn(t *testing.T, database *store.Store, comment string) []graphstore.Tuple {
	t.Helper()
	held, err := graphstore.CurrentOut(context.Background(), database.Pool, graphstore.KindComment, comment, []string{graphstore.TupleAnchoredIn})
	if err != nil {
		t.Fatalf("read the anchored_in of comment %s: %v", comment, err)
	}
	return held
}

// backfillAnchors runs the whole backfill and answers its line for the comments' anchor blocks.
func backfillAnchors(t *testing.T, database *store.Store) KindBackfill {
	t.Helper()
	report, err := Backfill(context.Background(), database, io.Discard)
	if err != nil {
		t.Fatalf("backfill the store: %v", err)
	}
	return reportedKind(t, report, anchorProjectionKind)
}

// requireOneAnchorDifference holds the census to reading exactly one difference, of the anchor
// kind, at the row or the tuple at names with cause, and none of any other kind.
func requireOneAnchorDifference(t *testing.T, what string, report *StoreReport, node bool, cause, at string) {
	t.Helper()
	for _, kind := range report.Kinds {
		if kind.Kind != anchorProjectionKind {
			if kind.RowDifferences != 0 || kind.NodeDifferences != 0 {
				t.Errorf("%s: the census reads %d row and %d node difference(s) of kind %s (%+v %+v), want none there",
					what, kind.RowDifferences, kind.NodeDifferences, kind.Kind, kind.ReportedRows, kind.ReportedNodes)
			}
			continue
		}
		found, other := kind.ReportedRows, kind.NodeDifferences
		count := kind.RowDifferences
		if node {
			found, other, count = kind.ReportedNodes, kind.RowDifferences, kind.NodeDifferences
		}
		if count != 1 || len(found) != 1 || found[0].Cause != cause || found[0].At != at || other != 0 {
			t.Errorf("%s: the census reads %d row and %d node difference(s) of kind %s (%+v %+v), want the one %q at %s",
				what, kind.RowDifferences, kind.NodeDifferences, kind.Kind, kind.ReportedRows, kind.ReportedNodes, cause, at)
		}
	}
	if !report.Refused() {
		t.Errorf("%s: the census does not refuse a store whose anchor blocks disagree with their rows", what)
	}
}

// The census holds comments.anchor's block_id to the anchored_in each comment's node states now,
// in both directions (T17b of dispatch://INFRA-1253's plan), and the backfill gives each stored
// anchor with a block its tuple. Over a store the census agrees with (seedAgreeingStore), a comment
// a build before the store existed wrote, anchored on the document's inner block, is given its node
// by the comment pass and its anchored_in to the inner block's node by the anchor pass, dated at the
// comment's own created_at, and the census then agrees, holding one row and one tuple of the kind.
// With the tuple retracted the census reads the row no tuple states, and the backfill states it
// again; with the row's anchor orphaned behind the store's back the census reads the tuple no row
// projects, and the backfill retracts it, after which the kind holds no row and no tuple and the
// census agrees, and a second backfill levels nothing.
func TestTheAnchorCensusHoldsEachAnchorBlockToItsAnchoredIn(t *testing.T) {
	ctx := context.Background()
	database := storetest.Open(t)
	seeded := seedAgreeingStore(t, database)
	var comment string
	var createdAt time.Time
	if err := database.Pool.QueryRow(ctx, `
		insert into comments (artifact_id, author, body, anchor, created_at)
		values ($1::uuid, '{"kind":"user","id":"alice"}', 'A remark on the inner block.',
		        jsonb_build_object('artifact_id', $1::text, 'block_id', 'inner', 'mark_id', 'm-inner', 'version', 1,
		                           'quote', 'Block inner.', 'orphaned', false),
		        now() - interval '2 days')
		returning id::text, created_at
	`, seeded.document.artifactID).Scan(&comment, &createdAt); err != nil {
		t.Fatalf("write the anchored comment the release before wrote: %v", err)
	}

	done := backfillAnchors(t, database)
	if done.Rows != 1 || done.Tuples != 1 || !done.Complete {
		t.Errorf("the anchor pass reports %#v, want one comment, one tuple and none left", done)
	}
	held := currentAnchoredIn(t, database, comment)
	if len(held) != 1 || held[0].ToKind != refs.BlockNodeKind || held[0].ToID != seeded.inner || !held[0].At.Equal(createdAt) {
		t.Fatalf("the backfilled comment states anchored_in %+v, want one to %s:%s dated at its created_at %s", held, refs.BlockNodeKind, seeded.inner, createdAt)
	}
	report := storeCensusOf(t, database)
	requireTheCensusAgrees(t, "the census after the anchor pass", report)
	if kind := censusKind(t, report, anchorProjectionKind); kind.Rows != 1 || kind.Nodes != 1 {
		t.Errorf("the anchor kind reads %d rows and %d tuples, want the one anchored comment's", kind.Rows, kind.Nodes)
	}

	appendTuple(t, database, graphstore.Tuple{
		FromKind: graphstore.KindComment, FromID: comment, Kind: graphstore.TupleAnchoredIn,
		ToKind: refs.BlockNodeKind, ToID: seeded.inner, Asserted: false,
	})
	requireOneAnchorDifference(t, "a retracted anchored_in", storeCensusOf(t, database), false, "a row no tuple states",
		"comment "+comment+" "+graphstore.TupleAnchoredIn+" -> "+refs.BlockNodeKind+" "+seeded.inner)
	if done := backfillAnchors(t, database); done.Rows != 1 || done.Tuples != 1 || !done.Complete {
		t.Errorf("the anchor pass over a retracted anchored_in reports %#v, want it stated again", done)
	}
	requireTheCensusAgrees(t, "the census once the anchored_in is stated again", storeCensusOf(t, database))

	if _, err := database.Pool.Exec(ctx, `
		update comments set anchor = jsonb_set(anchor, '{orphaned}', 'true') where id = $1::uuid
	`, comment); err != nil {
		t.Fatalf("orphan the anchor as a process of the release before does: %v", err)
	}
	requireOneAnchorDifference(t, "an orphaned anchor whose anchored_in stands", storeCensusOf(t, database), true, "a tuple no row projects",
		graphstore.KindComment+":"+comment+" "+graphstore.TupleAnchoredIn+" -> "+refs.BlockNodeKind+":"+seeded.inner)
	if done := backfillAnchors(t, database); done.Rows != 1 || done.Tuples != 1 || !done.Complete {
		t.Errorf("the anchor pass over an orphaned anchor reports %#v, want its anchored_in retracted", done)
	}
	if held := currentAnchoredIn(t, database, comment); len(held) != 0 {
		t.Errorf("the orphaned anchor's comment states anchored_in %+v, want none", held)
	}
	report = storeCensusOf(t, database)
	requireTheCensusAgrees(t, "the census once the orphaned anchor's anchored_in is retracted", report)
	if kind := censusKind(t, report, anchorProjectionKind); kind.Rows != 0 || kind.Nodes != 0 {
		t.Errorf("the anchor kind reads %d rows and %d tuples, want none", kind.Rows, kind.Nodes)
	}
	if done := backfillAnchors(t, database); done.Rows != 0 || done.Tuples != 0 || !done.Complete {
		t.Errorf("a second anchor pass reports %#v, want nothing levelled", done)
	}
}

// TestTheAnchorBatchTakesTheStoreLockOrder holds writeAnchorBatch (T17b of dispatch://INFRA-1253's
// plan) to the store's lock order, as TestEveryBackfillBatchTakesTheStoreLockOrder holds the other
// batches: the owner row the comment stands under, its issue's row, first, then the anchored_in. The
// tuple is stated at the sentinel, which no repeat key holds, so no hold stands on it, and the batch
// takes no project row. The comment is one this build wrote with its node, anchored on a block of
// its issue's document, whose node states no anchored_in, as a process of the release before leaves
// the row it anchors. After the run the comment states one anchored_in to the block's node, dated at
// the comment's created_at, and the backfill reports it levelled.
func TestTheAnchorBatchTakesTheStoreLockOrder(t *testing.T) {
	database := storetest.Open(t)
	ctx := context.Background()
	seedLockProject(t, database)
	alice := model.Actor{Kind: "user", ID: "alice"}
	var document, refKey, comment string
	var createdAt time.Time
	if err := pgx.BeginFunc(ctx, database.Pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `
			insert into issues (key, project_key, number, title, created_by, rank)
			values ('LOCK-1', 'LOCK', 1, 'Anchored', '{"kind":"user","id":"alice"}', 'U')
		`); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `
			insert into artifacts (issue_key, project_key, slug, name, kind, is_primary, created_by)
			values ('LOCK-1', 'LOCK', 'spec', 'spec.md', 'doc', true, '{"kind":"user","id":"alice"}')
			returning id::text, ref_key
		`).Scan(&document, &refKey); err != nil {
			return err
		}
		comment = "11111111-1111-4111-8111-111111111111"
		createdAt = time.Now().UTC().Add(-48 * time.Hour).Truncate(time.Microsecond)
		node, tuples := graphstore.CommentFacts(nil, comment, alice, createdAt, graphstore.NoEvent, refKey, "", "LOCK", nil)
		if err := graphstore.Write(ctx, tx, []graphstore.Node{node}, tuples); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `
			insert into comments (id, issue_key, author, body, anchor, node_kind, node_address, created_at)
			values ($1::uuid, 'LOCK-1', '{"kind":"user","id":"alice"}', 'A remark on the block.',
			        jsonb_build_object('artifact_id', $2::text, 'block_id', 'b1', 'mark_id', 'm-b1', 'version', 1,
			                           'quote', 'The block.', 'orphaned', false),
			        $3, $1::text, $4)
		`, comment, document, graphstore.KindComment, createdAt)
		return err
	}); err != nil {
		t.Fatalf("seed the anchored comment: %v", err)
	}

	owner, project := holdLockOwner(t, database)
	report := followTheRun(t, database, "writeAnchorBatch", onePass(backfillAnchoredIn), []*lockstep.Hold{owner, project})
	held := currentAnchoredIn(t, database, comment)
	if len(held) != 1 || held[0].ToID != refKey+"#b1" || !held[0].At.Equal(createdAt) {
		t.Errorf("the comment states anchored_in %+v, want one to %s:%s#b1 dated at its created_at %s: the run the test followed is not one that committed",
			held, refs.BlockNodeKind, refKey, createdAt)
	}
	if done := reportedKind(t, report, anchorProjectionKind); done.Rows != 1 || done.Tuples != 1 || !done.Complete {
		t.Errorf("the backfill reports %#v for anchor blocks, want 1 comment, 1 tuple and none left", done)
	}
}
