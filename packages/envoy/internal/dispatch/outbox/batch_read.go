package outbox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// A batch's read: the rows of pending events it plans from, each with its owner's routing and
// last_seq, read out whole before anything is sent (readBatch). batch.go plans and sends them.

// pendingEvent is one row of a batch, read out before anything is published: an open cursor holds
// its pooled connection until it closes, and publishing a batch reads followers and authors through
// the same pool, so the rows are drained first and the work done after.
type pendingEvent struct {
	event model.Event
	// owner is the issue key, document id or project key whose events this one is ordered among,
	// and empty for an event with none.
	owner string
	// due is whether the event may be sent now: it was never tried, or its back-off has passed.
	// An event that is not due is read only as a blocker of its owner's later events
	// (planChains): it is never sent, and the record writes nothing for it but what it owes now
	// and the owner's last_seq it read (owed_keys, owed_seq, owedAgain).
	due bool
	// nextAttempt is when an event that is not due falls due, on the database's clock.
	nextAttempt  *time.Time
	attempts     int
	destinations []string
	slug         string
	routing      routing
	// candidates are the owner's events that are due and that the batch's choice of owners found
	// (readBatch), and windowSize how many of the owner's events the batch read before it kept the
	// first batchSize due ones: a candidate a window read whole does not hold is one past it.
	candidates []int64
	windowSize int
	// ownerSeq is the owner's last_seq as the batch's read saw it (readBatchSQL): the read sees
	// every event of the owner with a seq at most this and the routing they were written with, and
	// every event written after it has a higher one. The record writes it beside what the event
	// owes (events.owed_seq, recordBatch), so a copy of a mention reads whether a write came after
	// that record (model.MentionCopyHolderSQL). It is read beside the event's id, before every
	// column a row can fail to scan, so a row that did not scan has it too (readRows).
	ownerSeq int
	// failure is why the row cannot be published as it was read - it did not scan, or its actor or
	// payload JSON did not decode - and failureLog is the predicate it reports under. Such an
	// event has no destinations: it is backed off, and holds back no other event.
	failure    error
	failureLog string
}

// routing is what the batch read off the event's issue when it publishes, not what it was when the
// event was written: the owner's route (the issue's, or the issue's whose document the event is
// about), and the session holding the issue's claim ("" when the issue is unclaimed or a person
// holds it, since a person has no Envoy topic). Every batch reads them again (readBatchSQL), so a
// retry after the route or the claim moved reaches the new destination.
type routing struct {
	route    *string
	claimant string
}

