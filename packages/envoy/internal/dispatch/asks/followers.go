// Package asks holds the ask_followers relation shared by every path that writes an
// ask: the API handlers, document ask blocks, and the outbox that routes to followers.
// It sits below api and docs (docs cannot import api) so no insert site can miss it.
package asks

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// Execer is the pgx surface shared by *pgxpool.Pool and pgx.Tx for writes.
type Execer interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// Queryer is the pgx surface shared by *pgxpool.Pool and pgx.Tx for reads.
type Queryer interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// Followed is what one follow wrote: whether the follower row is new, and the session's node
// where this follow made it, held for the event its caller appends after the follow
// (graphstore.Uncaused). A caller that follows before its event exists passes
// graphstore.NoEvent to Follow and hands Uncaused to graphstore.Caused once it holds the event,
// in the same transaction, as a comment's node learns the event that stored it. Only a node
// this follow made at the sentinel moves, so a session another write recorded keeps that
// write's cause, and a caller that appends no event calls nothing and leaves its node at the
// sentinel.
type Followed struct {
	// Added is whether the session did not follow the ask before; following twice changes
	// nothing.
	Added bool
	// Uncaused holds the session's node, made by this follow at the cause it was given. It holds
	// none where the store held the node already, and graphstore.Caused moves nothing then.
	Uncaused graphstore.Uncaused
}

// Follow records that sessionID follows askID, in the caller's transaction, and writes the
// session's node there. An ask a session opened or follows is one of the writes R14 records a
// session from, so a follower that never wrote a comment and was never reached by a delivery is
// still a node of the store, and the ask's followed_by edge to it has a far end every read
// resolves: refs' loadStoreNodes answers a session from graph_nodes alone. Every server path that
// adds a follower reaches this insert: FollowAuthor at each site that writes an ask or a reply to
// one, and PUT /api/v1/asks/{id}/followers/{session_id}.
//
// The node is made by who, the actor of the write, at at, and caused by event. A caller whose
// event is appended after the follow passes graphstore.NoEvent and moves the node onto that event
// once it is appended (graphstore.Caused over Followed.Uncaused). The node is written whether or
// not the follower row is new, because a row written before the store existed carries no node,
// and the first follow of this build that names the session is then the first write that
// records it in the store.
//
// The follower row and the node are one statement: the store's node arm (graphstore.WriteArms)
// spliced into the follower insert, so a follow costs the round trip it cost before the store
// existed. The node arm upserts `do nothing`, which locks no row that is already there, and
// ask_followers carries no key into the store, so which of the two arms Postgres runs first moves
// no lock of the store's order. A session id past the store's address bound is no node the store
// can key, and the follow then writes the follower row alone, as graphstore.SessionRecorded
// writes nothing for such a session.
func Follow(ctx context.Context, tx pgx.Tx, askID, sessionID string, who model.Actor, at time.Time, event int64) (Followed, error) {
	var nodes []graphstore.Node
	if sessionID != "" && graphstore.AddressFits(sessionID) {
		nodes = []graphstore.Node{graphstore.SessionNode(sessionID, who, at, event)}
	}
	arms, params, err := graphstore.WriteArms(3, nodes, nil, nil)
	if err != nil {
		return Followed{}, fmt.Errorf("follow ask %q as %q: %w", askID, sessionID, err)
	}
	written := `0`
	if arms != "" {
		arms, written = ",\n"+arms, `(select count(*) from graph_nodes_written)`
	}
	var added, made int
	if err := tx.QueryRow(ctx, `
		with followed as (
			insert into ask_followers (ask_id, session_id)
			values ($1, $2)
			on conflict do nothing
			returning 1
		)`+arms+`
		select (select count(*) from followed), `+written,
		append([]any{askID, sessionID}, params...)...).Scan(&added, &made); err != nil {
		return Followed{}, fmt.Errorf("follow ask %q as %q: %w", askID, sessionID, err)
	}
	followed := Followed{Added: added == 1}
	if made == 1 {
		followed.Uncaused.Nodes = nodes
	}
	return followed, nil
}

// FollowAuthor follows askID for a session author and does nothing for a human, so every insert
// site can call it with the actor it already has. The session's node is made by the author at at
// and caused by event, as Follow makes it.
func FollowAuthor(ctx context.Context, tx pgx.Tx, askID string, author model.Actor, at time.Time, event int64) (Followed, error) {
	if author.Kind != "session" || author.ID == "" {
		return Followed{}, nil
	}
	return Follow(ctx, tx, askID, author.ID, author, at, event)
}

// Unfollow removes sessionID from askID's followers and reports whether a row went.
func Unfollow(ctx context.Context, db Execer, askID, sessionID string) (bool, error) {
	tag, err := db.Exec(ctx, `
		delete from ask_followers where ask_id = $1 and session_id = $2
	`, askID, sessionID)
	if err != nil {
		return false, fmt.Errorf("unfollow ask %q as %q: %w", askID, sessionID, err)
	}
	return tag.RowsAffected() == 1, nil
}

// Followers lists askID's followers in the order they joined.
func Followers(ctx context.Context, db Queryer, askID string) ([]model.AskFollower, error) {
	rows, err := db.Query(ctx, `
		select session_id, since from ask_followers
		where ask_id = $1
		order by since, session_id
	`, askID)
	if err != nil {
		return nil, fmt.Errorf("load followers of ask %q: %w", askID, err)
	}
	defer rows.Close()
	followers := []model.AskFollower{}
	for rows.Next() {
		var follower model.AskFollower
		if err := rows.Scan(&follower.SessionID, &follower.Since); err != nil {
			return nil, fmt.Errorf("scan follower of ask %q: %w", askID, err)
		}
		followers = append(followers, follower)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate followers of ask %q: %w", askID, err)
	}
	return followers, nil
}
