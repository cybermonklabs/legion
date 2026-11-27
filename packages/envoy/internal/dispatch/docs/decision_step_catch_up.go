package docs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/asks"
	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// caughtUpAsk is the ask an event of the catch-up's read names, aliased e: a follower event names
// it as ask_id, every other as the ask's own id.
// kept for one release: catch-up
const caughtUpAsk = `case when e.type like 'ask.follower_%' then e.payload->>'ask_id' else e.payload->>'id' end`

// caughtUpTypes is the type list of every read of the catch-up, written out so the planner can
// match it against a partial index's own: an answer, a resolution (a decline is one), an opening
// (a reopen, or settlement's restoration), a follower added or removed.
// kept for one release: catch-up
const caughtUpTypes = `('ask.answered', 'ask.resolved', 'ask.opened', 'ask.follower_added', 'ask.follower_removed')`

// caughtUpColumns is the event's own columns each row of a catch-up read starts with, in the order
// scanCaughtUpEvent takes them; the ask it names and its owner follow.
// kept for one release: catch-up
const caughtUpColumns = `e.id, e.type, e.payload, e.actor, e.created_at`

// caughtUpRead is the catch-up's one read of a pass: every event of the window above $1 of
// caughtUpTypes that no tuple of its ask's node states, since none carries the event's id as its
// source_event, and whose id is none of $2, which an earlier pass read already. Each row is the
// event, the ask it names and the event's owner, with the owner's last_seq as the read's snapshot
// holds it: every event of that owner the read could see stands at or below it, which is where
// catchUpAsk reads on from once it holds the ask (caughtUpSince). The rows come in the order of
// the asks and, within an ask, of the events' ids. It reads events through their primary key from
// $1 on, so a pass reads what was appended since the passes before it and not the whole window
// again, each event's tuples through an index keyed on its ask's node, and each owner's row by its
// key (TestTheCatchUpReadsItsWindowThroughIndexes). The tuples are looked for by a scalar
// subquery, `(select … limit 1) is null`, rather than by NOT EXISTS, which Postgres plans as an
// anti-join over every tuple of every ask, read once and kept beside the events (copied_asks.go's
// owedCopiesQuery meets the same with a hashed subplan); the subquery runs once for each event,
// each run one probe of the index.
// kept for one release: catch-up
const caughtUpRead = `
	select ` + caughtUpColumns + `, coalesce(` + caughtUpAsk + `, '') as ask,
	       coalesce(e.issue_key, ''), coalesce(e.artifact_id::text, ''),
	       coalesce((select i.last_seq from issues i where i.key = e.issue_key),
	                (select d.last_seq from artifacts d where d.id = e.artifact_id), 0)
	from events e
	where e.id > $1
	  and e.type in ` + caughtUpTypes + `
	  and e.id <> all($2::bigint[])
	  and (select 1 from graph_tuples t
	       where t.from_kind = 'ask' and t.from_id = ` + caughtUpAsk + ` and t.source_event = e.id
	       limit 1) is null
	order by ask, e.id`

// caughtUpSince is the read catchUpAsk makes once it holds an ask's owner row and the ask's row:
// the events of caughtUpTypes of the ask $3 that its owner, keyed by owner ($1), appended after
// the sequence $2 the pass's read saw it at, which no tuple of the ask's node states, in the order
// of their ids. The owner's row is held, so they are every such event a write committed between
// the pass's read and the lock, and no later one can commit while the transaction stands. It reads
// the owner's events through the owner's unique (key, seq) index from $2 on, so it reads only
// what the owner appended in that gap, follower events among them, which name their ask as
// ask_id where events_ask_payload_id keys only on id.
// kept for one release: catch-up
func caughtUpSince(owner string) string {
	return `
	select ` + caughtUpColumns + `, $3::text, coalesce(e.issue_key, ''), coalesce(e.artifact_id::text, ''), e.seq
	from events e
	where ` + owner + ` and e.seq > $2
	  and e.type in ` + caughtUpTypes + `
	  and ` + caughtUpAsk + ` = $3
	  and (select 1 from graph_tuples t
	       where t.from_kind = 'ask' and t.from_id = $3 and t.source_event = e.id
	       limit 1) is null
	order by e.id`
}

// caughtUpSinceOnIssue and caughtUpSinceOnDocument are caughtUpSince of an ask an issue owns, read
// through events_issue_key_seq_key, and of one a document no issue owns, read through
// events_artifact_id_seq_key.
// kept for one release: catch-up
var (
	caughtUpSinceOnIssue    = caughtUpSince(`e.issue_key = $1`)
	caughtUpSinceOnDocument = caughtUpSince(`e.artifact_id = $1::uuid`)
)

