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

// Readable is the read rule a statement of this package holds the events it reads to, for a
// caller that answers a reader: Where, a predicate over the events row the statement reads
// (named events) that binds the reader's viewer at $2, and Viewer, the argument bound there. The
// rule is the api package's (eventReadableSQL), which this package cannot import: the access
// package it is built from imports this one.
type Readable struct {
	Where  string
	Viewer any
}

// EveryEvent is the server's own read, for a caller whose answer reaches no single reader - a
// write building an event's payload, the anchor-refresh cascade - which every event passes, and
// which binds nothing at $2.
var EveryEvent = Readable{Where: "true"}

// Args is a statement's arguments under the rule: first at $1, and the viewer at $2 where the
// rule binds one.
func (r Readable) Args(first any) []any {
	if r.Viewer == nil {
		return []any{first}
	}
	return []any{first, r.Viewer}
}

// OpenedEventIDs resolves each ask's identity-establishing event id: its
// ask.opened event, falling back to the earliest ask.answered/ask.resolved/
// ask.edited event for a legacy row whose opening event has since been
// pruned, among the events readable passes. This is the one place an ask's identity event is
// resolved, shared by the read API (api.attachOpenedEventIDs, and api.queueAskDetails through
// QueueOpenedEventIDs) and the anchor-refresh cascade
// (docs.refreshAnchors) — a second, stricter fallback ladder here would make
// a legacy ask readable but unrefreshable, aborting an unrelated document
// write for reasons the read API already tolerates.
//
// It is the sequential half of the one read: QueueOpenedEventIDs queues the same statement and
// reads it with the same scan, for a caller that resolves the ids beside its other reads
// (api.queueAskDetails, for the issue read's batch and every list of asks), so the two paths
// cannot drift.
func OpenedEventIDs(ctx context.Context, q Queryer, askIDs []string, readable Readable) (map[string]int64, error) {
	result := make(map[string]int64, len(askIDs))
	if len(askIDs) == 0 {
		return result, nil
	}
	rows, err := q.Query(ctx, openedEventIDsStatement(readable.Where), readable.Args(askIDs)...)
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
func QueueOpenedEventIDs(b *pgx.Batch, askIDs []string, readable Readable, into map[string]int64) {
	if len(askIDs) == 0 {
		return
	}
	b.Queue(openedEventIDsStatement(readable.Where), readable.Args(askIDs)...).Query(func(rows pgx.Rows) error {
		return scanOpenedEventIDs(rows, into)
	})
}

// openedEventIDsStatement is the query both halves run, $1 the ask ids, held to readable. The
// partial index events_ask_payload_id (migration 0030) serves it, and that index's `type in (...)`
// list mirrors this one.
func openedEventIDsStatement(readable string) string {
	return `
		select payload->>'id', min(id)
		from events
		where type in ('ask.opened', 'ask.answered', 'ask.resolved', 'ask.edited')
		  and payload->>'id' = any($1) and ` + readable + `
		group by payload->>'id'
	`
}

// scanOpenedEventIDs reads the rows openedEventIDsStatement returns into result, keyed by ask id.
func scanOpenedEventIDs(rows pgx.Rows, result map[string]int64) error {
	for rows.Next() {
		var askID string
		var eventID int64
		if err := rows.Scan(&askID, &eventID); err != nil {
			return err
		}
		result[askID] = eventID
	}
	return rows.Err()
}
