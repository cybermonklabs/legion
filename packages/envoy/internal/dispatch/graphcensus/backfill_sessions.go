package graphcensus

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// sessionRow is one session the store census holds to a node (recordedSessions) whose node the
// store does not hold: its id, and the cause and the time its node is made at.
type sessionRow struct {
	id    string
	event int64
	at    time.Time
}

// backfillSessions writes the node of every session the store census holds to one and that the
// store holds no node of: the sessions that asked, followed or were reached before the store
// existed, or wrote a comment the comment pass could not give a node. Its input is
// recordedSessions, the census's own session row side, so the sessions this gives a node are
// exactly the sessions the census holds to one, as backfillRoles's input is the census's role row
// side.
//
// A session has no projection row, so there is no row to name the node on and no fence to take.
// It walks only the sessions that carry no node, and a node another write made meanwhile is
// written `do nothing` (graphstore.WriteArms), so a second run, and a run beside a live server,
// writes nothing for a session the store already holds. The writer is the run (backfillActor);
// the cause is the first event that named the session - an ask.follower_added or a
// comment.delivery receipt - or the sentinel for a session only a row names, and the time is that
// event's, or the earliest row's that names it.
//
// No arm of graph_edges answers a session, so its Complete switches nothing off: it reads whether
// every session the census names holds its node.
func backfillSessions(ctx context.Context, database *store.Store) (KindBackfill, error) {
	done := KindBackfill{Kind: graphstore.KindSession}
	missing, err := readSessionRows(ctx, database)
	if err != nil {
		return done, err
	}
	for start := 0; start < len(missing); start += backfillBatch {
		batch := missing[start:min(start+backfillBatch, len(missing))]
		nodes, tuples, err := writeSessionBatch(ctx, database.Pool, batch)
		if err != nil {
			return done, err
		}
		done.Rows += int64(len(batch))
		done.Nodes += nodes
		done.Tuples += tuples
	}
	left, err := readSessionRows(ctx, database)
	if err != nil {
		return done, err
	}
	done.Remaining = int64(len(left))
	done.Complete = done.Remaining == 0
	return done, nil
}

// readSessionRows reads every session recordedSessions names whose node the store does not hold
// and whose id fits the store's bound, in id order. A session past the bound is no node of the
// store, so it is neither written here nor counted as remaining (graphstore.QueueSessionRecorded).
func readSessionRows(ctx context.Context, database *store.Store) ([]sessionRow, error) {
	rows, err := database.Pool.Query(ctx, `
		select recorded.id, coalesce(recorded.event, $1), coalesce(e.created_at, recorded.at)
		from (`+recordedSessions(inlineAuthors, MessagesNameTheirNode)+`) recorded
		left join events e on e.id = recorded.event
		left join graph_nodes node on node.kind = $2::text and node.id = recorded.id
		where node.id is null and octet_length(recorded.id) <= $3
		order by recorded.id
	`, graphstore.NoEvent, graphstore.KindSession, graphstore.MaxAddressBytes)
	if err != nil {
		return nil, fmt.Errorf("read the sessions the store holds no node of: %w", err)
	}
	return scanSessionRows(rows)
}

// reachedSessions reads, of the attempt rows of table keys names (the attempt pass's keys), each
// session one the listener took reached - recordedSessions's arm over the table - whose node the
// store does not hold and whose id fits its bound, with the cause backfillSessions gives such a
// session: the first receipt that names it (table.receipt), which recordedSessions reads too, or
// the sentinel where none does, and the time of that receipt or of the earliest of those rows. The
// block-index tick's catch-up of an attempt row a build before this one left (CatchUpComments,
// CatchUpOwedRows) writes those nodes before the attempts, as the backfill runs its session pass
// before its attempt pass: a row an older build settled after the backfill names a session no
// pass of it gave a node.
func (table attemptTable) reachedSessions(ctx context.Context, q graphstore.Queryer, keys [][]string) ([]sessionRow, error) {
	named, args := table.named(keys)
	rows, err := q.Query(ctx, `
		select d.session_id, coalesce(min(receipt.id), `+noEvent+`), min(coalesce(receipt.created_at, d.created_at))
		from `+table.table+` d`+table.receipt+`
		where `+named+`
		  and coalesce(d.session_id, '') <> '' and d.state in `+model.DeliveryTakenStatesSQL+`
		  and octet_length(d.session_id) <= `+addressBound+`
		  and not exists (select 1 from graph_nodes n where n.kind = '`+graphstore.KindSession+`' and n.id = d.session_id)
		group by d.session_id
		order by d.session_id
	`, args...)
	if err != nil {
		return nil, fmt.Errorf("read the sessions a batch of %s reached that hold no node: %w", table.table, err)
	}
	return scanSessionRows(rows)
}