// readBatchSQL chooses a batch and reads it, writing nothing. It chooses owners that have a due
// event: the lowest-id events never tried (events_outbox_fresh) and the earliest back-offs that
// have fallen due (events_outbox_retry), batchSize of each, grouped by owner, the owner first
// whose due event has the lowest id. For each chosen owner it reads that owner's unpublished events
// in id order (events_outbox_owner), batchSize at most, due or not: an event not due holds the
// later ones back from its unreached destinations. An event with no owner has no destination and
// forms no chain, and is read alone. It reads no event of an owner $2 names, the owners the
// batches running beside this one hold in flight (lanes), so no two batches ever take one owner
// and an owner waiting on a send holds back only its own events. With $3, the private-delivery
// hold (Deps.holdRead), it chooses no owner whose project the database holds private
// (privateOwnerSQL): such an owner is never read, so it takes no place among the batchSize
// owners a batch chooses and holds back no public owner's event, and every event of it waits,
// unread and unwritten, for a read without $3. A pending subscription command
// (api/subscribers.go) is in no index this reads, and neither is a backed-off event of an owner
// the batch does not choose. Every predicate on pending is spelled as the indexes spell it
// (migration 20261101143203_outbox_claim), which is what lets Postgres prove the indexes' predicates from the query's.
//
// Of the rows read, the batch keeps every row before its batchSize-th due one in id order, and that
// row: at most batchSize events are sent per batch, and each owner keeps a prefix of its events, the
// owners' events interleaved as they were written, so a backlog over many owners makes short chains
// rather than long ones for a few. The choice carries only each row's id and back-off: the event's
// own columns, and the issue (its route and claim, routing), document and project columns a
// publish needs, are joined onto the kept rows, the owner's last_seq among them (ownerSeq), read
// in this same statement and so as of the routing it reads. The event's id and that last_seq are
// its first two columns, which no row fails to scan, so a row that fails a later one is known by
// both (readRows). A kept row's event is read by its id in a lateral subquery with a limit, which
// Postgres does not flatten into a join, so each is one lookup of events_pkey: as a join, the
// planner, which expects many more kept rows than a batch keeps, merges them with a walk of every
// event's id.
var readBatchSQL = `
	with fresh as (
		select id, coalesce(issue_key, artifact_id::text, project_key) as owner, next_attempt_at
		from events
		where published_at is null and next_attempt_at is null
		  and (payload ->> 'pending') is distinct from 'true'
		  and coalesce(issue_key, artifact_id::text, project_key, '') <> all($2::text[])
		  and not ($3::boolean and ` + privateOwnerSQL("events") + `)
		order by id
		limit $1
	), retried as (
		select id, coalesce(issue_key, artifact_id::text, project_key) as owner, next_attempt_at
		from events
		where published_at is null and next_attempt_at is not null
		  and (payload ->> 'pending') is distinct from 'true'
		  and coalesce(issue_key, artifact_id::text, project_key, '') <> all($2::text[])
		  and not ($3::boolean and ` + privateOwnerSQL("events") + `)
		  and next_attempt_at <= now()
		order by next_attempt_at, id
		limit $1
	), due as (
		select id, owner, next_attempt_at from fresh
		union all
		select id, owner, next_attempt_at from retried
	), owners as (
		select owner, min(id) as first, array_agg(id order by id) as candidates
		from due
		where owner is not null
		group by owner
		order by first
		limit $1
	), chosen as (
		select o.first, o.candidates, w.id, w.next_attempt_at
		from owners o
		cross join lateral (
			select e.id, e.next_attempt_at
			from events e
			where coalesce(e.issue_key, e.artifact_id::text, e.project_key) = o.owner
			  and e.published_at is null
			  and (e.payload ->> 'pending') is distinct from 'true'
			order by e.id
			limit $1
		) w
		union all
		select d.id, array[d.id], d.id, d.next_attempt_at
		from due d
		where d.owner is null
	), ranked as (
		select chosen.*,
		       (next_attempt_at is null or next_attempt_at <= now()) as due,
		       count(*) filter (where next_attempt_at is null or next_attempt_at <= now())
		           over (order by id) as sendable,
		       count(*) over (partition by first) as window_size
		from chosen
	)
	select e.id, coalesce(i.last_seq, ar.last_seq, p.last_seq, 0),
	       e.issue_key, e.artifact_id::text, coalesce(e.project_key, i.project_key, ar.project_key, ''),
	       e.seq, e.type, e.actor, e.notify, e.created_at, e.payload, e.attempt_count,
	       e.published_destinations, coalesce(ar.slug, ''), coalesce(i.route, ai.route),
	       coalesce(case when i.claimed_by ->> 'kind' = 'session' then i.claimed_by ->> 'id' end, ''),
	       coalesce(e.issue_key, e.artifact_id::text, e.project_key), r.due, r.next_attempt_at, r.candidates,
	       r.window_size
	from ranked r
	cross join lateral (select * from events where id = r.id limit 1) e
	left join issues i on i.key = e.issue_key
	left join artifacts ar on ar.id = e.artifact_id
	left join issues ai on ai.key = ar.issue_key
	left join projects p on p.key = e.project_key
	where r.sendable - r.due::int < $1
	order by r.first, r.id
`

// privateOwnerSQL is whether the events row alias names belongs to an owner whose project the
// database holds private (model.PrivateOwnerSQL), the one test the private-delivery hold makes of a
// row: in the batch's read (readBatchSQL), the fallback pass's claim (claimFallbackCopies), the
// retry timer (Outbox.nextRetry) and the count of what the hold keeps (privateHeldSQL), and in the
// event log's refusal past that count's limit (events.Broker.HoldPrivate).
func privateOwnerSQL(alias string) string {
	return model.PrivateOwnerSQL(alias)
}