// scanCaughtUpEvent reads one row of caughtUpRead or caughtUpSince.
// kept for one release: catch-up
func scanCaughtUpEvent(row pgx.CollectableRow) (caughtUpEvent, error) {
	var event caughtUpEvent
	var actor []byte
	if err := row.Scan(&event.id, &event.eventType, &event.payload, &actor, &event.at, &event.ask,
		&event.issue, &event.document, &event.ownerSeq); err != nil {
		return event, err
	}
	if err := json.Unmarshal(actor, &event.actor); err != nil {
		return event, fmt.Errorf("read the actor of event %d: %w", event.id, err)
	}
	return event, nil
}

// askNodeMigration is the migration that gives the asks and ask_followers rows their marker
// (20261125163550_asks_node): no build marks a row before it is applied, and every build of this
// release applies it before it serves.
// kept for one release: catch-up
const askNodeMigration = 20261125163550

// catchUpWindowOpens is the read of where the catch-up's window opens (catchUp): the newest event
// created before the newest server_build_announcements row announced before the migration $1
// (askNodeMigration) was applied, 0 where there is none.
// kept for one release: catch-up
const catchUpWindowOpens = `
	select coalesce((
		select e.id from events e
		where e.created_at < (
			select a.announced_at from server_build_announcements a
			where a.announced_at < (select m.applied_at from schema_migrations m where m.version = $1)
			order by a.seq desc limit 1)
		order by e.id desc limit 1), 0)`

// catchUp is phase 2, and reports whether an ask failed. Its window opens at the newest
// server_build_announcements row announced before the migration that gives the asks rows their
// marker was applied (catchUpWindowOpens): the fleet last stood whole then on a build that marks
// no row, no row was marked before the migration, and every process of a build that marks one
// started after it. So the window holds every event a process of another build appended on a row
// a build of this release marked, whatever was announced since: a rollback to the release before
// announces that build again once the fleet stands whole on it, and an event that build appended
// after this build's last pass and before that announcement is in the window of the next roll
// forward's first pass, of this commit or of another of this release. Where no announcement
// precedes the migration, as on a database a build of this release created, it opens at the first
// event. The step reads it on its first pass and keeps it, since nothing it is read from moves
// once the migration is applied. A pass reads the window's events no tuple states once
// (caughtUpRead), from the step's high-water mark on, and catches each ask up in a transaction of
// its own (catchUpAsk); a failure on one is logged and leaves the others to go on.
//
// An event that needs nothing - one older than the node's newest tuple of its attribute, a follow
// the backfill already stated at the sentinel, an event of an ask the backfill has not marked yet -
// states nothing, so no tuple ever names it and every pass would read it again. The step keeps
// instead what its passes read. After a pass on which no ask failed, every event at or below the
// floor it read from (writerFloor) was final before it read, so it read each of them, and the
// high-water mark moves up to that floor; the ids it read above the floor are kept until the mark
// passes them. A pass on which an ask failed moves neither, and the next pass reads that ask's
// events again, with whatever else needed nothing. The mark is the process's own: a process that
// takes the step over reads its window again from where it opens, and an event already stated is
// left out of its read as it is of every read.
// kept for one release: catch-up
func (s *DecisionStep) catchUp(ctx context.Context) (bool, error) {
	floor, newest, err := s.writerFloor(ctx)
	if err != nil {
		return false, fmt.Errorf("read which events the catch-up reads final: %w", err)
	}
	if newest < s.caughtUpThrough || newest < s.windowOpens {
		// The log holds no event as far as the mark or the window's start (TRUNCATE ... RESTART
		// IDENTITY): its ids are a new log's, and nothing either says holds of them.
		s.caughtUpThrough, s.caughtUpAbove, s.windowRead = 0, nil, false
	}
	if !s.windowRead {
		if err := s.database.Pool.QueryRow(ctx, catchUpWindowOpens, askNodeMigration).Scan(&s.windowOpens); err != nil {
			return false, fmt.Errorf("read where the catch-up's window opens: %w", err)
		}
		s.windowRead = true
	}
	read := s.caughtUpAbove
	if read == nil {
		// A nil list is bound as null, and no id is unequal to every member of null.
		read = []int64{}
	}
	rows, err := s.database.Pool.Query(ctx, caughtUpRead, max(s.windowOpens, s.caughtUpThrough), read)
	if err != nil {
		return false, fmt.Errorf("read the events no tuple states: %w", err)
	}
	unstated, err := pgx.CollectRows(rows, scanCaughtUpEvent)
	if err != nil {
		return false, fmt.Errorf("read the events no tuple states: %w", err)
	}
	failed := false
	// What the asks' own reads under their locks found beside what the pass read, which the
	// high-water mark takes in as read too.
	var readUnderLocks []caughtUpEvent
	for first := 0; first < len(unstated); {
		last := first + 1
		for last < len(unstated) && unstated[last].ask == unstated[first].ask {
			last++
		}
		askID := unstated[first].ask
		applied, later, err := s.catchUpAsk(ctx, askID, unstated[first:last])
		first = last
		if err != nil {
			failed = true
			s.logger.Error("dispatch: decisions step could not catch up an ask; the next tick tries again",
				"ask", askID, "error", err)
			continue
		}
		readUnderLocks = append(readUnderLocks, later...)
		if applied > 0 {
			s.logger.Info("dispatch: decisions step caught up the events another build wrote", "ask", askID, "events", applied)
		}
	}
	if !failed {
		s.keepCaughtUp(floor, append(unstated, readUnderLocks...))
	}
	return failed, nil
}

