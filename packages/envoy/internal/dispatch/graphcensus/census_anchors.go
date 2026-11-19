package graphcensus

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// anchorProjectionKind is the census's and the backfill's name of a comment's anchor block.
const anchorProjectionKind = "comment_anchor_block"

// anchorProjections are the census's kind of a comment's anchor block (projections; T17b of
// dispatch://INFRA-1253's plan), held triple by triple (projection.tuples): each comments row
// naming its node whose anchor names a block and is not orphaned, as the triple of its node,
// anchored_in and the block's node (graphstore.CommentAnchoredInSQL), against every anchored_in a
// present comment node states now, in both directions. A comment whose row names no node is a row
// a process of the release before wrote, which the comment backfill gives its node first and which
// the census counts as a class of its own; a block whose address is past the store's bound is no
// node, counted as a class of its own too. A deployment whose comments name no anchor block holds
// no row, no tuple and no unmarked row of the kind, which is no refusal (projection.emptyAgrees).
var anchorProjections = []projection{{
	name:     anchorProjectionKind,
	nodeKind: graphstore.KindComment,
	table:    "comments.anchor",
	note: "a comment's anchor block: the row side is every marked comments row whose anchor names a block and is not" +
		" orphaned, as the triple of its node, anchored_in and the block's node under the room its document is edited" +
		" in, against every anchored_in a present comment node states now",
	rows: `
		select 'comment ' || c.id::text, c.node_kind, c.node_address,
		       '` + graphstore.TupleAnchoredIn + `', '` + refs.BlockNodeKind + `', wanted.block
		from comments c` + wantedAnchorBlock + `
		where c.node_kind is not null and octet_length(wanted.block) <= ` + addressBound,
	tuples: heldTuples(`t.kind = '`+graphstore.TupleAnchoredIn+`'`, graphstore.KindComment),
	pastBound: `
		select count(*) from comments c` + wantedAnchorBlock + `
		where c.node_kind is not null and octet_length(wanted.block) > ` + addressBound,
	unmarked: `
		select count(*) from comments c` + wantedAnchorBlock + `
		where c.node_kind is null and wanted.block is not null`,
	emptyAgrees: true,
}}

// wantedAnchorBlock joins, to the comments row aliased c, wanted.block: the far end of the
// anchored_in the row states (graphstore.CommentAnchoredInSQL), null where it states none.
var wantedAnchorBlock = `
		cross join lateral (select ` + graphstore.CommentAnchoredInSQL("c") + ` as block) wanted`

// heldAnchoredIn is every anchored_in the node of the comments row aliased c states, the newest
// tuple of each far end with whether it asserts (graphstore.CurrentTupleRead), read through
// graph_tuples_current.
var heldAnchoredIn = graphstore.CurrentTupleRead(`t.to_kind, t.to_id`, `t.to_id, t.asserted`,
	`t.from_kind = c.node_kind and t.from_id = c.node_address and t.kind = '`+graphstore.TupleAnchoredIn+`'`)

// anchoredInMoves is, for the comments row aliased c joined to wanted (wantedAnchorBlock), what
// levels its node's anchored_in with the row, as (block, asserted): the block the row states where
// its node states no anchored_in to it now, and a retraction of each anchored_in its node states
// now to any other block. A block past the store's bound is no node, so it is stated nowhere.
var anchoredInMoves = `
		select wanted.block as block, true as asserted
		where octet_length(wanted.block) <= ` + addressBound + `
		  and not exists (select 1 from (` + heldAnchoredIn + `) held where held.asserted and held.to_id = wanted.block)
		union all
		select held.to_id, false from (` + heldAnchoredIn + `) held
		where held.asserted and held.to_id is distinct from wanted.block`

// owedAnchoredIn is the statement shape of the comments whose node does not state the anchored_in
// their row states: rows naming their node, joined to wanted, that owe a move (anchoredInMoves).
// The pass and its count read the same predicate, so the count says what the next run would write.
var owedAnchoredIn = `
		from comments c` + wantedAnchorBlock + `
		where c.node_kind is not null and exists (` + anchoredInMoves + `)`

// anchorBackfills are the backfill's passes over the comments' anchor blocks (backfillPasses),
// after the comment pass, which gives every comment written before the store existed its node:
// the anchored_in each stored anchor with a block owes (backfillAnchoredIn).
var anchorBackfills = []backfillPass{oneKind(backfillAnchoredIn)}

// anchorRow is one comment whose node owes a move of its anchored_in, with the owner its row
// stands under: its issue, or the project document it belongs to.
type anchorRow struct {
	id, issue, artifact string
}

