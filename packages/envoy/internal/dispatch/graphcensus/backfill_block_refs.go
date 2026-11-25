package graphcensus

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// blockRefRow is one block_refs row the backfill reads: the pair it holds, the block node it
// is a link out of, the row's own dates and sequence, and whether the store holds that block's
// node in a state other than gone (blockNodeStands).
type blockRefRow struct {
	artifactID, blockID string
	address             string
	kind, toKind, toID  string
	createdAt           time.Time
	sourceSeq           *int64
	event               int64
	blockNodeStands     bool
}

// backfillBlockRefs states one tuple per block reference written before the store existed. A
// block_refs row is one link out of a block and its node is that block's, so this writes no
// node of its own: it runs after the blocks, and a row whose block has no node in the store, or
// one the store holds gone, is skipped and counted. A row whose far end is past the address bound
// is no tuple the store can key beside the block's own address, as the live writer leaves it out
// too (refs' planBlockRefs), so this reads none of those and the row keeps its null node
// columns for the old arm. A row a version write deleted or wrote after its batch was read is not
// written (writeBlockRefBatch), and is counted nowhere.
//
// The guard reads the store and no document_blocks row (blockNodeStands), because the backfill
// reads no row its marker marks, and the block's row is marked once the block pass has run: the
// pass walks only the rows still owed, so it can run again at any time over what a rollback left
// unmarked. The node is what the pointer row's foreign key needs, and its state is graph_edges'
// own test of the tuple arm (migration 20261109215947): after this release drops a block, a build
// of the release before it can restore the block with no node on its rows, and a pointer stamped
// against the gone node would answer from neither arm, since the old arm skips a marked row and
// the tuple arm skips a gone node.
//
// The row's own source_seq is the event that stamped it, which is both the cause of its tuple
// and that tuple's legacy_source_seq; a row with none takes the store's sentinel as its cause
// and keeps no legacy sequence, exactly as the graph_edges arm over block_refs answers null
// for it. Each batch is p's, and p paces the pass (pacer).
func backfillBlockRefs(ctx context.Context, database *store.Store, p *pacer) (KindBackfill, error) {
	done := KindBackfill{Kind: "block_ref"}
	after := []any{"", "", "", ""}
	p.begin(done.Kind)
	for {
		batch, err := readBlockRefRows(ctx, database, after, p.throttle.Batch)
		if err != nil {
			return done, err
		}
		if len(batch) == 0 {
			break
		}
		last := batch[len(batch)-1]
		after = []any{last.address, last.kind, last.toKind, last.toID}
		writable := make([]blockRefRow, 0, len(batch))
		for _, row := range batch {
			if !row.blockNodeStands {
				done.Skipped++
				continue
			}
			writable = append(writable, row)
		}
		if len(writable) > 0 {
			written, err := writeBlockRefBatch(ctx, database, writable)
			if err != nil {
				return done, err
			}
			done.Rows += written
			done.Tuples += written
		}
		if err := p.after(ctx, len(batch)); err != nil {
			return done, err
		}
		if len(batch) < p.throttle.Batch {
			break
		}
	}
	p.end()
	if err := database.Pool.QueryRow(ctx, `
		select count(*) from block_refs
		where node_kind is null and octet_length(from_node) <= `+addressBound+`
		  and octet_length(to_id) <= `+addressBound).Scan(&done.Remaining); err != nil {
		return done, fmt.Errorf("count the block references still naming no node: %w", err)
	}
	done.Complete = done.Remaining == 0
	return done, nil
}

