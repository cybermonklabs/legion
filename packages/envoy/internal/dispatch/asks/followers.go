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
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// Queryer is the pgx surface shared by *pgxpool.Pool and pgx.Tx for reads.
type Queryer interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// Followed is what one follow wrote: whether the follower row is new, and what the follow holds
// for the event its caller appends after it (graphstore.Uncaused): the session's node, and the
// ask node's followed_by to the session where this follow marked the follower row. A caller that
// follows before its event exists passes graphstore.NoEvent and hands Uncaused to
// graphstore.QueueCaused once it has queued the event, in the same transaction, as a comment's
// node learns the event that stored it. Only a node this follow made moves, so a session another
// write recorded keeps that write's cause. A caller that appends no event hands Uncaused to
// graphstore.QueueCaused with graphstore.NoEvent all the same, which appends its facts at the
// sentinel, where the node row stands: dropping it would leave a node row and a marked follower
// row no appended fact describes.
type Followed struct {
	// Added is whether the session did not follow the ask before; following twice changes
	// nothing.
	Added bool
	// Uncaused holds the session's node, which this follow makes at the sentinel where the store
	// does not hold it, and whose base attributes the follow's own statement does not state, and
	// the followed_by the follow stated. It is the zero write for a follow given its cause, whose
	// statements state them, and for one that found the node there and the row marked already.
	Uncaused graphstore.Uncaused
}

// Follow records that sessionID follows askID, in the caller's transaction, at once, and writes
// the session's node there. An ask a session opened or follows is one of the writes R14 records
// a session from, so a follower that never wrote a comment and was never reached by a delivery
// is still a node of the store, and the ask's followed_by edge to it has a far end every read
// resolves: refs' loadStoreNodes answers a session from graph_nodes alone. Every server path that
// adds a follower reaches this insert: PlanFollowAuthor and PlanFollowNewAsk at each site that
// writes an ask or a reply to one, and PUT /api/v1/asks/{id}/followers/{session_id}.
//
// The node is made by who, the actor of the write, at at, and caused by event. A caller whose
// event is appended after the follow passes graphstore.NoEvent and moves the node onto that event
// once it is appended (graphstore.QueueCaused over Followed.Uncaused). The node is written whether
// or not the follower row is new, because a row written before the store existed carries no node,
// and the first follow of this build that names the session is then the first write that records
// it in the store.
//
// The follower row's marker and the followed_by it records are queueFollow's: Follow runs that
// statement as a batch of its own and states the relation from what the statement answers, the
// row it marked and its `since` - held in Followed for a caller whose event comes later, and
// otherwise appended at once, in a batch of its own that runs only when the row was marked. A
// transaction that queues its writes plans the follow in its read phase instead
// (PlanFollowAuthor, or PlanFollowNewAsk for an ask it creates), whose plan decides the same
// from the row it reads, or for a new ask from what its caller knows.
func Follow(ctx context.Context, tx pgx.Tx, askID, sessionID string, who model.Actor, at time.Time, event int64) (Followed, error) {
	var followed Followed
	var result followResult
	err := store.RunWrite(ctx, tx, func(b *store.WriteBatch) error {
		var err error
		followed.Uncaused, err = queueFollow(b, askID, sessionID, who, at, nil, event, func(got followResult) error {
			result = got
			return nil
		})
		return err
	})
	if err != nil {
		return Followed{}, err
	}
	followed.Added = result.added
	if !result.marked {
		return followed, nil
	}
	relation := followChange(askID, sessionID, true, who, result.since, event)
	if event == graphstore.NoEvent {
		followed.Uncaused.Tuples = append(followed.Uncaused.Tuples, graphstore.UncausedTuple{Tuple: relation})
		return followed, nil
	}
	if err := store.RunWrite(ctx, tx, func(b *store.WriteBatch) error {
		graphstore.QueueWrite(b, nil, []graphstore.Tuple{relation})
		graphstore.QueueHeads(b, graphstore.HeadsOf(nil, []graphstore.Tuple{relation}))
		return nil
	}); err != nil {
		return Followed{}, fmt.Errorf("state that %q follows ask %q: %w", sessionID, askID, err)
	}
	return followed, nil
}

