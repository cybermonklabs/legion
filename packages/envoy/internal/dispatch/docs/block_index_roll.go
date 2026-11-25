package docs

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/graphcensus"
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
// comment they name whose row names no node its node and its tuples (refs.CommentWrite), records
// in the cursor's row whether this build owes the object pass, and counts in tick what it read,
// marked and wrote, and whether another build serves.
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
// it, and the `dispatch: block index` line prints it. It decides nothing of which events the tick
// reads; it makes the tick owe the object pass, as below.
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
// after it commits, or later while a transaction on the instance stays open across those ticks.
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
// causedByEvent): a tick that wrote a comment's author's node first and then waited for that
// document's row would hold what the settlement waits for, while the settlement held what the
// tick waits for. A tick that fails moves nothing and the next reads the same events again; a
// document already marked keeps its mark and its time. A document marked here is listed by the
// owed documents this same tick reads next, and drained by it when its batch has room.
//
// It answers, as owed, the time since which this build owes the object pass
// (writeObjectNodesAnotherBuildWrote), nil where it owes none. The tick owes the pass while a
// process of another build serves and on the first tick that finds none after one did
// (block_index_event_cursors.another_build_seen), since that build writes rows with no node; where
// an event it reads names a row of an issue, a document, an ask or a message with no node, or a
// removed document whose node still stands (graphcensus.EventsOweObjectPass), as such a build
// leaves them when it holds every period of the lease; and on each process's first tick with a
// proven horizon and once objectPassInterval has passed since the process's last pass finished: a
// conversation's file such a build uploads appends no event, and a process that starts may follow
// a release that wrote while none of this build's ran. That tick sets the time in the cursor's
// row, in the statement that moves the cursor, and a debt an earlier tick left stands until a pass
// clears it, so whichever process of the build holds the lease next runs it. It answers owed only
// once its transaction has committed, and nil where it fails. Before a horizon is proven the
// cursor holds no row, so no debt can be recorded: such a tick owes the pass only while another
// build serves (rollNow), and the process's next tick owes it otherwise. A rollNow pass does not
// count as the process's pass (objectPassFinished), so the process's first tick with a proven
// horizon owes the pass however recently a rollNow pass ran: the other build may have removed or
// made a document after it and stopped before that tick, and no event that tick reads names it.
func (s *Service) readEventsAfterCursor(ctx context.Context, tick *BlockIndexTick) (full bool, owed *time.Time, err error) {
	safe, err := s.blockIndexHorizon().Safe(ctx)
	if err != nil {
		return false, nil, fmt.Errorf("read the event log's safe horizon: %w", err)
	}
	live, err := processes.Live(ctx, s.store.Pool)
	if err != nil {
		return false, nil, err
	}
	tick.AnotherBuild = slices.ContainsFunc(live, func(process processes.Process) bool {
		return process.Commit != s.buildCommit
	})
	tx, err := s.store.Pool.Begin(ctx)
	if err != nil {
		return false, nil, err
	}
	defer tx.Rollback(ctx)
	queued, ledger := queueWrites(ctx, tx)
	// A build's cursor is its own row, else where its first tick starts it: at the newest event_id
	// a row of its own marking rule holds, else at the horizon the newest backfill of its own rule
	// recorded, else at the tick's horizon. A safe of 0 is a horizon that has proven nothing yet,
	// as a process's first observation never does on its own, or an empty log: until a tick reads
	// one, nothing is read, and a build's cursor never starts at the beginning of the log.
	var cursor *int64
	var seen bool
	if err := tx.QueryRow(queued, `
		select coalesce(
			(select event_id from block_index_event_cursors where build_commit = $1),
			case when $2::bigint > 0 then coalesce(
				(select max(event_id) from block_index_event_cursors where marking_rule = $3::integer),
				(select event_id from graph_backfill_horizon where marking_rule = $3::integer),
				$2::bigint) end),
			coalesce((select another_build_seen from block_index_event_cursors where build_commit = $1), false)
	`, s.buildCommit, safe, s.markingRule).Scan(&cursor, &seen); err != nil {
		return false, nil, fmt.Errorf("read this build's block-index event cursor: %w", err)
	}
	if cursor == nil {
		if tick.AnotherBuild {
			return false, &rollNow, nil
		}
		return false, nil, nil
	}
	// Another process of this build may have moved the cursor past this process's horizon; the
	// cursor never moves back.
	next := max(*cursor, safe)
	// The pass is owed while another build serves and on the first tick after one did, on a
	// process's first tick, and an interval after its last pass.
	finished := s.objectPassFinished.Load()
	owes := tick.AnotherBuild || seen || finished == 0 || s.now().Sub(time.Unix(0, finished)) >= objectPassInterval
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
			return false, nil, fmt.Errorf("read the events after the cursor: %w", err)
		}
		if full = read == blockIndexEventPage; full {
			next = *last
		}
		if !owes {
			if owes, err = graphcensus.EventsOweObjectPass(queued, tx, *cursor, next); err != nil {
				return false, nil, err
			}
		}
		if len(comments) > 0 {
			if err := refs.LockCommentOwners(queued, tx, comments); err != nil {
				return false, nil, err
			}
			if commented, err = refs.CommentsNamingNoNode(queued, tx, comments, causes); err != nil {
				return false, nil, err
			}
		}
	}
	// The write phase, in the store's lock order: the cursor, the documents' marks, the comments.
	// The cursor row records the build's marking rule beside where it has read to, and whether
	// this build owes the object pass.
	var debt *time.Time
	if err := write(queued, tx, func(b *store.WriteBatch) error {
		b.Queue("move this build's block-index event cursor", `
			insert into block_index_event_cursors (build_commit, event_id, marking_rule, another_build_seen, object_nodes_owed_at)
			values ($1, $2, $5, $4, case when $3 then clock_timestamp() end)
			on conflict (build_commit) do update
			set event_id = greatest(block_index_event_cursors.event_id, excluded.event_id),
			    marking_rule = excluded.marking_rule,
			    another_build_seen = excluded.another_build_seen,
			    object_nodes_owed_at = coalesce(excluded.object_nodes_owed_at, block_index_event_cursors.object_nodes_owed_at)
			returning object_nodes_owed_at
		`, s.buildCommit, next, owes, tick.AnotherBuild, s.markingRule).QueryRow(func(row pgx.Row) error {
			return row.Scan(&debt)
		})
		return nil
	}); err != nil {
		return false, nil, err
	}
	if len(named) > 0 {
		if err := write(queued, tx, func(b *store.WriteBatch) error {
			b.Queue("mark the documents the events after the cursor name", markVersionedDocuments, named, s.markingRule).QueryRow(func(row pgx.Row) error {
				return row.Scan(&marked)
			})
			return nil
		}); err != nil {
			return false, nil, err
		}
	}
	if commented.Comments() > 0 {
		if err := write(queued, tx, func(b *store.WriteBatch) error {
			commented.Queue(b)
			return nil
		}); err != nil {
			return false, nil, fmt.Errorf("write the nodes of the comments the events after the cursor name: %w", err)
		}
		written = commented.Comments()
	}
	if err := ledger.runQueue(ctx, nil); err != nil {
		return false, nil, fmt.Errorf("write what the events after the cursor name: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return false, nil, fmt.Errorf("commit what the events after the cursor name: %w", err)
	}
	// What the tick counts is what it committed: a read that fails counts nothing.
	tick.Events, tick.Marked, tick.Comments = read, int(marked), written
	return full, debt, nil
}

