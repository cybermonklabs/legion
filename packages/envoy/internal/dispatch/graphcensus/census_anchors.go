package graphcensus

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// anchorProjectionKind is the census's and the backfill's name of a comment's anchor block.
const anchorProjectionKind = "comment_anchor_block"

// anchorProjections are the census's kind of a comment's anchor (projections; T17b of
// the issue-tree plan, its spec R31), held triple by triple (projection.tuples): each
// comments row naming its node whose anchor names a node - a document anchor's block, or a node
// anchor's lens, finding, risk, constraint or result - and is not orphaned, as the triple of its
// node, anchored_in and that node (graphstore.CommentAnchoredInSQL) and, for a comment that answers
// nothing, the triple of its node, part_of and that same node beside it, against every anchored_in
// a present comment node states now and every part_of a present comment node that answers nothing
// states now, in both directions. A comment whose row names no node is a row a process of the
// release before wrote, which the comment backfill gives its node first and which the census counts
// as a class of its own; a node whose address is past the store's bound is no node, counted as a
// class of its own too. A deployment whose comments name no anchor holds no row, no tuple and no
// unmarked row of the kind, which is no refusal (projection.emptyAgrees).
var anchorProjections = []projection{{
	name:     anchorProjectionKind,
	nodeKind: graphstore.KindComment,
	table:    "comments.anchor, comments.node_anchor",
	note: "a comment's anchor: the row side is every marked comments row whose anchor names a block or a node and is not" +
		" orphaned, as the triple of its node, anchored_in and that node (a block under the room its document is edited" +
		" in) and, for a comment that answers nothing, the triple of its node, part_of and that node, against every" +
		" anchored_in a present comment node states now and every part_of a present comment node that answers nothing" +
		" states now",
	rows: `
		select 'comment ' || c.id::text, c.node_kind, c.node_address, relation.kind, wanted.kind, wanted.node
		from comments c` + wantedAnchor + wantedRelations + `
		where c.node_kind is not null and octet_length(wanted.node) <= ` + addressBound,
	tuples: heldTuples(`t.kind in ('`+graphstore.TupleAnchoredIn+`', '`+graphstore.TuplePartOf+`')
		  and (t.kind = '`+graphstore.TupleAnchoredIn+`' or `+nodeAnswersNothing("n")+`)`, graphstore.KindComment),
	pastBound: `
		select count(*) from comments c` + wantedAnchor + `
		where c.node_kind is not null and octet_length(wanted.node) > ` + addressBound,
	unmarked: `
		select count(*) from comments c` + wantedAnchor + `
		where c.node_kind is null and wanted.node is not null`,
	emptyAgrees: true,
}}

// wantedAnchor joins, to the comments row aliased c, what the row states of its anchor:
// wanted.kind and wanted.node, the far end of its anchored_in (graphstore.CommentAnchoredInKindSQL,
// graphstore.CommentAnchoredInSQL), the node null where it states none; and wanted.placed, whether
// the row answers nothing, so its anchor states a part_of to that same node
// (graphstore.CommentAnswersNothingSQL, graphstore.AnchorTuples).
var wantedAnchor = `
		cross join lateral (select ` + graphstore.CommentAnchoredInKindSQL("c") + ` as kind, ` +
	graphstore.CommentAnchoredInSQL("c") + ` as node, ` + graphstore.CommentAnswersNothingSQL("c") + ` as placed) wanted`

// wantedRelations joins, to the comments row aliased c joined to wanted (wantedAnchor),
// relation.kind: each relation its anchor states, its anchored_in and, where the row answers
// nothing, its part_of.
var wantedRelations = `
		cross join lateral (select '` + graphstore.TupleAnchoredIn + `' as kind
			union all select '` + graphstore.TuplePartOf + `' where wanted.placed) relation`

// nodeAnswersNothing is whether the comment node whose graph_nodes row is aliased alias answers
// nothing, as SQL text answering a boolean: no comments row naming that node answers something
// (graphstore.CommentAnswersNothingSQL), so a part_of out of it is its anchor's.
func nodeAnswersNothing(alias string) string {
	return `not exists (select 1 from comments r
			where r.node_kind = ` + alias + `.kind and r.node_address = ` + alias + `.id and not ` + graphstore.CommentAnswersNothingSQL("r") + `)`
}