// followResult is what the follow's statement answers once it has run: whether the follower row
// is new, whether the statement marked it, and the row's since where it did.
type followResult struct {
	added, marked bool
	since         time.Time
}

// queueFollow queues the follow's statement on b and answers the node it holds for the follow's
// cause; read is handed what the statement answers (followResult) once it has run, and its error
// stops the batch there. since is the since the statement writes on a new follower row, where
// its caller dates the row itself (PlanFollowNewAsk), and nil where the column's default, the
// transaction's now(), gives it; a row that stands keeps its own either way.
//
// The follower row and the node are one statement: the store's node arm (graphstore.WriteArms)
// spliced into the follower insert, so a follow costs the statement it cost before the store
// existed. The node arm upserts `do nothing`, which locks no row that is already there, and
// which of the two arms Postgres runs first moves no lock of the store's order. A session id past
// the store's address bound is no node the store can key, and the follow then writes the follower
// row alone, as graphstore.QueueSessionRecorded writes nothing for such a session.
//
// The same statement stamps the follower row's marker (ask_followers.node_kind and node_address),
// which says the ask node's followed_by to the session is the record of this row: on a new row,
// and on a row a process of the release before wrote with none, which this follow makes the
// store's. graph_edges' tuple arm answers a marked row from that relation, and its old arm answers
// an unmarked one (20261125163553). A row is marked only where the ask's node is there to name -
// an ask a process of the release before created has none until the boot step's backfill writes it
// - and the session is a node the store can key. A follow that marks the row states followed_by,
// dated by the row's own `since`, as the old arm dates the edge; one that finds the row marked
// already states nothing, so a session replying on an ask it follows appends no relation at all.
// That relation is stated after the row: its time is the row's, which the insert gives it.
// That followed_by changes the ask's node it goes out of (graphstore.Tuple's Changes), as a
// pointer a request appends does, so a follow made for a run of the command bar names the ask
// as changed by that run (store.WithRun); the following session's node is never marked.
//
// A follow whose cause comes later states no base attribute in that statement and holds the
// node for the caller to cause under its event (graphstore.WriteArmsUncaused): no tuple may be
// updated to learn its cause, and graphstore.QueueCaused moves the node row onto that event, so
// stating them here would leave the row naming one event and its own base tuples naming another.
func queueFollow(b *store.WriteBatch, askID, sessionID string, who model.Actor, at time.Time, since *time.Time, event int64, read func(followResult) error) (graphstore.Uncaused, error) {
	var nodes []graphstore.Node
	fits := sessionID != "" && graphstore.AddressFits(sessionID)
	if fits {
		nodes = []graphstore.Node{graphstore.SessionNode(sessionID, who, at, event)}
	}
	var (
		arms   string
		params []any
		held   graphstore.Uncaused
		err    error
	)
	if event == graphstore.NoEvent {
		arms, params, held, err = graphstore.WriteArmsUncaused(6, nodes, nil, nil)
	} else {
		arms, params, err = graphstore.WriteArms(6, nodes, nil, nil)
	}
	if err != nil {
		return graphstore.Uncaused{}, fmt.Errorf("follow ask %q as %q: %w", askID, sessionID, err)
	}
	if arms != "" {
		arms = ",\n" + arms
	}
	b.Queue(fmt.Sprintf("follow ask %q as %q", askID, sessionID), `
		with followed as (
			insert into ask_followers (ask_id, session_id, node_kind, node_address, since)
			values ($1, $2,
				(select node.kind from graph_nodes node where $4 and node.kind = $3 and node.id = $1::uuid::text),
				(select node.id from graph_nodes node where $4 and node.kind = $3 and node.id = $1::uuid::text),
				coalesce($5::timestamptz, now()))
			on conflict (ask_id, session_id) do update -- kept for one release: follower-row-mark
				set node_kind = excluded.node_kind, node_address = excluded.node_address
				where ask_followers.node_kind is null and excluded.node_kind is not null
			returning xmax = 0 as added, node_kind is not null as marked, since
		)`+arms+`
		select coalesce((select added from followed), false), coalesce((select marked from followed), false),
			(select since from followed)`,
		append([]any{askID, sessionID, graphstore.KindAsk, fits, since}, params...)...).QueryRow(func(row pgx.Row) error {
		var result followResult
		var stored *time.Time
		if err := row.Scan(&result.added, &result.marked, &stored); err != nil {
			return err
		}
		if stored != nil {
			result.since = *stored
		} else {
			result.marked = false
		}
		return read(result)
	})
	return held, nil
}