// backfillAnchoredIn gives each stored anchor with a block its anchored_in (T17b of
// dispatch://INFRA-1253's plan): the anchored_in a comment's row states that its node does not
// state now, dated at the comment's own created_at, and a retraction, dated at the run's time, of
// each anchored_in its node states to a block its row no longer names - an anchor a process of the
// release before orphaned, or whose block it pinned, after this build stated the tuple. The
// comments row takes no marker column, so the tuple itself is the marker: the pass finds a comment
// owed by comparing its row with its node's current anchored_in, walking the rows batch by batch
// in their primary key's order, and a second run reads no comment a first one levelled. It appends
// no event: every fact it states carries the sentinel and is written by the run (backfillActor).
func backfillAnchoredIn(ctx context.Context, database *store.Store) (KindBackfill, error) {
	done := KindBackfill{Kind: anchorProjectionKind}
	after := zeroUUID
	for {
		rows, err := database.Pool.Query(ctx, `
			select c.id::text, coalesce(c.issue_key, ''), coalesce(c.artifact_id::text, '') `+owedAnchoredIn+`
			  and c.id > $1::uuid
			order by c.id
			limit $2
		`, after, backfillBatch)
		if err != nil {
			return done, fmt.Errorf("read the comments whose anchor block their node does not state: %w", err)
		}
		batch, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (anchorRow, error) {
			var comment anchorRow
			err := row.Scan(&comment.id, &comment.issue, &comment.artifact)
			return comment, err
		})
		if err != nil {
			return done, fmt.Errorf("read the comments whose anchor block their node does not state: %w", err)
		}
		if len(batch) == 0 {
			break
		}
		after = batch[len(batch)-1].id
		levelled, tuples, err := writeAnchorBatch(ctx, database, batch)
		if err != nil {
			return done, err
		}
		done.Rows += levelled
		done.Tuples += tuples
		if len(batch) < backfillBatch {
			break
		}
	}
	if err := database.Pool.QueryRow(ctx, `select count(*) `+owedAnchoredIn).Scan(&done.Remaining); err != nil {
		return done, fmt.Errorf("count the comments whose anchor block their node does not state: %w", err)
	}
	done.Complete = done.Remaining == 0
	return done, nil
}

// writeAnchorBatch levels one batch's anchored_in in one transaction, in the store's lock order
// (api/inbox_read.go's markInboxReadThreads): the owner rows the batch's comments stand under
// first, each once, in ascending key (events.LockOwnerRows), then the batch's moves read again under
// them, then the tuples (graphstore.Write, which writes no node: each comment's node is the comment
// pass's). Every writer that moves an anchor's block or its orphan flag on this build holds the
// comment's owner row (the refresh, an accept or a reject) or writes its tuple in the transaction
// that pins the block (docs' BackfillAnchorBlocks), so the moves read under the owner rows are the
// rows' as they commit. It answers how many comments it levelled and how many tuples it stated.
func writeAnchorBatch(ctx context.Context, database *store.Store, batch []anchorRow) (int64, int64, error) {
	var levelled, stated int64
	if err := pgx.BeginFunc(ctx, database.Pool, func(tx pgx.Tx) error {
		owners := events.OwnerRows{}
		ids := make([]string, len(batch))
		for index, comment := range batch {
			owners.Add(comment.issue, comment.artifact, "comment "+comment.id)
			ids[index] = comment.id
		}
		if err := events.LockOwnerRows(ctx, tx, owners); err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `
			select c.id::text, c.created_at, move.block, move.asserted
			from comments c`+wantedAnchorBlock+`
			cross join lateral (`+anchoredInMoves+`) move
			where c.id = any($1::uuid[]) and c.node_kind is not null
			order by c.id, move.asserted, move.block
		`, ids)
		if err != nil {
			return fmt.Errorf("read a batch's anchor blocks again under their owners: %w", err)
		}
		now := time.Now().UTC()
		var tuples []graphstore.Tuple
		comments := map[string]struct{}{}
		for rows.Next() {
			var id, block string
			var createdAt time.Time
			var asserted bool
			if err := rows.Scan(&id, &createdAt, &block, &asserted); err != nil {
				rows.Close()
				return fmt.Errorf("read a batch's anchor blocks again under their owners: %w", err)
			}
			at := now
			if asserted {
				at = createdAt
			}
			tuples = append(tuples, graphstore.AnchoredIn(id, block, asserted, backfillActor, at, graphstore.NoEvent))
			comments[id] = struct{}{}
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return fmt.Errorf("read a batch's anchor blocks again under their owners: %w", err)
		}
		if len(tuples) == 0 {
			return nil
		}
		if err := graphstore.Write(ctx, tx, nil, tuples); err != nil {
			return err
		}
		levelled, stated = int64(len(comments)), int64(len(tuples))
		return nil
	}); err != nil {
		return 0, 0, fmt.Errorf("write a batch of anchor blocks: %w", err)
	}
	return levelled, stated, nil
}
