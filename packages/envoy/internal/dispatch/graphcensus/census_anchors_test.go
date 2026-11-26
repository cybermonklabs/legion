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

// currentAnchorPair is every anchored_in and every part_of comment's node states now, by kind.
func currentAnchorPair(t *testing.T, database *store.Store, comment string) map[string][]graphstore.Tuple {
	t.Helper()
	pair := map[string][]graphstore.Tuple{}
	for _, kind := range []string{graphstore.TupleAnchoredIn, graphstore.TuplePartOf} {
		held, err := graphstore.CurrentOut(context.Background(), database.Pool, graphstore.KindComment, comment, []string{kind})
		if err != nil {
			t.Fatalf("read the %s of comment %s: %v", kind, comment, err)
		}
		pair[kind] = held
	}
	return pair
}

// requireAnchorPairAt fails t unless comment's node states one anchored_in and one part_of, each
// to the block node block and dated at at, naming what.
func requireAnchorPairAt(t *testing.T, what string, database *store.Store, comment, block string, at time.Time) {
	t.Helper()
	for kind, held := range currentAnchorPair(t, database, comment) {
		if len(held) != 1 || held[0].ToKind != refs.BlockNodeKind || held[0].ToID != block || !held[0].At.Equal(at) {
			t.Errorf("%s states %s %+v, want one to %s:%s dated at %s", what, kind, held, refs.BlockNodeKind, block, at)
		}
	}
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

// requireAnchorDifferences holds the census to reading exactly the differences of the anchor kind
// at names, in their order, each with cause, at the rows or the tuples, and none of any other kind.
func requireAnchorDifferences(t *testing.T, what string, report *StoreReport, node bool, cause string, at ...string) {
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
		matches := count == int64(len(at)) && len(found) == len(at) && other == 0
		for index := 0; matches && index < len(at); index++ {
			matches = found[index].Cause == cause && found[index].At == at[index]
		}
		if !matches {
			t.Errorf("%s: the census reads %d row and %d node difference(s) of kind %s (%+v %+v), want %q at each of %q",
				what, kind.RowDifferences, kind.NodeDifferences, kind.Kind, kind.ReportedRows, kind.ReportedNodes, cause, at)
		}
	}
	if !report.Refused() {
		t.Errorf("%s: the census does not refuse a store whose anchor blocks disagree with their rows", what)
	}
}

// The census holds comments.anchor's block_id to the anchored_in each comment's node states now,
// and, for a comment that answers nothing, to its part_of beside it, in both directions (T17b of
// the issue-tree plan, its spec R31: "The census kind `comment_anchor_block` holds, for
// each comment that answers nothing, the triple of its `part_of` beside the triple of its
// `anchored_in`, in both directions"), and the backfill gives each stored anchor with a block both
// tuples. Over a store the census agrees with (seedAgreeingStore), a comment a build before the
// store existed wrote, anchored on the document's inner block and answering nothing, is given its
// node by the comment pass and its anchored_in and its part_of to the inner block's node by the
// anchor pass, each dated at the comment's own created_at, and the census then agrees, holding two
// rows and two tuples of the kind. With the anchored_in retracted the census reads the row no tuple
// states, and the backfill states it again alone; with the row's anchor orphaned behind the store's
// back the census reads the two tuples no row projects, and the backfill retracts both, after
// which the kind holds no row and no tuple and the census agrees, and a second backfill levels
// nothing.
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
	if done.Rows != 1 || done.Tuples != 2 || !done.Complete {
		t.Errorf("the anchor pass reports %#v, want one comment, its anchored_in and its part_of, and none left", done)
	}
	requireAnchorPairAt(t, "the backfilled comment", database, comment, seeded.inner, createdAt)
	report := storeCensusOf(t, database)
	requireTheCensusAgrees(t, "the census after the anchor pass", report)
	if kind := censusKind(t, report, anchorProjectionKind); kind.Rows != 2 || kind.Nodes != 2 {
		t.Errorf("the anchor kind reads %d rows and %d tuples, want the anchored comment's anchored_in and part_of", kind.Rows, kind.Nodes)
	}

	appendTuple(t, database, graphstore.Tuple{
		FromKind: graphstore.KindComment, FromID: comment, Kind: graphstore.TupleAnchoredIn,
		ToKind: refs.BlockNodeKind, ToID: seeded.inner, Asserted: false,
	})
	requireAnchorDifferences(t, "a retracted anchored_in", storeCensusOf(t, database), false, "a row no tuple states",
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
	requireAnchorDifferences(t, "an orphaned anchor whose anchored_in and part_of stand", storeCensusOf(t, database), true, "a tuple no row projects",
		graphstore.KindComment+":"+comment+" "+graphstore.TupleAnchoredIn+" -> "+refs.BlockNodeKind+":"+seeded.inner,
		graphstore.KindComment+":"+comment+" "+graphstore.TuplePartOf+" -> "+refs.BlockNodeKind+":"+seeded.inner)
	if done := backfillAnchors(t, database); done.Rows != 1 || done.Tuples != 2 || !done.Complete {
		t.Errorf("the anchor pass over an orphaned anchor reports %#v, want its anchored_in and its part_of retracted", done)
	}
	for kind, held := range currentAnchorPair(t, database, comment) {
		if len(held) != 0 {
			t.Errorf("the orphaned anchor's comment states %s %+v, want none", kind, held)
		}
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

// TestTheAnchorBatchTakesTheStoreLockOrder holds writeAnchorBatch (T17b of the issue-tree
// plan) to the store's lock order, as TestEveryBackfillBatchTakesTheStoreLockOrder holds the other
// batches: the owner row the comment stands under, its issue's row, first, then the anchored_in and
// the part_of. The tuples are stated at the sentinel, which no repeat key holds, so no hold stands
// on them, and the batch takes no project row. The comment is one this build wrote with its node,
// anchored on a block of its issue's document, whose node states no anchored_in, as a process of the
// release before leaves the row it anchors. It answers nothing, so its spec's R31 owes it the
// part_of beside the anchored_in: "Each writer of its `anchored_in` states or retracts that
// `part_of` in the same write: ... and `backfillAnchoredIn` in `graphcensus/census_anchors.go`,
// which also states it for each comment anchored before this child, dated at the comment's
// `created_at`." After the run the comment states one anchored_in and one part_of to the block's
// node, each dated at the comment's created_at, and the backfill reports the comment levelled with
// those two tuples.
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
	requireAnchorPairAt(t, "the comment, after a run the test followed that committed,", database, comment, refKey+"#b1", createdAt)
	if done := reportedKind(t, report, anchorProjectionKind); done.Rows != 1 || done.Tuples != 2 || !done.Complete {
		t.Errorf("the backfill reports %#v for anchor blocks, want 1 comment, its anchored_in and its part_of, and none left", done)
	}
}