// keepCaughtUp moves the high-water mark over what a pass on which no ask failed read: every event
// at or below floor, and the ids of what it read above it, beside those an earlier pass read above
// the mark that it has not passed yet.
// kept for one release: catch-up
func (s *DecisionStep) keepCaughtUp(floor int64, read []caughtUpEvent) {
	through := max(s.caughtUpThrough, floor)
	above := s.caughtUpAbove[:0]
	for _, id := range s.caughtUpAbove {
		if id > through {
			above = append(above, id)
		}
	}
	for _, event := range read {
		if event.id > through {
			above = append(above, event.id)
		}
	}
	s.caughtUpThrough, s.caughtUpAbove = through, above
}

// caughtUpEvent is one event a catch-up read (caughtUpRead, caughtUpSince): the event, the ask it
// names, and its owner's key - its issue, or the document it belongs to - with ownerSeq, for a row
// of caughtUpRead the owner's last_seq as that read saw it.
// kept for one release: catch-up
type caughtUpEvent struct {
	id              int64
	eventType       string
	payload         []byte
	actor           model.Actor
	at              time.Time
	ask             string
	issue, document string
	ownerSeq        int64
}

// catchUpAsk applies, in one transaction, the events of askID a pass read (unstated), and every
// later one, in the order of their ids. It takes the store's lock order (api/inbox_read.go's
// markInboxReadThreads), as every writer of an ask's answer does: the ask's owner row first - its
// issue, or the document an ask on no issue belongs to, read off the row without a lock and taken
// through events.LockOwnerRows - then the ask's row, `for no key update`, and only then the nodes
// and the tuples asks.Settle and asks.CatchUpFollower write. An ask whose owner moved between the
// two is tried again at the next pass. An ask whose row is gone, or names no node, is the
// backfill's: the row is stated whole when it is marked.
//
// The pass read its events before it took any lock, so a write of another build can commit on the
// ask in between: the write holds the owner row, which the lock waits for, and its event is newer
// than any the pass read. So under its locks catchUpAsk reads the ask's events its owner appended
// since the pass's read (caughtUpSince) and applies them after the ones read. Without them the row
// Settle writes would be the one the older event records, written over the newer write, and every
// reader of the row would see the older state until the next pass, which would then skip the newer
// event as older than the node's answer_state. It answers the events it read under its locks too,
// which the pass's high-water mark takes in.
//
// An answer event older than the node's newest answer_state, and a follower event older than the
// node's newest followed_by to that session, is skipped, so an old event caught up after a newer
// write never puts back what that write replaced. Every other answer event is written as
// asks.Settle writes the event's own state, answer and resolution, under the event, by its actor at
// its time; a follower event is stated as the row stands (asks.CatchUpFollower). It answers how
// many events it applied.
// kept for one release: catch-up
func (s *DecisionStep) catchUpAsk(ctx context.Context, askID string, unstated []caughtUpEvent) (int, []caughtUpEvent, error) {
	if askID == "" {
		return 0, nil, nil
	}
	var issue, document string
	err := s.database.Pool.QueryRow(ctx, `
		select coalesce(issue_key, ''), coalesce(artifact_id::text, '') from asks where id = $1::uuid
	`, askID).Scan(&issue, &document)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, nil, nil
	}
	if err != nil {
		return 0, nil, fmt.Errorf("read the owner of ask %s: %w", askID, err)
	}
	tx, err := s.database.Pool.Begin(ctx)
	if err != nil {
		return 0, nil, err
	}
	defer tx.Rollback(ctx)
	owners := events.OwnerRows{}
	owners.Add(issue, document)
	if err := events.LockOwnerRows(ctx, tx, owners); err != nil {
		return 0, nil, err
	}
	var marked bool
	var heldIssue, heldDocument string
	err = tx.QueryRow(ctx, `
		select node_kind is not null, coalesce(issue_key, ''), coalesce(artifact_id::text, '')
		from asks where id = $1::uuid for no key update
	`, askID).Scan(&marked, &heldIssue, &heldDocument)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && !marked) {
		return 0, nil, nil
	}
	if err != nil {
		return 0, nil, fmt.Errorf("lock ask %s: %w", askID, err)
	}
	if heldIssue != issue || heldDocument != document {
		return 0, nil, fmt.Errorf("ask %s moved to another owner while its catch-up took the owner's row", askID)
	}
	var later []caughtUpEvent
	if read := unstated[0]; read.issue != "" || read.document != "" {
		since, owner := caughtUpSinceOnIssue, read.issue
		if read.issue == "" {
			since, owner = caughtUpSinceOnDocument, read.document
		}
		rows, err := tx.Query(ctx, since, owner, read.ownerSeq, askID)
		if err != nil {
			return 0, nil, fmt.Errorf("read the events of ask %s appended since the pass read it: %w", askID, err)
		}
		if later, err = pgx.CollectRows(rows, scanCaughtUpEvent); err != nil {
			return 0, nil, fmt.Errorf("read the events of ask %s appended since the pass read it: %w", askID, err)
		}
		// Each is newer than every event the pass read: the owner's events commit in the order of
		// their sequences, which is the order of their ids.
		unstated = append(slices.Clip(unstated), later...)
	}
	applied := 0
	for _, event := range unstated {
		done, err := applyCaughtUpEvent(ctx, tx, askID, event)
		if err != nil {
			return 0, nil, fmt.Errorf("apply event %d (%s): %w", event.id, event.eventType, err)
		}
		if done {
			applied++
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, nil, fmt.Errorf("commit the catch-up of ask %s: %w", askID, err)
	}
	return applied, later, nil
}

