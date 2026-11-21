package graphstore

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/sjawhar/envoy/internal/dispatch/store"
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
// So the delete is by seq and the node's row goes only where no tuple of either side is left: a
// tuple that landed since the listing refuses the whole erase, naming each seq, and the caller's
// transaction rolls back having removed nothing. The operator reads the listing again, which
// then names the new tuple's row as a blocker or lists it, and runs the erase anew.
//
// Its three statements - the delete of the listed tuples, the read of what is left of the node's,
// and the delete of its row - are one batch of their own (store.RunWrite), so no Go code runs
// between them. The refusals are the callbacks of the first two, which read the count the delete
// took and the tuples the read found as Send reads their results: the statements after a refused
// one have run by then, and the refusal ends the caller's transaction, which rolls them back.
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
	return store.RunWrite(ctx, tx, func(b *store.WriteBatch) error {
		b.Queue(fmt.Sprintf("delete the tuples of %s:%s", kind, id), `delete from graph_tuples where seq = any($3::bigint[])
			and `+NodeTuplesSQL("", "$1", "$2", "$4"), kind, id, seqs, addressed).Exec(func(tag pgconn.CommandTag) error {
			if tag.RowsAffected() != int64(len(seqs)) {
				return fmt.Errorf("the erase refuses %s:%s: it listed %d tuple(s) of the node and deleted %d, so the tuples moved while it ran; nothing was removed, and the erase can be run again",
					kind, id, len(seqs), tag.RowsAffected())
			}
			return nil
		})
		what := fmt.Sprintf("read the tuples left of %s:%s", kind, id)
		b.Queue(what, `
			select seq || ' (' || from_kind || ':' || from_id || ' ' || kind || ' ' || to_kind || ':' || to_id || ')'
			from graph_tuples
			where `+NodeTuplesSQL("", "$1", "$2", "$3")+`
			order by seq`, kind, id, addressed).Query(func(rows pgx.Rows) error {
			left, err := pgx.CollectRows(rows, pgx.RowTo[string])
			if err != nil {
				return fmt.Errorf("%s: %w", what, err)
			}
			if len(left) > 0 {
				return fmt.Errorf("the erase refuses %s:%s: %s after its listing was read, so the erase would delete a tuple nobody listed; nothing was removed, and the erase can be run again",
					kind, id, landed(left))
			}
			return nil
		})
		b.Queue(fmt.Sprintf("delete the node %s:%s", kind, id), `delete from graph_nodes where kind = $1 and id = $2`, kind, id)
		return nil
	})
}

// NodeTuplesSQL is true of a tuple of either side of one node: one out of the node as the store
// keys it, or one into it under that kind or under the kind the graph addresses it by
// (AddressKind). kind, id and addressed are SQL expressions of the caller's query - the node's
// kind, its id and AddressKind of the two - and prefix is what the query writes before a column
// of graph_tuples: its alias and a dot, or nothing where it names the table alone. A finding of a
// lens is keyed as a block, and a tuple into it - the part_of of a lens invoked on it - names it
// lens_block, so every read and delete of a node's tuples takes both: the erase lists the tuples
// it deletes (grapherase's readTuples), deletes them by seq and reads what is left over this one
// set (EraseNode), and its blockers read the tuples into the node over NodeTuplesIntoSQL.
func NodeTuplesSQL(prefix, kind, id, addressed string) string {
	return `((` + prefix + `from_kind = ` + kind + ` and ` + prefix + `from_id = ` + id + `) or (` +
		NodeTuplesIntoSQL(prefix, kind, id, addressed) + `))`
}

// NodeTuplesIntoSQL is the half of NodeTuplesSQL into the node: a tuple whose far end is the node,
// under either kind.
func NodeTuplesIntoSQL(prefix, kind, id, addressed string) string {
	return prefix + `to_kind in (` + kind + `, ` + addressed + `) and ` + prefix + `to_id = ` + id
}

// landed names the tuples that landed since the listing as the refusal reads them.
func landed(left []string) string {
	if len(left) == 1 {
		return "the tuple " + left[0] + " landed"
	}
	return strconv.Itoa(len(left)) + " tuples landed, " + strings.Join(left, ", ")
}
