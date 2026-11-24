package docs

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/processes"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// blockIndexEventPage is the most events one tick reads (readEventsAfterCursor). A tick reads the
// events after its build's cursor, at most this many, and moves the cursor to the last one it read
// when the page is full, so it reads at most this many event rows however far the log has run
// ahead of the cursor; a full page makes the tick full (BlockIndexTick.Full), and the next tick,
// blockIndexStep later, reads on from there. It is the suggestion outcome sweep's page (api's
// suggestionOutcomePage).
const blockIndexEventPage = 1000

// readEventPage is one tick's read of the event log, in one statement: it reads up to $3 events
// after the cursor ($1) and at or below the safe horizon ($2), in id order, and answers how many
// events it read, the id of the last one (null when it read none), every document an
// artifact.version or artifact.created event among them names, and every document of each issue
// an issue.created event among them names, in id order, and the comments a comment.created or
// comment.answered event among them names whose row names no node, in id order, each beside the
// first of those events that names it.
//
// A version event names its document in payload->>'artifact_id' (ArtifactVersionEventPayload),
// and the event that creates a document, which records its version 1, carries the whole artifact
// in its payload and names the document in payload->'artifact'->>'id' (api's
// artifactCreatedEventPayload, read the same way by versionEventID). An issue's create writes its
// primary document, the spec, and records that document's version 1 with the issue.created event
// alone (api's issue create, read the same way by versionEventID), which names the issue and no
// document: the event's issue_key, its owner, is the issue, so every document of kind 'doc' that
// issue holds in artifacts is named, read through artifacts_issue_key. A comment's event carries
// the comment, the reply a comment.answered stores included, and names it in payload->>'id' (api's
// commentEventPayload). Each event names what it names whichever build wrote it, and the read
// filters on nothing else: a project document's event carries no issue key, and an agent's
// workspace document writes no event at all. The payload is read only for the four types that
// name a document or a comment, and a value that is no uuid names nothing rather than failing the
// tick (store.CastableUUID). Only a document that exists is named, and only one of kind 'doc'. The
// page bounds the issues as it bounds the documents, and an issue holds a few documents.
//
// The comments rows are read with no lock, and a comment whose row already names its node, as the
// row of a comment this build writes does from the write that stores it, is left out: the tick
// has nothing to write for it, so it takes no owner lock for it (readEventsAfterCursor). A comment
// another writer gives its node after this read is left alone by the read the tick makes under
// the owner locks (refs.CommentsNamingNoNode).
var readEventPage = `
	with page as (
		select id, type, issue_key, case type
			when 'artifact.version' then ` + store.CastableUUID(`payload->>'artifact_id'`) + `
			when 'artifact.created' then ` + store.CastableUUID(`payload->'artifact'->>'id'`) + `
		end as artifact_id, case when type in ('comment.created', 'comment.answered')
			then ` + store.CastableUUID(`payload->>'id'`) + `
		end as comment_id
		from events
		where id > $1 and id <= $2
		order by id
		limit $3
	),
	named as (
		select a.id from artifacts a
		where a.kind = 'doc' and (
			a.id = any(array(select artifact_id from page where artifact_id is not null))
			or a.issue_key = any(array(select distinct issue_key from page where type = 'issue.created' and issue_key is not null)))
	),
	commented as (
		select comment_id, min(id) as cause from page
		where comment_id is not null
		  and exists (select 1 from comments c where c.id = page.comment_id and c.node_kind is null)
		group by comment_id
	)
	select (select count(*) from page), (select max(id) from page),
	       coalesce((select array_agg(id::text order by id) from named), '{}'),
	       coalesce((select array_agg(comment_id::text order by comment_id) from commented), '{}'),
	       coalesce((select array_agg(cause order by comment_id) from commented), '{}')`