// objectPassBatches bounds the batches one tick's object pass writes across the four kinds, so a
// first deploy's whole backlog never holds the block-index lease in one tick: a pass that stops
// with rows left leaves its debt and makes the tick full, and the next one runs blockIndexStep
// later. At the backfill's batch size of 200 it writes the nodes of up to 800 rows a tick. Rows no
// pass can write - a refused party, a node held in another project, live or gone - spend none of
// it and open no transaction: each pass leaves them before it writes a batch, and reads them
// again, two index reads for each 200 of them (graphcensus's writeObjectKind).
const objectPassBatches = 4

// orphanedNodesSwept bounds the orphaned document nodes one tick states gone, each in a
// transaction of its own (graphcensus's StateOrphanedGone); a tick that reads more leaves its debt
// and is full.
const orphanedNodesSwept = 20

// objectPassInterval is the longest a process of this build goes without finishing the object
// pass: beside the ticks that owe it while another build serves, on the first tick after one did,
// and on events that name a row with no node (readEventsAfterCursor), a tick owes it once this
// long has passed since the process's last pass finished (Service.objectPassFinished). A
// conversation's file that a build before this one uploads appends no event, so no event a tick
// reads names it: while that build serves, every tick owes the pass, and once none does, the file
// waits for its node this long and one tick at most. A pass with nothing to write reads the four
// tables whole, 97 to 190 ms at production's row counts, so each process spends that once in this
// interval while this build serves alone, and on every tick while another build serves.
const objectPassInterval = 10 * time.Minute

