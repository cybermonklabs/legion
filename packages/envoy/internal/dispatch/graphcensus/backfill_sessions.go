package graphcensus

import (
	"context"
	"fmt"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
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
		nodes, tuples, err := writeSessionBatch(ctx, database, batch)
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
		from (`+recordedSessions(inlineAuthors)+`) recorded
		left join events e on e.id = recorded.event
		left join graph_nodes node on node.kind = $2::text and node.id = recorded.id
		where node.id is null and octet_length(recorded.id) <= $3
		order by recorded.id
	`, graphstore.NoEvent, graphstore.KindSession, graphstore.MaxAddressBytes)
	if err != nil {
		return nil, fmt.Errorf("read the sessions the store holds no node of: %w", err)
	}
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

// writeSessionBatch writes one batch's session nodes in one statement, and answers the nodes it
// wrote and, as the upper bound the report states, the tuples: the two base attributes of each
// node, which the write appends only for a node its insert made.
func writeSessionBatch(ctx context.Context, database *store.Store, batch []sessionRow) (int64, int64, error) {
	nodes := make([]graphstore.Node, 0, len(batch))
	for _, row := range batch {
		nodes = append(nodes, graphstore.SessionNode(row.id, backfillActor, row.at, row.event))
	}
	arms, params, err := graphstore.WriteArms(1, nodes, nil, nil)
	if err != nil {
		return 0, 0, err
	}
	if _, err := database.Pool.Exec(ctx, "with "+arms+"\nselect 1", params...); err != nil {
		return 0, 0, fmt.Errorf("write the nodes of a session batch: %w", err)
	}
	return int64(len(nodes)), 2 * int64(len(nodes)), nil
}