// FollowedBy is the ask node's followed_by to a session, asserted or retracted, made by who at at
// under event: the one build of that tuple, which every follow, unfollow and catch-up of a follow
// states, and the boot step's backfill of a follower row written before this release (R3).
func FollowedBy(askID, sessionID string, asserted bool, who model.Actor, at time.Time, event int64) graphstore.Tuple {
	return graphstore.Tuple{
		FromKind: graphstore.KindAsk, FromID: askID, Kind: graphstore.TupleFollowedBy,
		ToKind: graphstore.KindSession, ToID: sessionID,
		At: at, Who: who, SourceEvent: event, Asserted: asserted,
	}
}

// followChange is FollowedBy marked as changing the ask's node (graphstore.Tuple's Changes): the
// relation a follow or an unfollow states, which a write made for a run names the ask's node
// changed by. CatchUpFollower states what another build's follow or unfollow did, which no run
// made, through FollowedBy alone.
func followChange(askID, sessionID string, asserted bool, who model.Actor, at time.Time, event int64) graphstore.Tuple {
	relation := FollowedBy(askID, sessionID, asserted, who, at, event)
	relation.Changes = true
	return relation
}

// PlannedFollow is the follow of an ask by a session author as PlanFollowAuthor or
// PlanFollowNewAsk planned it in the read phase of a write that queues its statements, decided
// before anything is written, which Queue queues in that write's write phase. The zero
// PlannedFollow, a human author's, queues nothing.
type PlannedFollow struct {
	askID, sessionID string
	who              model.Actor
	at               time.Time
	// Added is whether the session did not follow the ask before.
	Added bool
	// marks says the follow's statement marks the follower row, and since is the row's since
	// then: the row's own for a row the release before wrote with no marker, the transaction's
	// now() for a new row of an ask that stood before, which its column's default gives it, and
	// for an ask this transaction creates the time its caller passed, which the statement writes.
	marks bool
	since time.Time
	// dated says the follow's statement writes since on the new row itself rather than leave it
	// to the column's default: the follow of an ask this transaction creates (PlanFollowNewAsk).
	dated bool
}

// PlanFollowNewAsk plans the follow of askID, an ask this transaction creates, by a session
// author, and plans nothing for a human. It reads nothing: an ask this transaction creates has no
// follower row yet, so the follow adds one, and the row is marked exactly where the session is a
// node the store can key, since every writer of an ask queues the ask's node before its row
// (graphstore.AskFacts, written first) and the caller queues the follow after both, so the
// follow's statement finds that node. The one value a read would supply is the new row's since,
// which dates the followed_by the follow states: the follow's statement writes it as at, the time
// the caller passes - each caller's ask's created_at, the transaction's now(), which is what the
// column's default would give it - so the relation's date is known here. Queue holds the
// statement to this plan as it holds PlanFollowAuthor's: a row it did not mark, or marked at
// another since, stops the write phase.
func PlanFollowNewAsk(askID string, author model.Actor, at time.Time) PlannedFollow {
	if author.Kind != "session" || author.ID == "" {
		return PlannedFollow{}
	}
	return PlannedFollow{
		askID: askID, sessionID: author.ID, who: author, at: at, Added: true,
		marks: graphstore.AddressFits(author.ID), since: at, dated: true,
	}
}