// markVersionedDocuments marks owed a block index, with this build's own mark of its marking rule
// $2 (refs.BlockIndexMarkingRule, the rule the Service carries), every document of $1, the
// documents the events after the cursor name (readEventPage), that holds no such mark of $2 or a
// higher rule yet and whose blocks this build's store last stated at a version other than the one
// the document stands at, and answers how many it marked.
//
// A document is marked where store_version, the version whose blocks this build's index pass last
// stated in the store (refs' BlockIndexing.queueStand, its one writer), is not the document's
// version, a null on either side included. The release before this one moves version and never
// store_version, so a document it indexed after this build's store stated it is marked, and so is
// one it created, whose store_version is null, and one whose version a write owes an index still
// (null): whatever that version drops, moves or rewords, which may leave no row naming no node,
// this build's pass over it states. A document this build wrote and indexed stands at the
// store_version that pass wrote, and is not marked again.
//
// The mark is the one the backfill sets (graphcensus's markStaleBatch) and oweDocumentIndex keeps:
// store_owed_at, which the release before this one neither reads nor clears, with the rule that
// set it in store_owed_rule. A mark of a lower rule is read as no mark, in the selection and in
// the update alike, so the statement raises it to $2 and keeps its time: the pass of rule $2 owes
// the document what its rule owes, and a pass of the lower rule would clear a mark of its own
// rule (queueStand). A mark of $2 or a higher rule is left as it is, and so is its rule. A row
// that holds no mark takes $2 whatever rule it holds, since the rule is read only where a mark
// stands. The statement takes no owner lock and writes nothing but document_block_versions rows,
// and it takes those `for no key update` in ascending artifact_id order before its update
// re-locks them, which is the order graphcensus's lockBlockDocuments takes several of them in: a
// writer holding several such rows and this statement can never each hold one the other waits
// for. It decides on the rows as it locks them, in the write phase, so a row a version's index
// stated meanwhile is not marked. Its selection and its update read one condition of no mark
// (refs.OwedMarkAbsent).
var markVersionedDocuments = `
	with held as (
		select b.artifact_id from document_block_versions b
		where b.artifact_id = any($1::uuid[])
		  and ` + refs.OwedMarkAbsent("b", "$2::integer") + `
		  and not coalesce(b.store_version = b.version, false)
		order by b.artifact_id
		for no key update of b
	),
	marked as (
		update document_block_versions b set store_owed_at = coalesce(b.store_owed_at, now()), store_owed_rule = $2::integer
		from held
		where b.artifact_id = held.artifact_id and ` + refs.OwedMarkAbsent("b", "$2::integer") + `
		returning b.artifact_id
	)
	select count(*) from marked`

