package intake

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/record"
)

// seenEvent is one event of an issue of the daemon's own Dispatch project that the Dispatch
// consumer received, whatever its kind: the issue, and the event's sequence on that issue's log.
// Every event moves an issue's sequence, a message as much as an update, so the newest one the
// consumer has handled or dropped is the cursor a queued write to Dispatch waits on
// (record.Issue.SeenSeq; decision 11 of dispatch://INFRA-4730): a write that read the issue at
// sequence S writes only once the record has seen every event up to S.
type seenEvent struct {
	key string
	seq int64
}

type seenEventKey struct{}

// withSeenEvent is ctx carrying the event the consumer delivers, for ApplyFact to advance its
// issue's cursor in the fact's own transaction (recordSeen).
func withSeenEvent(ctx context.Context, seen *seenEvent) context.Context {
	if seen == nil {
		return ctx
	}
	return context.WithValue(ctx, seenEventKey{}, *seen)
}

// recordSeen advances the cursor of the issue whose event ctx carries (withSeenEvent) to that
// event's sequence, in tx, so the cursor moves with the fact that handled the event or not at
// all (record.Store.AdvanceSeenSeq: forward only, and a recorded issue's alone). A context with
// no event (a fact from the API, supervision or the outbox) moves nothing.
func recordSeen(ctx context.Context, tx pgx.Tx) error {
	seen, ok := ctx.Value(seenEventKey{}).(seenEvent)
	if !ok {
		return nil
	}
	return record.NewStore().AdvanceSeenSeq(ctx, tx, seen.key, seen.seq)
}

// seeDropped advances the cursor of the issue of an event no handler takes (a message, say),
// which decodes to no fact and so has no fact transaction to do it in: a transaction of its own,
// which takes no fact lock, since the cursor is the one thing it writes and it only moves
// forward.
func seeDropped(ctx context.Context, pool Pool, seen seenEvent) error {
	tx, err := pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return fmt.Errorf("record that event %d of %s was seen: %w", seen.seq, seen.key, err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := record.NewStore().AdvanceSeenSeq(ctx, tx, seen.key, seen.seq); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("record that event %d of %s was seen: %w", seen.seq, seen.key, err)
	}
	return nil
}

// DispatchEventsSeen says whether the record has had, of each of events up to sequence through,
// all the Dispatch consumer would make of it: events are issue key's, as Dispatch's event log
// serves them (dispatch.Client.EventsAfter), the shape the stream carries, and key is an issue of
// the daemon's own project, whose consumer decodes them. An event no handler takes — one that
// decodes to no fact, or one the consumer would terminate as poison — changes no record; an event
// that decodes to a fact counts once the consumer applied it (processed_events under its
// envelope's id, `dispatch-<id>`, Applied). It is the outbox's reading of R20's wait when the
// cursor (record.Issue.SeenSeq) has not reached through: the events the consumer handled before
// the cursor was kept, and any it dropped or terminated, are read as the consumer left them
// (decision 11 of dispatch://INFRA-4730).
func DispatchEventsSeen(ctx context.Context, pool Pool, key string, events []json.RawMessage, through int64) (bool, error) {
	project, _, _ := strings.Cut(key, "-")
	var ids []string
	for _, raw := range events {
		var header struct {
			ID  int64 `json:"id"`
			Seq int64 `json:"seq"`
		}
		if err := json.Unmarshal(raw, &header); err != nil {
			return false, fmt.Errorf("read the events of %s: %w", key, err)
		}
		if header.Seq > through {
			break
		}
		fact, _, _, err := decodeDispatchIssueEvent(key, project, string(raw))
		if err != nil || fact == nil {
			continue
		}
		ids = append(ids, fmt.Sprintf("dispatch-%d", header.ID))
	}
	applied, err := Applied(ctx, pool, "dispatch", ids)
	if err != nil {
		return false, err
	}
	return len(applied) == len(ids), nil
}