// readBatch reads a batch (readBatchSQL), in owner order and each owner's events in id order. It
// writes no row. stopped reports a row that failed to scan, which ends pgx's cursor: the rows after
// it are not read, and the batch is the rows before it and that row, backed off. The read leaves
// the owners the batches running beside this one hold alone, and takes the owners of the rows it
// read, both under the lanes' one lock (lanes.reserve): publishBatch gives each of them back.
// excluded reports that owners were left out of it, which the scan reads as a batch that may have
// read past their events (batchResult.excluded).
func readBatch(ctx context.Context, deps Deps) (pending []pendingEvent, stopped, excluded bool, err error) {
	return deps.lanes.reserve(func(inFlight []string) ([]pendingEvent, bool, error) {
		return readRows(ctx, deps, inFlight)
	})
}

// readRows is readBatch's statement, run with no other batch's statement beside it (lanes), and
// its rows drained before it gives the gate back: an open cursor holds one of the outbox pool's
// two connections.
func readRows(ctx context.Context, deps Deps, inFlight []string) (pending []pendingEvent, stopped bool, err error) {
	deps.lanes.lock()
	defer deps.lanes.unlock()
	rows, err := deps.Store.Pool.Query(ctx, readBatchSQL, batchSize, inFlight, deps.holdRead())
	if err != nil {
		return nil, false, fmt.Errorf("read the outbox's batch: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var item pendingEvent
		var actor, payload []byte
		var destinations []pgtype.Text
		var owner *string
		if err := rows.Scan(
			&item.event.ID,
			&item.ownerSeq,
			&item.event.IssueKey,
			&item.event.ArtifactID,
			&item.event.Project,
			&item.event.Seq,
			&item.event.Type,
			&actor,
			&item.event.Notify,
			&item.event.CreatedAt,
			&payload,
			&item.attempts,
			&destinations,
			&item.slug,
			&item.routing.route,
			&item.routing.claimant,
			&owner,
			&item.due,
			&item.nextAttempt,
			&item.candidates,
			&item.windowSize,
		); err != nil {
			// pgx scans positionally and treats a failure as fatal to the cursor, so this row ends
			// the batch and rows.Err() below repeats the same error. e.id and the owner's last_seq
			// are the first two columns, which no row fails to scan - a created_at of infinity, which
			// no time.Time holds, fails a later one - so the row is known by both: it is carried on
			// as a failure, backed off like any other event that cannot be published, and recorded
			// as of that last_seq owing nothing (owedKeys), so it holds no copy of a mention.
			item.failure = fmt.Errorf("read event: %w", err)
			item.failureLog = "dispatch outbox: read event"
			item.due = true
			pending = append(pending, item)
			stopped = true
			continue
		}
		if owner != nil {
			item.owner = *owner
		}
		for _, destination := range destinations {
			if !destination.Valid {
				item.failure = errors.New("read event: published_destinations holds a null")
				item.failureLog = "dispatch outbox: read event"
				break
			}
			item.destinations = append(item.destinations, destination.String)
		}
		if item.failure == nil {
			if err := json.Unmarshal(actor, &item.event.Actor); err != nil {
				item.failure = fmt.Errorf("decode event actor: %w", err)
				item.failureLog = "dispatch outbox: decode event actor"
			} else if err := json.Unmarshal(payload, &item.event.Payload); err != nil {
				item.failure = fmt.Errorf("decode event payload: %w", err)
				item.failureLog = "dispatch outbox: decode event payload"
			}
		}
		pending = append(pending, item)
	}
	// Closed before anything else reads the pool: a cursor still open holds one of its
	// connections, and a caller asking for a second is refused (store.ErrNestedAcquire). Close is
	// idempotent, so the deferred one above still stands.
	rows.Close()
	if err := rows.Err(); err != nil && !stopped {
		return nil, false, fmt.Errorf("read the outbox's batch: %w", err)
	}
	return pending, stopped, nil
}