// PlanFollowAuthor plans the follow of askID, an ask that stood before this transaction, by a
// session author, as Follow writes it, for a transaction that queues its writes, and plans nothing
// for a human. It reads, through q, under the owner lock the caller holds over the ask
// (store.LockOwner), under which no other transaction writes its followers, whether the ask's node
// stands, the session's follower row - whether it is there, whether it is marked, and its since -
// and the transaction's now(). It decides from them what queueFollow's statement will do to the
// row: a new row, or one the release before wrote with no marker, is marked where the ask's node
// is there to name and the session is a node the store can key, and the follow then states
// followed_by dated by the row's since; a row marked already is left as it stands, and the follow
// states nothing of it. Queue holds the statement to the plan: a row it did not mark as the plan
// read, or marked at another since, stops the write phase, since the fact the plan states would
// not be that row's. An ask this transaction creates is planned by PlanFollowNewAsk, with no read.
func PlanFollowAuthor(ctx context.Context, q RowQueryer, askID string, author model.Actor, at time.Time) (PlannedFollow, error) {
	if author.Kind != "session" || author.ID == "" {
		return PlannedFollow{}, nil
	}
	plan := PlannedFollow{askID: askID, sessionID: author.ID, who: author, at: at}
	var named, held, marked bool
	var since *time.Time
	var now time.Time
	if err := q.QueryRow(ctx, `
		select exists (select 1 from graph_nodes node where node.kind = $3 and node.id = $1::uuid::text),
		       f.ask_id is not null, coalesce(f.node_kind is not null, false), f.since, now()
		from (select 1) one
		left join ask_followers f on f.ask_id = $1::uuid and f.session_id = $2
	`, askID, author.ID, graphstore.KindAsk).Scan(&named, &held, &marked, &since, &now); err != nil {
		return PlannedFollow{}, fmt.Errorf("read the follow of ask %q by %q: %w", askID, author.ID, err)
	}
	plan.Added = !held
	if !graphstore.AddressFits(author.ID) || !named {
		return plan, nil
	}
	switch {
	case !held:
		plan.marks, plan.since = true, now
	// kept for one release: follower-row-mark
	case !marked && since != nil:
		plan.marks, plan.since = true, *since
	}
	return plan, nil
}

// Queue queues the planned follow on b where its write makes it: the follower row, marked as the
// plan decided, with the session's node in the same statement (queueFollow), and the ask node's
// followed_by to the session where the statement marks the row, dated by the row's since - held
// in the answer for the caller's graphstore.QueueCaused where event is graphstore.NoEvent, and
// otherwise queued right after the statement under event, graphstore.LatestEvent for an event the
// write phase appended just before it. It answers what it holds for that cause.
func (p PlannedFollow) Queue(b *store.WriteBatch, event int64) (graphstore.Uncaused, error) {
	if p.sessionID == "" {
		return graphstore.Uncaused{}, nil
	}
	var since *time.Time
	if p.dated {
		since = &p.since
	}
	held, err := queueFollow(b, p.askID, p.sessionID, p.who, p.at, since, event, func(got followResult) error {
		if got.marked != p.marks || got.marked && !got.since.Equal(p.since) {
			return fmt.Errorf("follow ask %q as %q: the statement marked the follower row %t at %s, the plan read %t at %s",
				p.askID, p.sessionID, got.marked, got.since, p.marks, p.since)
		}
		return nil
	})
	if err != nil || !p.marks {
		return held, err
	}
	relation := followChange(p.askID, p.sessionID, true, p.who, p.since, event)
	if event == graphstore.NoEvent {
		held.Tuples = append(held.Tuples, graphstore.UncausedTuple{Tuple: relation})
		return held, nil
	}
	graphstore.QueueWrite(b, nil, []graphstore.Tuple{relation})
	graphstore.QueueHeads(b, graphstore.HeadsOf(nil, []graphstore.Tuple{relation}))
	return held, nil
}

// Unfollowed is what one unfollow wrote: whether a follower row went, and the ask node's
// retraction of followed_by to the session, held for the event its caller appends after it
// (graphstore.QueueCaused), where the row that went was marked.
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
		retraction := followChange(askID, sessionID, false, who, at, graphstore.NoEvent)
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
//
// kept for one release: catch-up
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
		relation := FollowedBy(askID, sessionID, true, who, since, event)
		if err := store.RunWrite(ctx, tx, func(b *store.WriteBatch) error {
			graphstore.QueueWrite(b, nodes, []graphstore.Tuple{relation})
			graphstore.QueueHeads(b, graphstore.HeadsOf(nil, []graphstore.Tuple{relation}))
			return nil
		}); err != nil {
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
	retraction := FollowedBy(askID, sessionID, false, who, at, event)
	if err := store.RunWrite(ctx, tx, func(b *store.WriteBatch) error {
		graphstore.QueueWrite(b, nil, []graphstore.Tuple{retraction})
		graphstore.QueueHeads(b, graphstore.HeadsOf(nil, []graphstore.Tuple{retraction}))
		return nil
	}); err != nil {
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