// heldAnchor is every relation of the anchor's pair the node of the comments row aliased c, joined
// to wanted (wantedAnchor), states: each anchored_in, and each part_of where the row answers
// nothing, the newest tuple of each far end with whether it asserts (graphstore.CurrentTupleRead),
// read through graph_tuples_current.
var heldAnchor = graphstore.CurrentTupleRead(`t.kind, t.to_kind, t.to_id`, `t.kind, t.to_kind, t.to_id, t.asserted`,
	`t.from_kind = c.node_kind and t.from_id = c.node_address and t.to_kind <> '`+graphstore.KindValue+`'
		and t.kind in ('`+graphstore.TupleAnchoredIn+`', '`+graphstore.TuplePartOf+`')
		and (t.kind = '`+graphstore.TupleAnchoredIn+`' or wanted.placed)`)

// anchorMoves is, for the comments row aliased c joined to wanted (wantedAnchor), what levels its
// node's anchored_in and, for a comment that answers nothing, its part_of with the row, as
// (relation, to_kind, node, asserted): each relation the row states to its anchor's node where its
// node states none to it now, and a retraction of each relation of the pair its node states now to
// any other node. A node past the store's bound is no node, so it is stated nowhere.
var anchorMoves = `
		select relation.kind as relation, wanted.kind as to_kind, wanted.node as node, true as asserted
		from (select '` + graphstore.TupleAnchoredIn + `' as kind union all select '` + graphstore.TuplePartOf + `' where wanted.placed) relation
		where octet_length(wanted.node) <= ` + addressBound + `
		  and not exists (select 1 from (` + heldAnchor + `) held
			where held.asserted and held.kind = relation.kind and held.to_kind = wanted.kind and held.to_id = wanted.node)
		union all
		select held.kind, held.to_kind, held.to_id, false from (` + heldAnchor + `) held
		where held.asserted and (held.to_kind, held.to_id) is distinct from (wanted.kind, wanted.node)`

// owedAnchoredIn is the statement shape of the comments whose node does not state the pair their
// row states: rows naming their node, joined to wanted, that owe a move (anchorMoves). The pass
// and its count read the same predicate, so the count says what the next run would write.
var owedAnchoredIn = `
		from comments c` + wantedAnchor + `
		where c.node_kind is not null and exists (` + anchorMoves + `)`

// anchorBackfills are the backfill's passes over the comments' anchors (backfillPasses), after the
// comment pass, which gives every comment written before the store existed its node: the
// anchored_in and the part_of each stored anchor owes (backfillAnchoredIn).
var anchorBackfills = []backfillPass{oneKind(backfillAnchoredIn)}

// anchorRow is one comment whose node owes a move of its anchor's pair, with the owner its row
// stands under: its issue, or the project document it belongs to.
type anchorRow struct {
	id, issue, artifact string
}

// backfillAnchoredIn gives each stored anchor that names a node its anchored_in and, for a comment
// that answers nothing, its part_of to the same node (T17b of the issue-tree plan, its
// spec R31): each relation of the pair a comment's row states that its node does not state now,
// dated at the comment's own created_at - so a comment anchored before this release takes both at
// the time it was written - and a retraction, dated at the run's time, of each relation of the
// pair its node states to a node its row no longer names: an anchor a process of the release
// before orphaned, or whose block it pinned, after this build stated the tuples. The comments row
// takes no marker column, so the tuples themselves are the marker: the pass finds a comment owed by
// comparing its row with its node's current pair, walking the rows batch by batch in their primary
// key's order, and a second run reads no comment a first one levelled. It appends no event: every
// fact it states carries the sentinel and is written by the run (backfillActor).
func backfillAnchoredIn(ctx context.Context, database *store.Store) (KindBackfill, error) {
	done := KindBackfill{Kind: anchorProjectionKind}
	if err := walkBatches(ctx, database, owedAnchorRowsStatement, "the comments whose anchor block their node does not state", zeroUUID,
		func(row pgx.CollectableRow) (anchorRow, error) {
			var comment anchorRow
			err := row.Scan(&comment.id, &comment.issue, &comment.artifact)
			return comment, err
		}, func(comment anchorRow) string { return comment.id }, func(batch []anchorRow) error {
			levelled, tuples, err := writeAnchorBatch(ctx, database, batch)
			if err != nil {
				return err
			}
			done.Rows += levelled
			done.Tuples += tuples
			return nil
		}); err != nil {
		return done, err
	}
	if err := database.Pool.QueryRow(ctx, `select count(*) `+owedAnchoredIn).Scan(&done.Remaining); err != nil {
		return done, fmt.Errorf("count the comments whose anchor block their node does not state: %w", err)
	}
	done.Complete = done.Remaining == 0
	return done, nil
}

