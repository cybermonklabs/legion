package graphcensus

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// markStaleDocuments marks, a batch at a time, every document that holds no mark of the marking
// rule rule or a higher one yet and that no pass of that rule has stated at its version: its
// stated_version is not its version, or its stated_rule is below the rule, a null on either side
// counted as not equal (refs.UnstatedBlockIndexSQL). It answers the documents it marked
// (StaleDocumentsStatement, markStaleBatch). Only a pass of the rule writes those two columns, in
// the statement that writes store_version (refs' queueStand), or where it clears the marks of a
// document it covers no version of (docs' indexOwedDocument and indexOwedRoom). store_version
// alone no longer says what the store owes: a build of an earlier rule writes it for each version
// it indexes, and states none of this rule's facts of its blocks. So the test marks a document
// such a build indexed, one the release before this one indexed after this build stated it - which
// deletes the rows of the blocks and the links that version drops, states no node gone and
// retracts no tuple - one it created, and one stored before this build. That is the whole of what
// this reads: the record the document's row holds, and no row of either side of a projection, so
// no document is chosen by reading its blocks against its tuples. The reconciliation's index pass
// over the marked document's latest version then states what its blocks owe, and each block and
// link that version dropped gone or retracted, as its render decides (refs.IndexBlocks), and
// records the version it stated by its rule.
//
// It marks on every run, not once: a build of an earlier rule can create or index a document
// after any run, and the next run reads it. It reads document_block_versions on its primary key,
// in batches of backfillBatch in artifact_id order, each batch after the last id of the batch
// before, and marks each batch in its own transaction (markStaleBatch), so a run stopped part way
// leaves the rest to the next run, which tests every row again. A second run finds nothing a first
// one marked, since a mark stands, and nothing a pass of this rule has stated since.
func markStaleDocuments(ctx context.Context, database *store.Store, rule int) ([]string, error) {
	var marked []string
	after := "00000000-0000-0000-0000-000000000000"
	for {
		rows, err := database.Pool.Query(ctx, StaleDocumentsStatement, after, backfillBatch, rule)
		if err != nil {
			return marked, fmt.Errorf("read the documents no pass of this build's rule has stated: %w", err)
		}
		batch, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			return marked, fmt.Errorf("read the documents no pass of this build's rule has stated: %w", err)
		}
		if len(batch) == 0 {
			return marked, nil
		}
		after = batch[len(batch)-1]
		written, err := markStaleBatch(ctx, database, batch, rule)
		if err != nil {
			return marked, err
		}
		marked = append(marked, written...)
		if len(batch) < backfillBatch {
			return marked, nil
		}
	}
}

// StaleDocumentsStatement is markStaleDocuments' selection: at most $2 documents past $1, in id
// order, that no pass of the marking rule $3 or a later one has stated at their version
// (refs.UnstatedBlockIndexSQL) and that hold no mark of $3 or a higher rule. A mark of a lower
// rule is read as no mark (refs.OwedMarkAbsent), so markStaleBatch raises it to $3.
var StaleDocumentsStatement = `
	select b.artifact_id::text from document_block_versions b
	where b.artifact_id > $1::uuid and ` + refs.OwedMarkAbsent("b", "$3::integer") + `
	  and ` + refs.UnstatedBlockIndexSQL("$3::integer") + `
	order by b.artifact_id
	limit $2
`

// markStaleBatch marks the documents of one batch in one transaction, under the locks a version
// write holds while it writes a document's blocks (lockBlockDocuments), and only those that still
// hold no mark of the marking rule rule or a higher one and that no pass of that rule has stated at
// their version under them (StaleBatchMarkStatement): an index pass of this release that reached a
// document in between has stated it at the version it indexed, and owes it nothing. It answers the
// documents it marked.
func markStaleBatch(ctx context.Context, database *store.Store, batch []string, rule int) ([]string, error) {
	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin the batch of documents no pass of this build's rule has stated: %w", err)
	}
	defer tx.Rollback(ctx)
	if err := lockBlockDocuments(ctx, tx, batch); err != nil {
		return nil, err
	}
	rows, err := tx.Query(ctx, StaleBatchMarkStatement, batch, rule)
	if err != nil {
		return nil, fmt.Errorf("mark the documents no pass of this build's rule has stated: %w", err)
	}
	marked, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, fmt.Errorf("mark the documents no pass of this build's rule has stated: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit the batch of documents no pass of this build's rule has stated: %w", err)
	}
	return marked, nil
}

// StaleBatchMarkStatement is markStaleBatch's mark, at the marking rule $2, of each document of
// $1 that no pass of $2 or a later rule has stated at its version (refs.UnstatedBlockIndexSQL)
// and that holds no mark of $2 or a higher rule: a mark of a lower rule is read as no mark
// (refs.OwedMarkAbsent) and raised to $2, keeping its time, and a document that holds no mark
// takes $2 and now. It answers the documents it marked.
var StaleBatchMarkStatement = `
	update document_block_versions b set store_owed_at = coalesce(b.store_owed_at, now()), store_owed_rule = $2::integer
	where b.artifact_id = any($1::uuid[]) and ` + refs.OwedMarkAbsent("b", "$2::integer") + `
	  and ` + refs.UnstatedBlockIndexSQL("$2::integer") + `
	returning b.artifact_id::text
`
