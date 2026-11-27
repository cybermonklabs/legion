package store

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
)

// QueuedUpdateAsks is every claim of issue whose ask of round still waits in the outbox: an
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
// A row is read by the claim it names (record.UpdateAsk.Claim): two claims of one issue can hold
// one role — the agent of a key on a step of another role, beside that role's own claim of an
// earlier version of the plan, or two agents at two steps of the role — and the one's ask in
// flight is no ask of the other. A row the release before wrote names no claim, and stands for
// each claim of its role.
//
// One read answers for every claim of the issue, and it reads the outbox through its issue index
// (outbox_issue), so it costs one indexed query for an issue the sweep asks and nothing for one it
// does not.
func (s *Store) QueuedUpdateAsks(ctx context.Context, issue, round string) (QueuedAsks, error) {
	queued := QueuedAsks{claims: map[claim.Token]bool{}, roles: map[claim.Role]bool{}}
	err := TransactWith(ctx, s.pool, pgx.TxOptions{AccessMode: pgx.ReadOnly}, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `select coalesce(payload->>'claim', ''), payload->>'role' from outbox
			where issue = $1 and kind = 'update_ask' and payload->>'round' = $2`, issue, round)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var token, role string
			if err := rows.Scan(&token, &role); err != nil {
				return err
			}
			if token != "" {
				queued.claims[claim.Token(token)] = true
				continue
			}
			queued.roles[claim.Role(role)] = true
		}
		return rows.Err()
	})
	if err != nil {
		return QueuedAsks{}, fmt.Errorf("read the queued update asks of %s for %s: %w", issue, round, err)
	}
	return queued, nil
}

// QueuedAsks is the asks of one round still queued for an issue's claims (QueuedUpdateAsks): the
// claims the rows name, and the role of each row that names none.
type QueuedAsks struct {
	claims map[claim.Token]bool
	roles  map[claim.Role]bool
}

// Of is whether an ask of the round still waits for the claim token, which holds role: a row that
// names that claim, or a row of its role that names none.
func (q QueuedAsks) Of(token claim.Token, role claim.Role) bool {
	return q.claims[token] || q.roles[role]
}