// readEventsAfterCursor is the first step of every block-index tick: it reads the events after its
// build's cursor, marks owed a block index, with this build's own mark, every document they name
// that this build's store has not stated at its version (markVersionedDocuments), gives every
// comment they name whose row names no node its node and its tuples (refs.CommentWrite),
// and counts in tick what it read, marked and wrote, and whether another build serves.
//
// The release before this one indexes a version it writes, and a document it creates, the spec of
// an issue it creates included, with no node, writes a comment's row with no node, states nothing
// in the node and tuple store, and neither reads nor sets store_owed_at. A document whose mark this
// build's drain has cleared and which that release then edits, one it creates after the drain,
// and a comment it writes after the backfill therefore hold no mark, and nothing would bring this
// build to them to name their rows' nodes and state what they project. So every tick reads the
// events after its build's cursor, whether or not another build serves: which build's ticks come
// first in each period of the shared block-index lease (BlockIndexLease), and so whether this
// build ticks while that release serves at all, decides nothing, and no roll, rollback or roll
// forward, with two builds serving at once or none of this code serving for a while, leaves a
// write of that release unread. tick.AnotherBuild says whether a live server process of another
// build serves (processes.Live: a process whose Commit is not this build's own); the tick reports
// it, and the `dispatch: block index` line prints it, and it decides nothing.
//
// The cursor is this build's row of block_index_event_cursors (migration 20261118115645), which
// records the marking rule the build applies (marking_rule, migration 20261120122600;
// refs.BlockIndexMarkingRule). The build's first tick that reads a proven horizon inserts the row:
// at the newest event_id a row of another build of its own marking rule holds; where no such row
// exists, at the horizon the newest backfill of its own rule recorded when it began
// (graph_backfill_horizon, one row for each rule, graphcensus's recordBackfillHorizon); and where
// neither exists, at that tick's horizon, so the cursor never starts at the beginning of the log.
// A build of the same rule marks what this build marks, so every event below its cursor is one it
// read and marked with the mark this build drains, and a deploy that keeps the rule does not read
// the log again from the backfill's horizon; every event above the horizon of a backfill of this
// rule is one that backfill cannot have seen, so whatever the release before wrote since either,
// this build reads. A cursor or a horizon of another rule is never read: that build or that
// backfill marked what its own rule owes, which is not what this build owes, as a log consumer
// takes a peer's committed offset only when it does the same work (Kafka's consumer groups). A
// build that finds neither a cursor nor a horizon of its own rule leaves every event below its
// first horizon to the backfill of its own rule, which reads every row whatever its age when it
// runs. A build that holds a row starts at its own row, however old, and reads every event since,
// one page a tick. The cursor moves with each tick, so each tick reads only the events since the
// last one and nothing scans a whole table. A tick reads (cursor, safe], safe being the event
// log's safe horizon (events.Horizon.Safe): event ids are not commit order, so an uncapped
// `id > cursor` read could pass an event still committing, and the cursor never moves past safe.
// A Horizon proves an observation only by a later one, so an event is read by the second tick
// after it commits, or later while an event write of this database stays open across those ticks.
//
// The tick reads everything first and writes after, in one batch back to back before its commit
// (queueWrites), so its transaction holds a transaction id for those statements alone: the safe
// horizon, the cursor and the page of events (readEventPage), which keeps, read with no lock, only
// the comments the events name whose row names no node; then the owner locks of those comments
// alone (refs.LockCommentOwners), the lock every writer of a comments row takes, and those
// comments' rows under them (refs.CommentsNamingNoNode), which decides again under the locks, so a
// comment another writer gave its node between the two reads is left alone. A comment whose row
// already names its node, as the row of a comment this build writes does, costs the tick no
// owner lock: every tick reads the event of each comment written since its cursor, and locking the
// owner of each would have every tick wait for, and hold up, the writers of every issue a recent
// comment was written on. The batch then takes its locks in the store's order: the cursor row
// first, which only block-index ticks write; then the marking statement's document_block_versions
// rows `for no key update`, in ascending artifact_id order; then the comments' node rows, in
// ascending (kind, id) in one store write, their tuples, and their comments rows. A writer under
// the revision fence takes a document's document_block_versions row before any node it writes
// (the fifth invariant on api's markInboxReadThreads), and a settlement takes its document's row
// and then the node of the session that follows an ask it opens (lockSettlementFence,
// followsByEvent): a tick that wrote a comment's author's node first and then waited for that
// document's row would hold what the settlement waits for, while the settlement held what the
// tick waits for. A tick that fails moves nothing and the next reads the same events again; a
// document already marked keeps its mark and its time. A document marked here is listed by the
// owed documents this same tick reads next, and drained by it when its batch has room.
func (s *Service) readEventsAfterCursor(ctx context.Context, tick *BlockIndexTick) (full bool, err error) {
	safe, err := s.blockIndexHorizon().Safe(ctx)
	if err != nil {
		return false, fmt.Errorf("read the event log's safe horizon: %w", err)
	}
	live, err := processes.Live(ctx, s.store.Pool)
	if err != nil {
		return false, err
	}
	tick.AnotherBuild = slices.ContainsFunc(live, func(process processes.Process) bool {
		return process.Commit != s.buildCommit
	})
	tx, err := s.store.Pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx)
	queued, ledger := queueWrites(ctx, tx)
	// A build's cursor is its own row, else where its first tick starts it: at the newest event_id
	// a row of its own marking rule holds, else at the horizon the newest backfill of its own rule
	// recorded, else at the tick's horizon. A safe of 0 is a horizon that has proven nothing yet,
	// as a process's first observation never does on its own, or an empty log: until a tick reads
	// one, nothing is read, and a build's cursor never starts at the beginning of the log.
	var cursor *int64
	if err := tx.QueryRow(queued, `
		select coalesce(
			(select event_id from block_index_event_cursors where build_commit = $1),
			case when $2::bigint > 0 then coalesce(
				(select max(event_id) from block_index_event_cursors where marking_rule = $3::integer),
				(select event_id from graph_backfill_horizon where marking_rule = $3::integer),
				$2::bigint) end)
	`, s.buildCommit, safe, s.markingRule).Scan(&cursor); err != nil {
		return false, fmt.Errorf("read this build's block-index event cursor: %w", err)
	}
	if cursor == nil {
		return false, nil
	}
	// Another process of this build may have moved the cursor past this process's horizon; the
	// cursor never moves back.
	next := max(*cursor, safe)
	var read, written int
	var marked int64
	var named []string
	var commented refs.CommentWrite
	if safe > *cursor {
		var last *int64
		var comments []string
		var causes []int64
		if err := tx.QueryRow(queued, readEventPage, *cursor, safe, blockIndexEventPage).Scan(
			&read, &last, &named, &comments, &causes,
		); err != nil {
			return false, fmt.Errorf("read the events after the cursor: %w", err)
		}
		if full = read == blockIndexEventPage; full {
			next = *last
		}
		if len(comments) > 0 {
			if err := refs.LockCommentOwners(queued, tx, comments); err != nil {
				return false, err
			}
			if commented, err = refs.CommentsNamingNoNode(queued, tx, comments, causes); err != nil {
				return false, err
			}
		}
	}
	// The write phase, in the store's lock order: the cursor, the documents' marks, the comments.
	// The cursor row records the build's marking rule beside where it has read to.
	if err := write(queued, tx, execStep("move this build's block-index event cursor", `
		insert into block_index_event_cursors (build_commit, event_id, marking_rule) values ($1, $2, $3)
		on conflict (build_commit) do update
		set event_id = greatest(block_index_event_cursors.event_id, excluded.event_id), marking_rule = excluded.marking_rule
	`, s.buildCommit, next, s.markingRule)); err != nil {
		return false, err
	}
	if len(named) > 0 {
		if err := write(queued, tx, func(b *store.WriteBatch) error {
			b.Queue("mark the documents the events after the cursor name", markVersionedDocuments, named, s.markingRule).QueryRow(func(row pgx.Row) error {
				return row.Scan(&marked)
			})
			return nil
		}); err != nil {
			return false, err
		}
	}
	if commented.Comments() > 0 {
		if err := write(queued, tx, func(b *store.WriteBatch) error {
			commented.Queue(b)
			return nil
		}); err != nil {
			return false, fmt.Errorf("write the nodes of the comments the events after the cursor name: %w", err)
		}
		written = commented.Comments()
	}
	if err := ledger.runQueue(ctx, nil); err != nil {
		return false, fmt.Errorf("write what the events after the cursor name: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return false, fmt.Errorf("commit what the events after the cursor name: %w", err)
	}
	// What the tick counts is what it committed: a read that fails counts nothing.
	tick.Events, tick.Marked, tick.Comments = read, int(marked), written
	return full, nil
}

// blockIndexHorizon is the events.Horizon the block-index tick reads the event log's safe horizon
// through (readEventsAfterCursor), built by the first tick that asks for it and observing through
// the store's horizon pool (store.Pool.Horizon), so an observation never queues behind the
// requests holding the shared pool, as the API server's one Horizon does (api's server.horizon).
// It is the service's own rather than that one because the service is built before the API
// server, and the tick also runs where no API server is: the service's own tests and the
// harnesses that drain the block index. It observes once a tick, and a tick runs under the
// block-index lease, so the fleet spends one transaction id on it a tick. A horizon pool that
// cannot open is logged once, and the Horizon then observes through the shared pool.
func (s *Service) blockIndexHorizon() *events.Horizon {
	s.horizonOnce.Do(func() {
		var db events.HorizonDB = s.store.Pool
		switch pool, err := s.store.Pool.Horizon(); {
		case err == nil:
			db = pool
		case !errors.Is(err, store.ErrPoolClosed):
			slog.Error("dispatch: could not open the event horizon's pool; the block-index tick observes the event log through the shared request pool until the process restarts",
				"error", err)
		}
		s.eventHorizon = events.NewHorizon(db)
	})
	return s.eventHorizon
}
