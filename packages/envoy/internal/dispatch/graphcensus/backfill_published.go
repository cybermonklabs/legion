package graphcensus

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// --- the sessions and roles an older build's outbox published to ---------------------------------

// publishedDeliveriesKind is the published-delivery pass's kind, the census kind it marks
// (publishedDestinationsProjection).
const publishedDeliveriesKind = "published_destinations"

// outboxWriter is who the store records as the writer of a delivery the outbox's record states,
// outbox's graphWriter: that an event reached a session is the outbox's fact and nobody's act.
var outboxWriter = model.Actor{Kind: "system", ID: "dispatch-outbox"}

// publishedOwedPairs is, for the event e of the statement around it, each delivered_to its
// published_destinations owe the store, as (item_kind, item_id, reached_kind, reached_id): what
// this release's outbox states when it records the topics it published an event to (outbox's
// deliveredGraph) and the outbox of the store's own release states only for a comment's event
// (its deliveredItem). Its near end is the item the event is about (outbox's deliveredItem), the
// comment or the message its payload names as id, or as comment_id or message_id; its far end the
// session or the role each topic names (outbox's nodeOfSubject); both within the store's bound, the
// item a node of the store, and no delivered_to between the two stated under the event yet.
var publishedOwedPairs = `
	select item.kind as item_kind, item.id as item_id, reached.kind as reached_kind, reached.id as reached_id
	from (
		select case when e.type like 'comment.%' then '` + graphstore.KindComment + `'
		            when e.type like 'message.%' then '` + graphstore.KindMessage + `' end as kind,
		       case when e.type like 'comment.%' then coalesce(nullif(e.payload->>'id', ''), nullif(e.payload->>'comment_id', ''))
		            when e.type like 'message.%' then coalesce(nullif(e.payload->>'id', ''), nullif(e.payload->>'message_id', '')) end as id
	) item
	cross join lateral unnest(e.published_destinations) topic (name)
	cross join lateral (
		select case when topic.name like '` + contracts.AgentTopicPrefix + `%' then '` + graphstore.KindSession + `'
		            when topic.name like '` + contracts.RoleTopicPrefix + `%' then '` + graphstore.KindRole + `' end as kind,
		       case when topic.name like '` + contracts.AgentTopicPrefix + `%' then substring(topic.name from ` + agentTopicFrom + `)
		            when topic.name like '` + contracts.RoleTopicPrefix + `%' then substring(topic.name from ` + roleTopicFrom + `) end as id
	) reached
	where item.id is not null and reached.kind is not null and reached.id <> ''
	  and ` + graphstore.AddressFitsSQL(`item.id`) + ` and ` + graphstore.AddressFitsSQL(`reached.id`) + `
	  and exists (select 1 from graph_nodes subject where subject.kind = item.kind and subject.id = item.id)
	  and not exists (
		select 1 from graph_tuples t
		where t.from_kind = item.kind and t.from_id = item.id and t.kind = '` + graphstore.TupleDeliveredTo + `'
		  and t.to_kind = reached.kind and t.to_id = reached.id and t.source_event = e.id and t.asserted)`

// publishedSinceTheStore is true of an event e the store existed for, the events the census holds
// published_destinations of (publishedOfAStoredItem): an event recorded before the store's first
// migration was published to destinations no tuple could record yet.
var publishedSinceTheStore = `(` + storeRelease + ` is null or e.created_at >= ` + storeRelease + `)`

// publishedDeliveriesOwed is the events the published-delivery pass owes: each about a comment or a
// message, published since the store existed, whose published_destinations owe a delivered_to
// (publishedOwedPairs). An event holds no marker of its own, so its marker is the tuples its record
// states, which an event this release's outbox recorded holds and one an older build's outbox
// recorded may not: the store's own release states none for a message's event.
var publishedDeliveriesOwed = `
	from events e
	where cardinality(e.published_destinations) > 0 and (e.type like 'comment.%' or e.type like 'message.%')
	  and ` + publishedSinceTheStore + `
	  and exists (` + publishedOwedPairs + `)`