// rollNow is the debt a tick answers while another build serves before any horizon is proven
// (readEventsAfterCursor): no cursor row holds it, so the pass runs, no row is cleared, and the
// pass does not count as the process's own (objectPassFinished).
var rollNow time.Time

// writeObjectNodesAnotherBuildWrote is the block-index tick's second step: while this build owes
// the object pass (readEventsAfterCursor's owed), it writes the node of every issue, document, ask
// and message whose row names none, through the backfill's object pass (graphcensus's
// WriteOwedObjectNodes), and states gone the node of every document a build before this one
// removed (graphcensus's OrphanedDocumentNodes and StateOrphanedGone). It counts both in tick, and
// answers whether it stopped with work left.
//
// The release before this one writes those four kinds' rows with no node, as it writes a
// version's block rows with none, and nothing else would give them one until someone ran the
// backfill again: an issue it creates, its spec, a document it uploads, a conversation's file,
// whose upload appends no event, an ask and a message. And its removal of a document states
// nothing, so the node this build wrote for it stands live behind no row. So the pass reads rows,
// not events, on the ticks that owe it (readEventsAfterCursor): every tick while another build
// serves and the first after one did, one whose events name such a row or such a node, each
// process's first, and one an interval after the process's last pass. A
// document node with no row behind it can mean nothing else: every writer of this build writes a
// node in the transaction that writes its row, and the pass writes one only for a row that stands
// under the owner locks it takes. The sweep states such a node gone as this build's own removal of
// the document states it (api's removeArtifact), under the removal's own event, so the store keeps
// every statement it held and the history of a removed document is one whichever build served the
// removal; a node whose row a create of this build commits meanwhile is left, since the sweep
// reads under its lock of the node whether a row stands.
//
// It runs after the event read has committed and outside its transaction, each batch in a
// transaction of its own that takes its rows' owner locks first (graphcensus's writeObjectBatch),
// and each gone statement in its own, owner lock first too, so it holds no lock of the event
// read's while it takes them. It writes at most objectPassBatches batches and states at most
// orphanedNodesSwept nodes gone a tick, and a kind that fails holds back neither the other kinds
// nor the sweep. A pass that fails, or stops with work left, leaves the debt, which only a pass
// that finishes clears, so no row is left without its node and no node without its row. A node
// the sweep leaves is logged with the reason.
//
// The pass and the sweep stay for good: a rollback to any earlier release, and a write an older
// build makes in any later roll, still reach them.
func (s *Service) writeObjectNodesAnotherBuildWrote(ctx context.Context, tick *BlockIndexTick, owed *time.Time) (more bool, err error) {
	if owed == nil {
		return false, nil
	}
	pass, passErr := graphcensus.WriteOwedObjectNodes(ctx, s.store, objectPassBatches)
	tick.ObjectNodes = int(pass.Rows)
	more = pass.More
	if pass.Moved != nil {
		// No second pass cures a row whose node the store holds in another project, so it holds
		// back no debt: it is logged for a person, and the census names the row.
		slog.Error("dispatch: block index left rows whose node the store holds in another project", "error", pass.Moved)
	}
	failed := []error{passErr}
	orphaned, err := graphcensus.OrphanedDocumentNodes(ctx, s.store.Pool, orphanedNodesSwept+1)
	failed = append(failed, err)
	more = more || len(orphaned) > orphanedNodesSwept
	for _, ref := range orphaned[:min(len(orphaned), orphanedNodesSwept)] {
		switch err := graphcensus.StateOrphanedGone(ctx, s.store, ref); {
		case errors.Is(err, graphcensus.ErrNotOrphaned):
			slog.Info("dispatch: block index left a document node it read as orphaned", "node", ref, "reason", err)
		case err != nil:
			failed = append(failed, fmt.Errorf("state the orphaned node of %s gone: %w", ref, err))
		default:
			tick.OrphansGone++
		}
	}
	switch err := errors.Join(failed...); {
	case err != nil:
		return more, fmt.Errorf("write the nodes of the rows another build wrote: %w", err)
	case more:
		return more, nil
	}
	if owed == &rollNow {
		// A pass no cursor row records is not the process's pass: its first tick with a proven
		// horizon still owes one, which reads what another build did after this pass.
		return false, nil
	}
	s.objectPassFinished.Store(s.now().UnixNano())
	if _, err := s.store.Pool.Exec(ctx, `
		update block_index_event_cursors set object_nodes_owed_at = null
		where build_commit = $1 and object_nodes_owed_at = $2
	`, s.buildCommit, *owed); err != nil {
		return false, fmt.Errorf("clear this build's debt of the object pass: %w", err)
	}
	return false, nil
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
