// Package asks holds what every path that writes an ask shares: the ask_followers relation, which
// the API handlers, document ask blocks and the outbox that routes to followers all reach, and the
// one writer of an ask's answer state (Settle). It sits below api and docs (docs cannot import
// api) so no insert site can miss either.
package asks

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// Queryer is the pgx surface shared by *pgxpool.Pool and pgx.Tx for reads.
type Queryer interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// Followed is what one follow wrote: whether the follower row is new, and what the follow holds
// for the event its caller appends after it (graphstore.Uncaused): the session's node where this
// follow made it, with that node's two base attributes, and the ask node's followed_by to the
// session where this follow marked the follower row. A caller that follows before its event exists
// passes graphstore.NoEvent to Follow and hands Uncaused to graphstore.Caused once it holds the
// event, in the same transaction, as a comment's node learns the event that stored it. Only a node
// this follow made at the sentinel moves, so a session another write recorded keeps that write's
// cause. A caller that appends no event hands Uncaused to graphstore.Caused with
// graphstore.NoEvent all the same, which appends them at the sentinel, where the node row stands:
// dropping it would leave a node row and a marked follower row no appended fact describes.
type Followed struct {
	// Added is whether the session did not follow the ask before; following twice changes
	// nothing.
	Added bool
	// Uncaused holds the session's node, made by this follow at the cause it was given, the
	// base attributes the follow's own statement did not state, and the followed_by the follow
	// stated. It holds none where the store held the node already and the row was marked
	// already, and graphstore.Caused moves nothing then.
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
// which of the two arms Postgres runs first moves no lock of the store's order. A session id past
// the store's address bound is no node the store can key, and the follow then writes the follower
// row alone, as graphstore.SessionRecorded writes nothing for such a session.
//
// The same statement stamps the follower row's marker (ask_followers.node_kind and node_address),
// which says the ask node's followed_by to the session is the record of this row: on a new row,
// and on a row a process of the release before wrote with none, which this follow makes the
// store's. graph_edges' tuple arm answers a marked row from that relation, and its old arm answers
// an unmarked one (20261116094101). A row is marked only where the ask's node is there to name -
// an ask a process of the release before created has none until the boot step's backfill writes it
// - and the session is a node the store can key. A follow that marks the row states followed_by,
// dated by the row's own `since`, as the old arm dates the edge; one that finds the row marked
// already states nothing, so a session replying on an ask it follows appends no relation at all.
// That relation is stated after the row: its time is the row's, which the insert gives it. A
// follow given its cause appends it at once, in a statement of its own that runs only when the row
// was marked; one whose cause comes later holds it in Uncaused with the node's base attributes.
// That followed_by changes the ask's node it goes out of (graphstore.Tuple's Changes), as a
// pointer a request appends does, so a follow made for a run of the command bar names the ask
// as changed by that run (graphstore.WithRun); the following session's node is never marked.
//
// A follow whose cause comes later states no base attribute in that statement and answers the
// node's two for the caller to append under its event (graphstore.WriteArmsUncaused,
// graphstore.BaseAttributeFacts): no tuple may be updated to learn its cause, and
// graphstore.Caused moves the node row onto that event, so stating them here would leave the
// row naming one event and its own base tuples naming another.
func Follow(ctx context.Context, tx pgx.Tx, askID, sessionID string, who model.Actor, at time.Time, event int64) (Followed, error) {
	var nodes []graphstore.Node
	fits := sessionID != "" && graphstore.AddressFits(sessionID)
	if fits {
		nodes = []graphstore.Node{graphstore.SessionNode(sessionID, who, at, event)}
	}
	writeArms := graphstore.WriteArms
	if event == graphstore.NoEvent {
		writeArms = graphstore.WriteArmsUncaused
	}
	arms, params, err := writeArms(5, nodes, nil, nil)
	if err != nil {
		return Followed{}, fmt.Errorf("follow ask %q as %q: %w", askID, sessionID, err)
	}
	written := `0`
	if arms != "" {
		arms, written = ",\n"+arms, `(select count(*) from graph_nodes_written)`
	}
	var added, marked bool
	var since *time.Time
	var made int
	if err := tx.QueryRow(ctx, `
		with followed as (
			insert into ask_followers (ask_id, session_id, node_kind, node_address)
			values ($1, $2,
				(select node.kind from graph_nodes node where $4 and node.kind = $3 and node.id = $1::uuid::text),
				(select node.id from graph_nodes node where $4 and node.kind = $3 and node.id = $1::uuid::text))
			on conflict (ask_id, session_id) do update
				set node_kind = excluded.node_kind, node_address = excluded.node_address
				where ask_followers.node_kind is null and excluded.node_kind is not null
			returning xmax = 0 as added, node_kind is not null as marked, since
		)`+arms+`
		select coalesce((select added from followed), false), coalesce((select marked from followed), false),
			(select since from followed), `+written,
		append([]any{askID, sessionID, graphstore.KindAsk, fits}, params...)...).Scan(&added, &marked, &since, &made); err != nil {
		return Followed{}, fmt.Errorf("follow ask %q as %q: %w", askID, sessionID, err)
	}
	followed := Followed{Added: added}
	if made == 1 {
		followed.Uncaused.Nodes = nodes
		if event == graphstore.NoEvent {
			followed.Uncaused.Tuples = graphstore.BaseAttributeFacts(nodes)
		}
	}
	if !marked || since == nil {
		return followed, nil
	}
	relation := followedBy(askID, sessionID, true, who, *since, event)
	relation.Changes = true
	if event == graphstore.NoEvent {
		followed.Uncaused.Tuples = append(followed.Uncaused.Tuples, graphstore.UncausedTuple{Tuple: relation})
		return followed, nil
	}
	if err := graphstore.Write(ctx, tx, nil, []graphstore.Tuple{relation}); err != nil {
		return Followed{}, fmt.Errorf("state that %q follows ask %q: %w", sessionID, askID, err)
	}
	return followed, nil
}

// followedBy is the ask node's followed_by to a session, asserted or retracted.
func followedBy(askID, sessionID string, asserted bool, who model.Actor, at time.Time, event int64) graphstore.Tuple {
	return graphstore.Tuple{
		FromKind: graphstore.KindAsk, FromID: askID, Kind: graphstore.TupleFollowedBy,
		ToKind: graphstore.KindSession, ToID: sessionID,
		At: at, Who: who, SourceEvent: event, Asserted: asserted,
	}
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

// Unfollowed is what one unfollow wrote: whether a follower row went, and the ask node's
// retraction of followed_by to the session, held for the event its caller appends after it
// (graphstore.Caused), where the row that went was marked.
type Unfollowed struct {
	Removed  bool
	Uncaused graphstore.Uncaused
}

// Unfollow removes sessionID from askID's followers in the caller's transaction and reports
// whether a row went. Where that row was marked, the ask node's followed_by to the session is its
// record, and the unfollow states its end: a retraction made by who at at, held in Unfollowed for
// the event the caller appends after the delete, as a follow holds its relation, and marked as
// changing the ask's node (graphstore.Tuple's Changes), as a follow's is. An unmarked row is
// answered by graph_edges' old arm, which its delete takes the edge out of with nothing more.
func Unfollow(ctx context.Context, tx pgx.Tx, askID, sessionID string, who model.Actor, at time.Time) (Unfollowed, error) {
	var marked bool
	err := tx.QueryRow(ctx, `
		delete from ask_followers where ask_id = $1 and session_id = $2
		returning node_kind is not null
	`, askID, sessionID).Scan(&marked)
	if errors.Is(err, pgx.ErrNoRows) {
		return Unfollowed{}, nil
	}
	if err != nil {
		return Unfollowed{}, fmt.Errorf("unfollow ask %q as %q: %w", askID, sessionID, err)
	}
	unfollowed := Unfollowed{Removed: true}
	if marked {
		retraction := followedBy(askID, sessionID, false, who, at, graphstore.NoEvent)
		retraction.Changes = true
		unfollowed.Uncaused.Tuples = []graphstore.UncausedTuple{{Tuple: retraction}}
	}
	return unfollowed, nil
}

// CatchUpFollower states on the ask node a follow or an unfollow a process of another build
// recorded, under the event it appended (event, made by who at at), as the row stands now: the
// boot step's catch-up reads that build's ask.follower_added and ask.follower_removed, which
// changed the row and stated nothing (docs' decision step). A follow the row still holds unmarked
// is marked and its followed_by asserted, dated by the row's since; an unfollow of a row that is
// gone retracts the followed_by the node still asserts. It writes no follower row and deletes
// none, so an event caught up after a later one never brings back a row the later one took away.
// It reports whether it stated anything.
func CatchUpFollower(ctx context.Context, tx pgx.Tx, askID, sessionID string, added bool, who model.Actor, at time.Time, event int64) (bool, error) {
	if !graphstore.AddressFits(sessionID) {
		return false, nil
	}
	if added {
		var since time.Time
		err := tx.QueryRow(ctx, `
			update ask_followers f set node_kind = $3, node_address = $1::uuid::text
			where f.ask_id = $1 and f.session_id = $2 and f.node_kind is null
			  and exists (select 1 from graph_nodes node where node.kind = $3 and node.id = $1::uuid::text)
			returning f.since
		`, askID, sessionID, graphstore.KindAsk).Scan(&since)
		if errors.Is(err, pgx.ErrNoRows) {
			return false, nil
		}
		if err != nil {
			return false, fmt.Errorf("mark the follow of ask %q by %q: %w", askID, sessionID, err)
		}
		nodes := []graphstore.Node{graphstore.SessionNode(sessionID, who, since, event)}
		if err := graphstore.Write(ctx, tx, nodes, []graphstore.Tuple{followedBy(askID, sessionID, true, who, since, event)}); err != nil {
			return false, fmt.Errorf("state that %q follows ask %q: %w", sessionID, askID, err)
		}
		return true, nil
	}
	var gone, asserted bool
	if err := tx.QueryRow(ctx, `
		select not exists (select 1 from ask_followers where ask_id = $1 and session_id = $2),
		       coalesce((select t.asserted from graph_tuples t
		                 where t.from_kind = $3 and t.from_id = $1::uuid::text and t.kind = $4
		                   and t.to_kind = $5 and t.to_id = $2
		                 order by t.seq desc limit 1), false)
	`, askID, sessionID, graphstore.KindAsk, graphstore.TupleFollowedBy, graphstore.KindSession).Scan(&gone, &asserted); err != nil {
		return false, fmt.Errorf("read the unfollow of ask %q by %q: %w", askID, sessionID, err)
	}
	if !gone || !asserted {
		return false, nil
	}
	if err := graphstore.Write(ctx, tx, nil, []graphstore.Tuple{followedBy(askID, sessionID, false, who, at, event)}); err != nil {
		return false, fmt.Errorf("state that %q no longer follows ask %q: %w", sessionID, askID, err)
	}
	return true, nil
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
