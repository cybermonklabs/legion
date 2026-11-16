package graphstore

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// EraseNode deletes node kind:id and every tuple on both sides of it, inside the caller's
// transaction: the tuples out of the node, the tuples whose far end it is, then its row. It is the
// one write of this store that deletes, and `envoy-dispatch erase` (grapherase) is its one caller:
// an erase (R1 and R5 of dispatch://INFRA-1149) takes a node's bytes away, which no tuple appended
// after them can do.
//
// The caller holds the node's row already, taken `for update` after every owner row it writes
// under and before anything else (grapherase's lockInStoreOrder), so these deletes take no row
// lock the store's lock order does not already place. A tuple is appended once and never written
// again, so no other writer's write meets a delete of one.
func EraseNode(ctx context.Context, tx pgx.Tx, kind, id string) error {
	if _, err := tx.Exec(ctx, `delete from graph_tuples where from_kind = $1 and from_id = $2`, kind, id); err != nil {
		return fmt.Errorf("delete the tuples out of %s:%s: %w", kind, id, err)
	}
	if _, err := tx.Exec(ctx, `delete from graph_tuples where to_kind = $1 and to_id = $2`, kind, id); err != nil {
		return fmt.Errorf("delete the tuples into %s:%s: %w", kind, id, err)
	}
	if _, err := tx.Exec(ctx, `delete from graph_nodes where kind = $1 and id = $2`, kind, id); err != nil {
		return fmt.Errorf("delete the node %s:%s: %w", kind, id, err)
	}
	return nil
}
