package graphcensus

import (
	"context"
	"fmt"
	"strconv"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/refs"
)

// StoreBackfill is one backfill pass of this branch the sizing counts the rows of: the rows its
// next run writes once, which the database's history holds now and no steady rate counts. Owed is
// how many projection rows still lack what the pass writes, and Nodes and Tuples what it writes
// for each.
type StoreBackfill struct {
	Name, MovedBy, Driver string
	Nodes, Tuples         int
	Owed                  int64
}

// writes is how many nodes and how many tuples the pass's next run writes: what it writes for
// each row it owes, times those rows.
func (b StoreBackfill) writes() (nodes, tuples int64) {
	return b.Owed * int64(b.Nodes), b.Owed * int64(b.Tuples)
}

// versionBackfillOwed counts the versions the version backfill writes a node for (backfillVersions):
// every version inside the address bound whose node the store does not hold, or every one of them
// on a database the store does not exist in yet, which R4 takes its readings on.
func versionBackfillOwed(ctx context.Context, tx pgx.Tx) (int64, error) {
	var held bool
	if err := tx.QueryRow(ctx, `select to_regclass('graph_nodes') is not null`).Scan(&held); err != nil {
		return 0, fmt.Errorf("look up the store: %w", err)
	}
	read := `select count(*) ` + missingVersionsSQL
	if !held {
		read = `select count(*) from artifact_versions v join artifacts a on a.id = v.artifact_id
			where octet_length(` + versionAddress + `) <= ` + addressBound
	}
	var owed int64
	if err := tx.QueryRow(ctx, read).Scan(&owed); err != nil {
		return 0, fmt.Errorf("count the versions the backfill owes a node: %w", err)
	}
	return owed, nil
}

// unstatedBlocksOwed counts the blocks the backfill's marking (markStaleDocuments) owes the store
// the values of, once: values, every document_blocks row inside the address bound of a document
// whose document_block_versions row holds no mark at all and that no pass of this build's rule has
// stated at its version (refs.UnstatedBlockIndexSQL) - each of which takes its rank, its text and
// its placement from the reconciliation's pass over the document; and tops, those of them at the
// top of an issue's spec, Verification or plan document, each of which takes the retraction of its
// part_of to its document and its part_of to the issue. A document that holds a mark already, of
// this build's rule or a lower one, is owed that pass by its mark, which the reconciliation of this
// build lists either way (refs.OwedMarkListed): the marking raises a lower rule's mark to this
// build's rule (refs.OwedMarkAbsent) and owes the store nothing more for it. A database the
// migration that adds the stated record (20261120130000) has not reached, which the sizing takes
// its readings on too, records no pass of this rule on any row, so every row of it is owed, and one
// that holds no parent_block_id column, from before the store, says nothing of which rows are at
// the top, so every row of such a document is counted there.
func unstatedBlocksOwed(ctx context.Context, tx pgx.Tx) (values, tops int64, err error) {
	var stated, parents bool
	if err := tx.QueryRow(ctx, `
		select (select count(*) from pg_attribute
		        where attrelid = to_regclass('document_block_versions') and not attisdropped
		          and attname in ('store_owed_at', 'stated_version', 'stated_rule')) = 3,
		       exists (select 1 from information_schema.columns
		               where table_schema = current_schema() and table_name = 'document_blocks'
		                 and column_name = 'parent_block_id')
	`).Scan(&stated, &parents); err != nil {
		return 0, 0, fmt.Errorf("look up the block index's record of the pass that stated each document: %w", err)
	}
	top := `true`
	if parents {
		top = `d.parent_block_id is null`
	}
	marked := ``
	if stated {
		marked = `
		  and exists (select 1 from document_block_versions b
		              where b.artifact_id = d.artifact_id and b.store_owed_at is null
		                and ` + refs.UnstatedBlockIndexSQL(strconv.Itoa(refs.BlockIndexMarkingRule)) + `)`
	}
	if err := tx.QueryRow(ctx, `
		select count(*), count(*) filter (where `+refs.DocumentBandSQL("a")+` <> '`+string(refs.RankBandOther)+`' and `+top+`)
		from document_blocks d
		join artifacts a on a.id = d.artifact_id
		where octet_length(d.node_id) <= `+addressBound+marked).Scan(&values, &tops); err != nil {
		return 0, 0, fmt.Errorf("count the blocks the backfill's marking owes: %w", err)
	}
	return values, tops, nil
}