// eventSessions reads each session an event in (after, through] names - recordedSessions's arm over
// the log: a follower an ask.follower_added names, a session an ask.opened names as the ask's
// author, and a session a comment.delivery receipt says the listener took an attempt to - whose
// node the store does not hold and whose id fits its bound, with the first of those events that
// names it, the cause backfillSessions gives it, and that event's time (CatchUpEvents).
func eventSessions(ctx context.Context, q graphstore.Queryer, after, through int64) ([]sessionRow, error) {
	rows, err := q.Query(ctx, `
		select named.id, min(named.event), min(named.at)
		from (
			select case when e.type in ('ask.follower_added', 'comment.delivery') then e.payload->>'session_id'
			            when e.payload->'author'->>'kind' = 'session' then e.payload->'author'->>'id' end as id,
			       e.id as event, e.created_at as at
			from events e
			where e.id > $1 and e.id <= $2
			  and (e.type in ('ask.follower_added', 'ask.opened')
			       or (e.type = 'comment.delivery' and e.payload->>'state' in `+model.DeliveryTakenStatesSQL+`))
		) named
		where coalesce(named.id, '') <> '' and octet_length(named.id) <= `+addressBound+`
		  and not exists (select 1 from graph_nodes n where n.kind = '`+graphstore.KindSession+`' and n.id = named.id)
		group by named.id
		order by named.id
	`, after, through)
	if err != nil {
		return nil, fmt.Errorf("read the sessions the events after %d name that hold no node: %w", after, err)
	}
	return scanSessionRows(rows)
}

// fallbackSessionsOwed is the sessions a published fallback copy reached - recordedSessions's arm
// over comment_fallback_copies - whose id fits the store's bound and that the store holds no node
// of, each with the earliest copy that reached it, as (id, at) over s.
var fallbackSessionsOwed = `
	from (
		select substring(f.destination from ` + agentTopicFrom + `) as id, min(f.created_at) as at
		from comment_fallback_copies f
		where f.published and f.destination like '` + contracts.AgentTopicPrefix + `%'
		group by 1
	) s
	where s.id <> '' and octet_length(s.id) <= ` + addressBound + `
	  and not exists (select 1 from graph_nodes n where n.kind = '` + graphstore.KindSession + `' and n.id = s.id)`

// fallbackSessionsPass is the session pass's arm over the sessions a published fallback copy
// reached, for the block-index tick (rollPasses): a build before this one's outbox publishes a copy
// with no event and states nothing, so no event the tick reads names such a session. Each takes
// its node as backfillSessions gives one only a row names: at the sentinel, at the earliest copy.
// kept for one release: inbox-roll-catch-up
func fallbackSessionsPass() inboxPass {
	return inboxPass{
		kind:      "fallback_sessions",
		batch:     `select s.id ` + fallbackSessionsOwed + ` and s.id > $1 order by s.id limit $2`,
		start:     []string{""},
		write:     writeFallbackSessions,
		remaining: fallbackSessionsOwed,
	}
}

// writeFallbackSessions writes the node of each session of keys a published fallback copy reached
// that still holds none (writeSessionBatch).
func writeFallbackSessions(ctx context.Context, tx pgx.Tx, keys [][]string) (inboxWrote, error) {
	rows, err := tx.Query(ctx, `
		select s.id, `+noEvent+`::bigint, s.at `+fallbackSessionsOwed+` and s.id = any($1::text[])
		order by s.id
	`, column(keys, 0))
	if err != nil {
		return inboxWrote{}, fmt.Errorf("read the sessions a fallback copy reached that hold no node: %w", err)
	}
	missing, err := scanSessionRows(rows)
	if err != nil || len(missing) == 0 {
		return inboxWrote{}, err
	}
	nodes, tuples, err := writeSessionBatch(ctx, tx, missing)
	return inboxWrote{rows: int64(len(missing)), nodes: nodes, tuples: tuples}, err
}

// scanSessionRows reads every row of rows, a read of sessionRow's columns in its order, and closes
// it.
func scanSessionRows(rows pgx.Rows) ([]sessionRow, error) {
	defer rows.Close()
	var missing []sessionRow
	for rows.Next() {
		var row sessionRow
		if err := rows.Scan(&row.id, &row.event, &row.at); err != nil {
			return nil, fmt.Errorf("read a session the store holds no node of: %w", err)
		}
		missing = append(missing, row)
	}
	return missing, rows.Err()
}

// writeSessionBatch writes one batch's session nodes in one statement through q, the pool for the
// backfill's session pass and the catch-up's transaction for the block-index tick
// (writeAttemptsAndSessions), and answers the nodes it wrote and, as the upper bound the report
// states, the tuples: the two base attributes of each node, which the write appends only for a node
// its insert made.
func writeSessionBatch(ctx context.Context, q refs.Execer, batch []sessionRow) (int64, int64, error) {
	nodes := make([]graphstore.Node, 0, len(batch))
	for _, row := range batch {
		nodes = append(nodes, graphstore.SessionNode(row.id, backfillActor, row.at, row.event))
	}
	arms, params, err := graphstore.WriteArms(1, nodes, nil, nil)
	if err != nil {
		return 0, 0, err
	}
	if _, err := q.Exec(ctx, "with "+arms+"\nselect 1", params...); err != nil {
		return 0, 0, fmt.Errorf("write the nodes of a session batch: %w", err)
	}
	return int64(len(nodes)), 2 * int64(len(nodes)), nil
}