// applyCaughtUpEvent applies one event a pass read, in catchUpAsk's transaction, and reports
// whether it stated anything.
// kept for one release: catch-up
func applyCaughtUpEvent(ctx context.Context, tx pgx.Tx, askID string, event caughtUpEvent) (bool, error) {
	if event.eventType == "ask.follower_added" || event.eventType == "ask.follower_removed" {
		var payload model.AskFollowerEventPayload
		if err := json.Unmarshal(event.payload, &payload); err != nil {
			return false, err
		}
		newest, err := newestSource(ctx, tx, `t.kind = $2 and t.to_kind = $3 and t.to_id = $4`,
			askID, graphstore.TupleFollowedBy, graphstore.KindSession, payload.SessionID)
		if err != nil || event.id < newest {
			return false, err
		}
		return asks.CatchUpFollower(ctx, tx, askID, payload.SessionID, event.eventType == "ask.follower_added", event.actor, event.at, event.id)
	}
	var recorded model.Ask
	if err := json.Unmarshal(event.payload, &recorded); err != nil {
		return false, err
	}
	switch recorded.State {
	case asks.StateOpen, asks.StateAnswered, asks.StateResolved:
	default:
		return false, fmt.Errorf("the event records the answer state %q", recorded.State)
	}
	newest, err := newestSource(ctx, tx, `t.kind = $2 and t.to_kind = $3`, askID, graphstore.TupleAnswerState, graphstore.KindValue)
	if err != nil || event.id < newest {
		return false, err
	}
	change := asks.Recorded(recorded.State, recorded.Answer, recorded.Resolution)
	change.Who, change.At, change.Event = event.actor, event.at, event.id
	if _, err := asks.Settle(ctx, tx, askID, change); err != nil {
		return false, err
	}
	return true, nil
}

// newestSource is the source_event of the newest tuple out of askID's node that narrow keeps, 0
// where there is none.
// kept for one release: catch-up
func newestSource(ctx context.Context, tx pgx.Tx, narrow string, askID string, args ...any) (int64, error) {
	var newest int64
	if err := tx.QueryRow(ctx, `
		select coalesce((select t.source_event from graph_tuples t
		                 where t.from_kind = 'ask' and t.from_id = $1 and `+narrow+`
		                 order by t.seq desc limit 1), 0)
	`, append([]any{askID}, args...)...).Scan(&newest); err != nil {
		return 0, fmt.Errorf("read the newest tuple of ask %s: %w", askID, err)
	}
	return newest, nil
}
