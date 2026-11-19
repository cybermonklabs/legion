package graphstore

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
)

// EraseNode deletes node kind:id and the tuples seqs names, inside the caller's transaction: the
// tuples of both sides of the node, then its row. It is the one write of this store that deletes,
// and `envoy-dispatch erase` (grapherase) is its one caller: an erase (R1 and R5 of
// this change) takes a node's bytes away, which no tuple appended after them can do.
//
// seqs are the tuples the erase listed, by seq, and the delete takes those rows and no others:
// the erase's transaction is READ COMMITTED, so a tuple another writer commits after the listing
// is read - another document's version citing this node, whose writer takes neither this node's
// owner row nor its row - is visible to this statement, and a delete by predicate would take it
// away although nothing listed it. The run then answers that it erased a tuple no operator ever
// saw, and the row the tuple projected stands with no tuple behind it.
//
// So the delete is by seq and the node's row goes only once no tuple of either side is left: a
// tuple that landed since the listing refuses the whole erase, naming each seq, and the caller's
// transaction rolls back having removed nothing. The operator reads the listing again, which
// then names the new tuple's row as a blocker or lists it, and runs the erase anew.
//
// The caller holds the node's row already, taken `for update` after every owner row it writes
// under and before anything else (grapherase's lockInStoreOrder), so these deletes take no row
// lock the store's lock order does not already place. A tuple is appended once and never written
// again, so no other writer's write meets a delete of one.
//
// A tuple into the node names it by the kind the graph addresses it by (AddressKind), so the tuples
// of the node are read and deleted under either kind (NodeTuplesSQL).
func EraseNode(ctx context.Context, tx pgx.Tx, kind, id string, seqs []int64) error {
	addressed := AddressKind(kind, id)
	tag, err := tx.Exec(ctx, `delete from graph_tuples where seq = any($3::bigint[])
		and `+NodeTuplesSQL("", "$1", "$2", "$4"), kind, id, seqs, addressed)
	if err != nil {
		return fmt.Errorf("delete the tuples of %s:%s: %w", kind, id, err)
	}
	if tag.RowsAffected() != int64(len(seqs)) {
		return fmt.Errorf("the erase refuses %s:%s: it listed %d tuple(s) of the node and deleted %d, so the tuples moved while it ran; nothing was removed, and the erase can be run again",
			kind, id, len(seqs), tag.RowsAffected())
	}
	left, err := tuplesLeft(ctx, tx, kind, id, addressed)
	if err != nil {
		return err
	}
	if len(left) > 0 {
		return fmt.Errorf("the erase refuses %s:%s: %s after its listing was read, so the erase would delete a tuple nobody listed; nothing was removed, and the erase can be run again",
			kind, id, landed(left))
	}
	if _, err := tx.Exec(ctx, `delete from graph_nodes where kind = $1 and id = $2`, kind, id); err != nil {
		return fmt.Errorf("delete the node %s:%s: %w", kind, id, err)
	}
	return nil
}

// RedactValues rewrites the value each tuple of seqs carries to the value beside it in values,
// inside the caller's transaction: the one write of this store that moves a tuple it keeps, and
// `envoy-dispatch erase` (grapherase) is its one caller. A block's text value is its whole slice of
// its version, the blocks inside it included (refs.IndexBlocks), so the text of every block that
// holds an erased block quotes the erased words, and those tuples are another node's, which the
// erase keeps: it rewrites the quote in them, as it rewrites the same quote in the
// document_block_texts row they project, and leaves each tuple's seq, time, writer and cause as
// they were.
//
// It rewrites value tuples alone, by seq, and refuses unless it rewrote every one it was handed,
// so a tuple that is no value or no longer there rolls the caller's transaction back having moved
// nothing. The caller holds the owner rows of the document those blocks stand in, which every
// writer of a block's values holds first, so no pass of the block index writes these tuples'
// blocks meanwhile.
func RedactValues(ctx context.Context, tx pgx.Tx, seqs []int64, values []string) error {
	if len(seqs) == 0 {
		return nil
	}
	tag, err := tx.Exec(ctx, `
		update graph_tuples t set to_id = redacted.value
		from unnest($1::bigint[], $2::text[]) as redacted (seq, value)
		where t.seq = redacted.seq and t.to_kind = '`+KindValue+`'`, seqs, values)
	if err != nil {
		return fmt.Errorf("redact %d value tuple(s): %w", len(seqs), err)
	}
	if tag.RowsAffected() != int64(len(seqs)) {
		return fmt.Errorf("the erase refuses to redact %d value tuple(s): %d of them stand as values, so the tuples moved while it ran; nothing was rewritten, and the erase can be run again",
			len(seqs), tag.RowsAffected())
	}
	return nil
}

// NodeTuplesSQL is true of a tuple of either side of one node: one out of the node as the store
// keys it, or one into it under that kind or under the kind the graph addresses it by
// (AddressKind). kind, id and addressed are SQL expressions of the caller's query - the node's
// kind, its id and AddressKind of the two - and prefix is what the query writes before a column
// of graph_tuples: its alias and a dot, or nothing where it names the table alone. A finding of a
// lens is keyed as a block, and a tuple into it - the part_of of a lens invoked on it - names it
// lens_block, so every read and delete of a node's tuples takes both: the erase lists the tuples
// it deletes (grapherase's readTuples), deletes them by seq (EraseNode) and reads what is left
// (tuplesLeft) over this one set, and its blockers read the tuples into the node over
// NodeTuplesIntoSQL.
func NodeTuplesSQL(prefix, kind, id, addressed string) string {
	return `((` + prefix + `from_kind = ` + kind + ` and ` + prefix + `from_id = ` + id + `) or (` +
		NodeTuplesIntoSQL(prefix, kind, id, addressed) + `))`
}

// NodeTuplesIntoSQL is the half of NodeTuplesSQL into the node: a tuple whose far end is the node,
// under either kind.
func NodeTuplesIntoSQL(prefix, kind, id, addressed string) string {
	return prefix + `to_kind in (` + kind + `, ` + addressed + `) and ` + prefix + `to_id = ` + id
}

// tuplesLeft is every tuple of either side of the node the delete by seq left, which is every
// tuple another writer appended since the listing was read, by seq with the fact it states.
func tuplesLeft(ctx context.Context, tx pgx.Tx, kind, id, addressed string) ([]string, error) {
	rows, err := tx.Query(ctx, `
		select seq || ' (' || from_kind || ':' || from_id || ' ' || kind || ' ' || to_kind || ':' || to_id || ')'
		from graph_tuples
		where `+NodeTuplesSQL("", "$1", "$2", "$3")+`
		order by seq`, kind, id, addressed)
	if err != nil {
		return nil, fmt.Errorf("read the tuples left of %s:%s: %w", kind, id, err)
	}
	left, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, fmt.Errorf("read the tuples left of %s:%s: %w", kind, id, err)
	}
	return left, nil
}

// landed names the tuples that landed since the listing as the refusal reads them.
func landed(left []string) string {
	if len(left) == 1 {
		return "the tuple " + left[0] + " landed"
	}
	return strconv.Itoa(len(left)) + " tuples landed, " + strings.Join(left, ", ")
}