// readBlockRefRows reads one batch of at most limit of the block references naming no node, by the
// key a row of block_refs is keyed on past its artifact, so the walk never reads one row twice.
// Each row carries whether its block's node stands (blockNodeStands), which is the guard
// backfillBlockRefs skips a row by.
func readBlockRefRows(ctx context.Context, database *store.Store, after []any, limit int) ([]blockRefRow, error) {
	rows, err := database.Pool.Query(ctx, `
		select r.artifact_id::text, r.block_id, r.from_node, r.kind, r.to_kind, r.to_id,
		       r.created_at, r.source_seq, coalesce(r.source_seq, $6),
		       `+blockNodeStands(`r`)+`
		from block_refs r
		where r.node_kind is null and octet_length(r.from_node) <= `+addressBound+`
		  and octet_length(r.to_id) <= `+addressBound+`
		  and (r.from_node, r.kind, r.to_kind, r.to_id) > ($1, $2, $3, $4)
		order by r.from_node, r.kind, r.to_kind, r.to_id
		limit $5
	`, after[0], after[1], after[2], after[3], limit, graphstore.NoEvent)
	if err != nil {
		return nil, fmt.Errorf("read the block references naming no node: %w", err)
	}
	defer rows.Close()
	var batch []blockRefRow
	for rows.Next() {
		var row blockRefRow
		if err := rows.Scan(&row.artifactID, &row.blockID, &row.address, &row.kind, &row.toKind,
			&row.toID, &row.createdAt, &row.sourceSeq, &row.event, &row.blockNodeStands); err != nil {
			return nil, fmt.Errorf("read a block reference naming no node: %w", err)
		}
		batch = append(batch, row)
	}
	return batch, rows.Err()
}

// blockNodeStands is the pointer pass's guard over the block_refs row at alias: the store holds
// the node of the block the row is a link out of, in a state other than gone. It is the test
// graph_edges' tuple arm makes of that node, read off graph_nodes alone, so the pass stamps a
// pointer only where the tuple arm will answer it (backfillBlockRefs).
func blockNodeStands(alias string) string {
	return `exists (select 1 from graph_nodes block
		where block.kind = '` + refs.BlockNodeKind + `' and block.id = ` + alias + `.from_node
			and block.state <> '` + graphstore.StateGone + `')`
}