// owedAnchorRowsStatement is backfillAnchoredIn's selection: at most $2 of the comments whose node
// does not state the anchored_in and part_of their row states (owedAnchoredIn), past the comment
// $1, in id order, each with its issue or its project document. Each batch reads comments_pkey
// from the id on, so a run reads each comment once however many batches it takes
// (TestEveryTopicPassWalksOneBatchThroughAnIndex).
var owedAnchorRowsStatement = `
	select c.id::text, coalesce(c.issue_key, ''), coalesce(c.artifact_id::text, '') ` + owedAnchoredIn + `
	  and c.id > $1::uuid
	order by c.id
	limit $2`

// writeAnchorBatch levels one batch's anchored_in and part_of in one transaction, in the store's
// lock order (api/inbox_read.go's markInboxReadThreads): the owner rows the batch's comments stand
// under first, each once, in ascending key (events.LockOwnerRows), then the batch's moves read
// again under them, then the tuples (graphstore.QueueWrite, which writes no node: each comment's
// node is the comment pass's). Every writer that moves an anchor's block, its node or its orphan
// flag on this build holds the comment's owner (the refresh, an accept or a reject, the node-anchor
// refresh and the node DELETE, under the issue's owner lock) or writes its tuples in the
// transaction that pins the block (docs' BackfillAnchorBlocks), so the moves read under the owner
// rows are the rows' as they commit. It answers how many comments it levelled and how many tuples
// it stated.
func writeAnchorBatch(ctx context.Context, database *store.Store, batch []anchorRow) (int64, int64, error) {
	var levelled, stated int64
	if err := pgx.BeginFunc(ctx, database.Pool, func(tx pgx.Tx) error {
		owners := events.OwnerRows{}
		ids := make([]string, len(batch))
		for index, comment := range batch {
			owners.Add(comment.issue, comment.artifact)
			ids[index] = comment.id
		}
		if err := events.LockOwnerRows(ctx, tx, owners); err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `
			select c.id::text, c.created_at, move.relation, move.to_kind, move.node, move.asserted
			from comments c`+wantedAnchor+`
			cross join lateral (`+anchorMoves+`) move
			where c.id = any($1::uuid[]) and c.node_kind is not null
			order by c.id, move.asserted, move.relation, move.to_kind, move.node
		`, ids)
		if err != nil {
			return fmt.Errorf("read a batch's anchor blocks again under their owners: %w", err)
		}
		now := time.Now().UTC()
		var tuples []graphstore.Tuple
		comments := map[string]struct{}{}
		for rows.Next() {
			var id, relation, kind, node string
			var createdAt time.Time
			var asserted bool
			if err := rows.Scan(&id, &createdAt, &relation, &kind, &node, &asserted); err != nil {
				rows.Close()
				return fmt.Errorf("read a batch's anchor blocks again under their owners: %w", err)
			}
			at := now
			if asserted {
				at = createdAt
			}
			tuples = append(tuples, graphstore.AnchorRelation(relation, id, kind, node, asserted, backfillActor, at, graphstore.NoEvent))
			comments[id] = struct{}{}
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return fmt.Errorf("read a batch's anchor blocks again under their owners: %w", err)
		}
		if len(tuples) == 0 {
			return nil
		}
		if err := store.RunWrite(ctx, tx, func(b *store.WriteBatch) error {
			graphstore.QueueWrite(b, nil, tuples)
			graphstore.QueueHeads(b, graphstore.HeadsOf(nil, tuples))
			return nil
		}); err != nil {
			return err
		}
		levelled, stated = int64(len(comments)), int64(len(tuples))
		return nil
	}); err != nil {
		return 0, 0, fmt.Errorf("write a batch of anchor blocks: %w", err)
	}
	return levelled, stated, nil
}
