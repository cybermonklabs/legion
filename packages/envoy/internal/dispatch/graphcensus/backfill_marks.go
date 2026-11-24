package graphcensus

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// markStaleDocuments marks, a batch at a time, every document another release indexed after this
// release's store last stated its blocks and that holds no mark of the marking rule rule or a
// higher one yet, and answers the documents it marked (StaleDocumentsStatement, markStaleBatch).
// The release before this one, indexing a version of a document the store has stated, deletes
// the rows of the blocks and the links that version drops and states no node gone and retracts
// no tuple; it leaves store_version, which only this release's index pass writes, below
// the version it wrote (migration 20261116221539). That is the whole of what this reads: the two
// versions the document's row records, and no row of either side of a projection, so no document
// is chosen by reading its blocks against its tuples. The reconciliation's index pass over the
// marked document's latest version then states each such block gone and retracts each such link,
// as its render decides (refs.IndexBlocks).
//
// A document the store has never stated (store_version null) is no document of this pass: the
// store holds nothing of it for a deletion to strand, and its rows name no node, which the block
// pass reaches. A document whose version is null is owed an index already, and the pass that
// indexes it finds the store stale by the same two versions. A second run finds nothing a first
// one marked, and nothing once the reconciliation has indexed the documents it marked, since that
// pass sets store_version to the version it indexed. Each batch is p's, and p paces the pass
// (pacer), whose kind is stale_document.
func markStaleDocuments(ctx context.Context, database *store.Store, rule int, p *pacer) ([]string, error) {
	var marked []string
	after := "00000000-0000-0000-0000-000000000000"
	p.begin("stale_document")
	for {
		rows, err := database.Pool.Query(ctx, StaleDocumentsStatement, after, p.throttle.Batch, rule)
		if err != nil {
			return marked, fmt.Errorf("read the documents another release indexed after the store: %w", err)
		}
		batch, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			return marked, fmt.Errorf("read the documents another release indexed after the store: %w", err)
		}
		if len(batch) == 0 {
			p.end()
			return marked, nil
		}
		after = batch[len(batch)-1]
		written, err := markStaleBatch(ctx, database, batch, rule)
		if err != nil {
			return marked, err
		}
		marked = append(marked, written...)
		if err := p.after(ctx, len(batch)); err != nil {
			return marked, err
		}
		if len(batch) < p.throttle.Batch {
			p.end()
			return marked, nil
		}
	}
}

// StaleDocumentsStatement is markStaleDocuments' selection: at most $2 documents past $1, in id
// order, that another release indexed after this release's store last stated them and that hold
// no mark of the marking rule $3 or a higher one. A mark of a lower rule is read as no mark
// (refs.OwedMarkAbsent), so markStaleBatch raises it to $3.
var StaleDocumentsStatement = `
	select artifact_id::text from document_block_versions
	where store_version < version and ` + refs.OwedMarkAbsent("", "$3::integer") + `
	  and artifact_id > $1::uuid
	order by artifact_id
	limit $2
`

// markStaleBatch marks the documents of one batch in one transaction, under the locks a version
// write holds while it writes a document's blocks (lockBlockDocuments), and only those whose two
// versions still say so under them (StaleBatchMarkStatement): an index pass of this release that
// reached a document in between has stated its blocks at the version it indexed, and owes it
// nothing. It answers the documents it marked.
func markStaleBatch(ctx context.Context, database *store.Store, batch []string, rule int) ([]string, error) {
	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin the batch of documents another release indexed: %w", err)
	}
	defer tx.Rollback(ctx)
	if err := lockBlockDocuments(ctx, tx, batch); err != nil {
		return nil, err
	}
	rows, err := tx.Query(ctx, StaleBatchMarkStatement, batch, rule)
	if err != nil {
		return nil, fmt.Errorf("mark the documents another release indexed after the store: %w", err)
	}
	marked, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, fmt.Errorf("mark the documents another release indexed after the store: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit the batch of documents another release indexed: %w", err)
	}
	return marked, nil
}

// StaleBatchMarkStatement is markStaleBatch's mark, at the marking rule $2, of each document of
// $1 whose store still stands below its version and that holds no mark of $2 or a higher rule:
// a mark of a lower rule is read as no mark (refs.OwedMarkAbsent) and raised to $2, keeping its
// time, and a document that holds no mark takes $2 and now. It answers the documents it marked.
var StaleBatchMarkStatement = `
	update document_block_versions set store_owed_at = coalesce(store_owed_at, now()), store_owed_rule = $2::integer
	where artifact_id = any($1::uuid[]) and store_version < version
	  and ` + refs.OwedMarkAbsent("", "$2::integer") + `
	returning artifact_id::text
`