// writeBlockRefBatch appends the tuples of the rows of one batch that are still there and still
// name no node, and names the block's node on each of them, in one transaction, and answers how
// many rows it wrote, each of which is one tuple. The nodes are already there - the blocks ran
// first - so this writes none, and the call still goes through the store's own writer, which is
// what holds the two sides of a projection in one transaction.
//
// The batch was read outside this transaction, so a version write may have deleted one of its
// references since, and a version write that deletes a reference states its tuple retracted
// (refs' planBlockRefs). A tuple asserted for that row would be appended after the retraction
// and so be the newest word on the link: the store would answer a mention no row holds. The
// batch is therefore held to the revision fence as writeBlockBatch is: before it writes anything
// it takes the locks a version write holds while it writes a document's references
// (lockBlockDocuments), reads the batch's rows again under them (heldBlockRefRows), and writes
// only the rows left.
func writeBlockRefBatch(ctx context.Context, database *store.Store, batch []blockRefRow) (int64, error) {
	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("begin the block reference batch: %w", err)
	}
	defer tx.Rollback(ctx)
	documents := make([]string, 0, len(batch))
	for _, row := range batch {
		documents = append(documents, row.artifactID)
	}
	if err := lockBlockDocuments(ctx, tx, documents); err != nil {
		return 0, err
	}
	held, err := heldBlockRefRows(ctx, tx, batch)
	if err != nil {
		return 0, err
	}
	if len(held) == 0 {
		return 0, nil
	}
	tuples := make([]graphstore.Tuple, 0, len(held))
	artifacts := make([]string, 0, len(held))
	blockIDs := make([]string, 0, len(held))
	kinds := make([]string, 0, len(held))
	toKinds := make([]string, 0, len(held))
	toIDs := make([]string, 0, len(held))
	addresses := make([]string, 0, len(held))
	for _, row := range held {
		tuples = append(tuples, graphstore.Tuple{
			FromKind: refs.BlockNodeKind, FromID: row.address, Kind: row.kind,
			ToKind: row.toKind, ToID: row.toID,
			At: row.createdAt, Who: backfillActor, SourceEvent: row.event,
			LegacySourceSeq: row.sourceSeq, Asserted: true,
		})
		artifacts = append(artifacts, row.artifactID)
		blockIDs = append(blockIDs, row.blockID)
		kinds = append(kinds, row.kind)
		toKinds = append(toKinds, row.toKind)
		toIDs = append(toIDs, row.toID)
		addresses = append(addresses, row.address)
	}
	if err := graphstore.Write(ctx, tx, nil, tuples); err != nil {
		return 0, fmt.Errorf("append the tuples of a block reference batch: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		update block_refs r
		set node_kind = $7, node_address = p.address
		from unnest($1::text[], $2::text[], $3::text[], $4::text[], $5::text[], $6::text[])
			as p (artifact_id, block_id, kind, to_kind, to_id, address)
		where r.artifact_id = p.artifact_id::uuid and r.block_id = p.block_id
		  and r.kind = p.kind and r.to_kind = p.to_kind and r.to_id = p.to_id
	`, artifacts, blockIDs, kinds, toKinds, toIDs, addresses, refs.BlockNodeKind); err != nil {
		return 0, fmt.Errorf("name the nodes of a block reference batch on their rows: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("commit a block reference batch: %w", err)
	}
	return int64(len(held)), nil
}

// heldBlockRefRows reads again, under the locks lockBlockDocuments took, which rows of the batch
// are still there, still name no node and are still links out of a block whose node stands
// (blockNodeStands), locks each of them `for update` in key order, and answers them in the
// batch's own order. The rest of a row is what the batch read: a version write keeps a surviving
// row's dates and source_seq, and a row it inserts names its node (refs' queueBlockRefs), so a
// row that still names no node is one no version has written since.
func heldBlockRefRows(ctx context.Context, tx pgx.Tx, batch []blockRefRow) ([]blockRefRow, error) {
	artifacts := make([]string, 0, len(batch))
	blockIDs := make([]string, 0, len(batch))
	kinds := make([]string, 0, len(batch))
	toKinds := make([]string, 0, len(batch))
	toIDs := make([]string, 0, len(batch))
	for _, row := range batch {
		artifacts = append(artifacts, row.artifactID)
		blockIDs = append(blockIDs, row.blockID)
		kinds = append(kinds, row.kind)
		toKinds = append(toKinds, row.toKind)
		toIDs = append(toIDs, row.toID)
	}
	rows, err := tx.Query(ctx, `
		select r.artifact_id::text, r.block_id, r.kind, r.to_kind, r.to_id
		from block_refs r
		join unnest($1::text[], $2::text[], $3::text[], $4::text[], $5::text[])
			as p (artifact_id, block_id, kind, to_kind, to_id)
			on r.artifact_id = p.artifact_id::uuid and r.block_id = p.block_id
			and r.kind = p.kind and r.to_kind = p.to_kind and r.to_id = p.to_id
		where r.node_kind is null and `+blockNodeStands(`r`)+`
		order by r.artifact_id, r.block_id, r.to_kind, r.to_id, r.kind
		for update of r
	`, artifacts, blockIDs, kinds, toKinds, toIDs)
	if err != nil {
		return nil, fmt.Errorf("read the block references of a batch again under their documents' locks: %w", err)
	}
	keys, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) ([5]string, error) {
		var key [5]string
		err := row.Scan(&key[0], &key[1], &key[2], &key[3], &key[4])
		return key, err
	})
	if err != nil {
		return nil, fmt.Errorf("scan the block references of a batch read again: %w", err)
	}
	still := make(map[[5]string]struct{}, len(keys))
	for _, key := range keys {
		still[key] = struct{}{}
	}
	held := make([]blockRefRow, 0, len(keys))
	for _, row := range batch {
		if _, ok := still[[5]string{row.artifactID, row.blockID, row.kind, row.toKind, row.toID}]; ok {
			held = append(held, row)
		}
	}
	return held, nil
}
