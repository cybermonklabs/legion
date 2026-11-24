package store

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
)

// QueuedUpdateAsks is every role of issue whose ask of round still waits in the outbox: an
// update-ask row (record.UpdateAsk) of that round the outbox has not finished, whatever its
// attempts so far. The outbox deletes a row when it finishes it, so a row that is there is an ask
// still in flight: not yet published, waiting to publish again, or published with its claim's
// mark not yet written.
//
// The daemon's sweep reads it before it asks (internal/daemon's askRound), so one claim has one
// ask of a round in flight at a time. The sweep no longer marks a claim when it enqueues an ask:
// the outbox marks it once the ask reached a session (supervise's Machine.PublishUpdateAsk), and
// without this read every sweep between the enqueue and that mark would enqueue the claim again.
//
// One read answers for every claim of the issue, and it reads the outbox through its issue index
// (outbox_issue), so it costs one indexed query for an issue the sweep asks and nothing for one it
// does not.
func (s *Store) QueuedUpdateAsks(ctx context.Context, issue, round string) (map[claim.Role]bool, error) {
	queued := map[claim.Role]bool{}
	err := TransactWith(ctx, s.pool, pgx.TxOptions{AccessMode: pgx.ReadOnly}, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `select payload->>'role' from outbox
			where issue = $1 and kind = 'update_ask' and payload->>'round' = $2`, issue, round)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var role string
			if err := rows.Scan(&role); err != nil {
				return err
			}
			queued[claim.Role(role)] = true
		}
		return rows.Err()
	})
	if err != nil {
		return nil, fmt.Errorf("read the queued update asks of %s for %s: %w", issue, round, err)
	}
	return queued, nil
}
