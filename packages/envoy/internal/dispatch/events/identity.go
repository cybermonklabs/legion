package events

import (
	"context"

	"github.com/jackc/pgx/v5"
)

// Queryer is the pgx surface shared by *pgxpool.Pool and pgx.Tx, so one query
// serves both the plain-read and in-transaction paths.
type Queryer interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// OpenedEventIDs resolves each ask's identity-establishing event id: its
// ask.opened event, falling back to the earliest ask.answered/ask.resolved/
// ask.edited event for a legacy row whose opening event has since been
// pruned. This is the one place an ask's identity event is resolved, shared
// by the read API (api.attachOpenedEventIDs, and api.queueAskDetails through
// QueueOpenedEventIDs) and the anchor-refresh cascade
// (docs.refreshAnchors) — a second, stricter fallback ladder here would make
// a legacy ask readable but unrefreshable, aborting an unrelated document
// write for reasons the read API already tolerates.
//
// It is the sequential half of the one read: QueueOpenedEventIDs queues the same statement and
// reads it with the same scan, for a caller that resolves the ids beside its other reads
// (api.queueAskDetails, for the issue read's batch and every list of asks), so the two paths
// cannot drift.
func OpenedEventIDs(ctx context.Context, q Queryer, askIDs []string) (map[string]int64, error) {
	result := make(map[string]int64, len(askIDs))
	if len(askIDs) == 0 {
		return result, nil
	}
	rows, err := q.Query(ctx, OpenedEventIDsStatement, askIDs)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	if err := scanOpenedEventIDs(rows, result); err != nil {
		return nil, err
	}
	return result, nil
}

// QueueOpenedEventIDs is the batched half: it queues OpenedEventIDs's statement in b and reads its
// rows into into, keyed by ask id. No ask queues nothing and leaves into as it is, so a caller
// need not ask whether it has any.
func QueueOpenedEventIDs(b *pgx.Batch, askIDs []string, into map[string]int64) {
	if len(askIDs) == 0 {
		return
	}
	b.Queue(OpenedEventIDsStatement, askIDs).Query(func(rows pgx.Rows) error {
		return scanOpenedEventIDs(rows, into)
	})
}

// OpenedEventIDsStatement is the query both halves run, $1 the ask ids (text[]), and the one a
// statement of another reader's splices in over ask ids of its own (api's GET /api/v1/me/inbox),
// one row per ask that has an event, read by ScanOpenedEventID. The partial index
// events_ask_payload_id (migration 0030) serves it, and that index's `type in (...)` list
// mirrors this one.
const OpenedEventIDsStatement = `
		select payload->>'id', min(id)
		from events
		where type in ('ask.opened', 'ask.answered', 'ask.resolved', 'ask.edited')
		  and payload->>'id' = any($1)
		group by payload->>'id'
	`

// ScanOpenedEventID reads one row OpenedEventIDsStatement returns: the ask's id and its identity
// event's.
func ScanOpenedEventID(row pgx.Row) (askID string, eventID int64, err error) {
	err = row.Scan(&askID, &eventID)
	return askID, eventID, err
}

// scanOpenedEventIDs reads the rows OpenedEventIDsStatement returns into result, keyed by ask id.
func scanOpenedEventIDs(rows pgx.Rows, result map[string]int64) error {
	for rows.Next() {
		askID, eventID, err := ScanOpenedEventID(rows)
		if err != nil {
			return err
		}
		result[askID] = eventID
	}
	return rows.Err()
}
