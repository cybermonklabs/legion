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
func EraseNode(ctx context.Context, tx pgx.Tx, kind, id string, seqs []int64) error {
	tag, err := tx.Exec(ctx, `delete from graph_tuples where seq = any($3::bigint[])
		and ((from_kind = $1 and from_id = $2) or (to_kind = $1 and to_id = $2))`, kind, id, seqs)
	if err != nil {
		return fmt.Errorf("delete the tuples of %s:%s: %w", kind, id, err)
	}
	if tag.RowsAffected() != int64(len(seqs)) {
		return fmt.Errorf("the erase refuses %s:%s: it listed %d tuple(s) of the node and deleted %d, so the tuples moved while it ran; nothing was removed, and the erase can be run again",
			kind, id, len(seqs), tag.RowsAffected())
	}
	left, err := tuplesLeft(ctx, tx, kind, id)
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

// tuplesLeft is every tuple of either side of the node the delete by seq left, which is every
// tuple another writer appended since the listing was read, by seq with the fact it states.
func tuplesLeft(ctx context.Context, tx pgx.Tx, kind, id string) ([]string, error) {
	rows, err := tx.Query(ctx, `
		select seq || ' (' || from_kind || ':' || from_id || ' ' || kind || ' ' || to_kind || ':' || to_id || ')'
		from graph_tuples
		where (from_kind = $1 and from_id = $2) or (to_kind = $1 and to_id = $2)
		order by seq`, kind, id)
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