// publishedDeliveriesQueued is the events an older build's outbox recorded what it published to
// since this release's schema reached the database: the trigger of migration 20261122100122 queues
// each in events_published_owed as that build's record updates its published_destinations, which
// this release's outbox, whose connections carry the store's marker writer setting, never does.
// The block-index tick's catch-up reads it in place of the log (rollPasses): that record appends no
// event, and it lands after the event commits, so a tick that has read the event already would
// never read the event again to find it.
//
// kept for one release: events-published-owed
var (
	publishedDeliveriesQueued      = `from events_published_owed q`
	publishedDeliveriesQueuedBatch = `select q.event_id::text ` + publishedDeliveriesQueued + `
		where q.event_id > $1::text::bigint order by q.event_id limit $2`
)

// publishedDeliveriesPass states the delivered_to of each event an older build's outbox published
// to a session or a role that no tuple states yet, as this release's outbox states it when it
// records that event (outbox's deliveredGraph): one tuple from the comment or the message the
// event is about to the session or the role each topic names, under the event, made by the outbox,
// at the moment the event was published. The backfill walks the log since the store existed; the
// block-index tick walks the queue of what such an outbox recorded after the backfill
// (publishedDeliveriesQueued).
func publishedDeliveriesPass() inboxPass {
	return inboxPass{
		kind:      publishedDeliveriesKind,
		batch:     `select e.id::text ` + publishedDeliveriesOwed + ` and e.id > $1::text::bigint order by e.id limit $2`,
		start:     []string{"0"},
		write:     writePublishedDeliveries,
		remaining: publishedDeliveriesOwed,
	}
}

// writePublishedDeliveries takes the events rows keys names `for no key update`, in ascending id,
// the lock the outbox's record takes of each event it updates, so a record of one of them waits
// for this batch and this batch for a record under way; then states the delivered_to each still
// owes (publishedOwedPairs) and deletes their rows of the queue (publishedDeliveriesQueued). A
// record that lands after the batch queues its event again.
func writePublishedDeliveries(ctx context.Context, tx pgx.Tx, keys [][]string) (inboxWrote, error) {
	ids := column(keys, 0)
	if _, err := tx.Exec(ctx, `select 1 from events where id = any($1::text[]::bigint[]) order by id for no key update`, ids); err != nil {
		return inboxWrote{}, fmt.Errorf("lock a batch of the events an outbox published: %w", err)
	}
	rows, err := tx.Query(ctx, `
		select e.id, p.item_kind, p.item_id, p.reached_kind, p.reached_id, coalesce(e.published_at, e.created_at)
		from events e
		cross join lateral (`+publishedOwedPairs+`) p
		where e.id = any($1::text[]::bigint[]) and `+publishedSinceTheStore+`
		order by e.id, p.item_kind, p.item_id, p.reached_kind, p.reached_id
	`, ids)
	if err != nil {
		return inboxWrote{}, fmt.Errorf("read what the events an outbox published owe: %w", err)
	}
	var tuples []graphstore.Tuple
	events := map[int64]struct{}{}
	var event int64
	var fromKind, fromID, toKind, toID string
	var at time.Time
	if _, err := pgx.ForEachRow(rows, []any{&event, &fromKind, &fromID, &toKind, &toID, &at}, func() error {
		events[event] = struct{}{}
		tuples = append(tuples, graphstore.Tuple{
			FromKind: fromKind, FromID: fromID, Kind: graphstore.TupleDeliveredTo, ToKind: toKind, ToID: toID,
			At: at, Who: outboxWriter, SourceEvent: event, Asserted: true,
		})
		return nil
	}); err != nil {
		return inboxWrote{}, fmt.Errorf("read what the events an outbox published owe: %w", err)
	}
	if len(tuples) > 0 {
		if err := writeRelations(ctx, tx, tuples); err != nil {
			return inboxWrote{}, fmt.Errorf("state what a batch of events an outbox published delivered: %w", err)
		}
	}
	// kept for one release: events-published-owed
	if _, err := tx.Exec(ctx, `delete from events_published_owed where event_id = any($1::text[]::bigint[])`, ids); err != nil {
		return inboxWrote{}, fmt.Errorf("take a batch of events off the queue of what an outbox published: %w", err)
	}
	return inboxWrote{rows: int64(len(events)), tuples: int64(len(tuples))}, nil
}
