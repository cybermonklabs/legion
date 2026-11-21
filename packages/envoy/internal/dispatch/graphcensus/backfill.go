package graphcensus

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// backfillBatch is how many rows of one kind one transaction writes. Each row costs a node, a
// tuple or two and one projection column write, and an author costs a statement of its own, so
// a batch is a few hundred statements at most and holds its row locks for about as long as one
// request does.
const backfillBatch = 200

// backfillActor is who the run records as the writer of every node and tuple it writes. The
// run is nobody's act: a row written before the store existed has a writer of its own only
// where the row itself records one, and inventing one from anywhere else would state a fact no
// row holds.
var backfillActor = model.Actor{Kind: "system", ID: "dispatch-backfill-graph-store"}

// KindBackfill is what the run did for one kind.
type KindBackfill struct {
	Kind string
	// Rows is how many projection rows it gave a node, Nodes and Tuples how many of each it
	// wrote, and Skipped how many rows it could not do and left as they were.
	Rows, Nodes, Tuples, Skipped int64
	// Owed is how many documents it marked owed this release's block index (store_owed_at),
	// for the block kind alone: those whose rows it named the nodes of, and those another
	// release indexed after the store last stated them (markStaleDocuments).
	Owed int64
	// Complete is whether every row of the kind the store can hold now names its node, which
	// is when the arm of graph_edges over that kind's table answers nothing: each of the four
	// old arms answers a row only while that row names no node (migration 20261109215947).
	Complete bool
	// Remaining is how many rows of the kind still name no node.
	Remaining int64
}

// BackfillReport is what one run of the backfill did.
type BackfillReport struct {
	Kinds []KindBackfill
}

// Backfill writes the nodes and tuples of the comments, document blocks and block references
// written before the node and tuple store existed: every row of the three projected tables
// that names no node (migration 20261109022248 added the columns and filled none of them). It
// then mends what a process of the release before writes of a run of the command bar during a
// roll or after a rollback (backfillRuns, backfillRunFrames, backfillGoneRuns): a run it recorded
// with no interaction node, a frame it appended with no frame value, and a run whose row it
// deleted an hour old, whose node it states gone; and the prevented_by it leaves asserted at a
// check it drops or at a check of a document it retires, which it retracts
// (backfillEndedPreventions). It then gives every role the store census holds to a node and that
// has none its node and its page (backfillRoles), since the publisher writes those only for the
// events it publishes from then on, and every session the census holds to a node and that has none
// its node (backfillSessions): a session that only asked, followed or was reached before the store
// existed was recorded by no write of the store, and the followed_by edge to it would otherwise
// answer no node.
//
// It walks each kind in batches, writing a batch's nodes, its tuples and the projection
// columns that name them in one transaction, so a row and its node commit together or not at
// all, exactly as a live write does. It appends no event of its own, and the cause of a fact
// whose row's own event cannot be found is the store's sentinel: a comment's own event is the
// comment.created that stored it, a block's the event that recorded the version its row was
// written at, and a block reference's is its own source_seq, which is that event's id already.
//
// Each tuple takes its row's own created_at as its time and its row's own source_seq as its
// legacy_source_seq, so a backfilled edge is dated and sequenced exactly as the graph_edges arm
// it replaces dates and sequences it.
//
// A block's parent is the one fact it does not write. The parent is the nesting of the
// rendered version, which no column of document_blocks holds, and the version's stored markdown
// cannot give it either, since a paragraph's id is no part of that text (sizing's
// replayParents): only the server renders the document whose blocks the ids name. So the run
// marks the document instead, and the reconciliation's index pass states every block's node,
// part_of tuple and parent_block_id, which is the one writer of that column, and clears the mark
// (refs.IndexBlocks). The mark is this release's own, document_block_versions.store_owed_at
// (migration 20261116221539), and not the null version the release before this one reads too: a
// pod of that release, during a roll or after a rollback, would drain a null version as a debt of
// its own, write the rows and state no part_of, and the document would be owed nothing again. The
// mark is written by the batch that names the document's block nodes, in its transaction, so a
// run stopped anywhere leaves every document whose batch committed marked, and a second run,
// which reads no row a batch marked, need not find them again. Every mark the run writes is of
// this build's marking rule (refs.BlockIndexMarkingRule), which store_owed_rule records beside it
// (writeBlockBatch, markStaleBatch).
//
// A row the release before this one deleted - the row of a block its version dropped, or of a
// link a block no longer holds - is nothing the run reads: no row of it is left to read, and a
// node stated gone because its row is missing would be a repair. That release deleted it while it
// indexed a version after the store had stated the document, so the version the store last stated
// the document's blocks at (store_version, which only this release's index pass writes) is below
// the version the document's rows stand at. The run marks each such document too
// (markStaleDocuments), by those two versions and by no row of either side, and the
// reconciliation's index pass over the latest version then states each such block gone and
// retracts each such link, from the version's render (refs.IndexBlocks). No release deletes a
// comment's row (writeCommentBatch).
//
// Each pass walks only the rows its marker does not mark, which for the three projected tables is
// the node column pair the view's old arms test (migration 20261109215947), names their nodes on
// them in the transaction that writes their tuples, and reads no marked row, its guards included
// (blockNodeStands). So it can run again at any time over what is still unmarked - after a
// rollback, when pods of the release before this one wrote rows with no node - and each run does
// only the rows still owed: a second run over tables every row of which is marked writes no node,
// no tuple and no marker (TestASecondBackfillWritesNothingOverMarkedRows).
//
// Every node it writes through graphstore.Write restates its node (graphstore.Node's Restates):
// the run owns a row's facts, not the state or the visibility of the node that row names, so a
// node another write made and then marked - proposed, settled, overtaken, or reserved for its
// author - keeps what that write gave it, and one stored gone comes back live, as the writer
// whose row it is would bring it back. The roles and the sessions go through
// graphstore.WriteArms, whose upsert moves nothing on a node already there.
//
// Before it reads any row it records where it began reading the event log (recordBackfillHorizon):
// an id at or below which every event was final by then, so every event the run cannot have seen
// is above it, recorded as the horizon of this build's marking rule. A process of the release
// before this one goes on writing rows that name no node, and versions of documents this build's
// store has stated, after the run, and a build of this code of the same marking rule with no
// event cursor of its rule to start from starts reading the log at that id (docs'
// readEventsAfterCursor), so nothing that release writes between the run and that build's first
// block-index tick is passed over.
//
// It is no repair. Its input is each row's own content: it never reads one side of a
// projection to write the other, so a row whose node is already written is left alone and a
// kind whose rows disagree with the store is for the census to report, not for this to paper
// over.
func Backfill(ctx context.Context, database *store.Store, out io.Writer) (BackfillReport, error) {
	return backfillAt(ctx, database, out, refs.BlockIndexMarkingRule)
}

// backfillAt is Backfill as a build of the marking rule rule runs it: its marks and its horizon
// are that rule's. A test runs a backfill of another rule through it.
func backfillAt(ctx context.Context, database *store.Store, out io.Writer, rule int) (BackfillReport, error) {
	report := BackfillReport{}
	// The cause of a fact whose row's own event cannot be found is the store's sentinel, which
	// reads as "no event recorded this write" and is exactly true of a row written before the
	// store existed. The run appends no event of its own: events.Broker is the one writer of
	// that table (api's event-insert guard holds every other writer to it, because the broker
	// takes the transaction id before the insert draws the event's id, which the safe horizon
	// rests on), and the broker refuses an event that belongs to no issue, document, project or
	// session, which a database-wide backfill belongs to none of. A run after another reads
	// none of the rows that run marked, so it states none of their facts again; two runs at once
	// can state such a fact twice, where both read one comment before either marks it, since the
	// repeat keys skip the sentinel, and nothing reads wrong for it: a current read takes the
	// newest asserting tuple and the census counts nodes and projection rows rather than tuples.
	fmt.Fprintf(out, "backfill-graph-store: event=%d\n", graphstore.NoEvent)
	horizon, err := recordBackfillHorizon(ctx, database, rule)
	if err != nil {
		return report, err
	}
	fmt.Fprintf(out, "backfill-graph-store: horizon=%d\n", horizon)
	blocks := func(ctx context.Context, database *store.Store) (KindBackfill, error) {
		return backfillBlocks(ctx, database, rule)
	}
	for _, write := range []func(context.Context, *store.Store) (KindBackfill, error){
		blocks, backfillBlockRefs, backfillComments, backfillRuns, backfillRunFrames, backfillGoneRuns,
		backfillEndedPreventions, backfillRoles, backfillSessions,
	} {
		done, err := write(ctx, database)
		if err != nil {
			return report, err
		}
		report.Kinds = append(report.Kinds, done)
		writeKindBackfill(out, done)
	}
	// The project of every node, which a read limited to a project depends on: the node of each
	// issue, document, ask and message written before its kind's writer wrote one, and the
	// project of each comment and block node written before a node named its project. Both run
	// after the passes above, so a comment or a block those passes wrote a node for takes its
	// project from them, and the fill finds nothing of it to do. A row the object pass leaves
	// because the store holds its node in another project (graphstore.ErrProjectMoved), which no
	// second run cures, holds back neither the rest of that pass nor the fill: the run answers it
	// after both.
	var moved error
	for _, write := range []func(context.Context, *store.Store) ([]KindBackfill, error){
		backfillObjectNodes, backfillScopes,
	} {
		done, err := write(ctx, database)
		report.Kinds = append(report.Kinds, done...)
		for _, kind := range done {
			writeKindBackfill(out, kind)
		}
		switch {
		case onlyProjectMoved(err):
			moved = err
		case err != nil:
			return report, errors.Join(moved, err)
		}
	}
	// The asks and their followers, after the object pass above wrote the node of every ask: the
	// rest of the boot step's first phase (BackfillAsks), so a database the command fills reads
	// as one the boot fills. A row the object pass left in another project holds them back no
	// more than it holds back the fill, and the run answers it after them.
	for _, write := range askPasses {
		done, err := write(ctx, database)
		report.Kinds = append(report.Kinds, done)
		writeKindBackfill(out, done)
		if err != nil {
			return report, errors.Join(moved, err)
		}
	}
	return report, moved
}

// onlyProjectMoved is whether err holds failures and every one is graphstore.ErrProjectMoved:
// the rows backfillObjectNodes leaves while writing every other one.
func onlyProjectMoved(err error) bool {
	if err == nil {
		return false
	}
	joined, ok := err.(interface{ Unwrap() []error })
	if !ok {
		return errors.Is(err, graphstore.ErrProjectMoved)
	}
	for _, one := range joined.Unwrap() {
		if !errors.Is(one, graphstore.ErrProjectMoved) {
			return false
		}
	}
	return true
}

func writeKindBackfill(out io.Writer, done KindBackfill) {
	fmt.Fprintf(out, "backfill-graph-store: kind=%s rows=%d nodes=%d tuples=%d skipped=%d owed=%d remaining=%d complete=%t\n",
		done.Kind, done.Rows, done.Nodes, done.Tuples, done.Skipped, done.Owed, done.Remaining, done.Complete)
}

// The waits between two readings of the event log's writers (recordBackfillHorizon): 10 ms,
// doubling to 250 ms, the steps events.Horizon.Await re-observes at.
const (
	backfillHorizonWaitMin = 10 * time.Millisecond
	backfillHorizonWaitMax = 250 * time.Millisecond
)

// recordBackfillHorizon records in graph_backfill_horizon (migrations 20261118115645 and
// 20261120122600), before the run reads any row, an id of the event log at or below which every
// event is final, as the horizon of the marking rule rule, and answers it. It takes a mark of the
// log (events.Horizon.Mark) and waits until no transaction that can still write an event took its
// transaction id before the mark (events.Horizon.WriterFloor): from then every event at or below
// the newest one the mark saw has committed or rolled back for good, and an event that commits
// after the run's first read is above it. That is a proven safe horizon of the log. The Horizon's
// own proof (Safe, Await) is not taken: a fresh Horizon proves nothing from its first
// observation, and Await waits on every transaction id on the Postgres instance, an operator's
// open session and any transaction holding a row lock included, where WriterFloor waits only on
// the writers of the event log, which hold their transactions for one request.
//
// The table holds one row for each marking rule (refs.BlockIndexMarkingRule), and each run
// replaces the row of its own rule and leaves the row of every other rule as it was. Whatever the
// release before this one wrote before the newest run of a rule began, that run reads as it reads
// every other row: a row that names no node, and a document indexed past the version the store
// last stated it at (markStaleDocuments), and it marks what that rule owes. So a build of that
// rule need start reading the log no earlier than that run's horizon, and a build of another rule
// reads nothing from it: were one row shared, a backfill of an older rule run after a newer one
// would move the row, and the newer rule's first tick would pass over every event between the
// two horizons, which the older backfill did not mark for it.
func recordBackfillHorizon(ctx context.Context, database *store.Store, rule int) (int64, error) {
	horizon := events.NewHorizon(database.Pool)
	mark, err := horizon.Mark(ctx)
	if err != nil {
		return 0, err
	}
	for wait := backfillHorizonWaitMin; ; wait = min(2*wait, backfillHorizonWaitMax) {
		floor, final, err := horizon.WriterFloor(ctx, mark)
		if err != nil {
			return 0, err
		}
		if final {
			if _, err := database.Pool.Exec(ctx, `
				insert into graph_backfill_horizon (marking_rule, event_id) values ($1, $2)
				on conflict (marking_rule) do update set event_id = excluded.event_id, recorded_at = now()
			`, rule, floor); err != nil {
				return 0, fmt.Errorf("record the event log's horizon the backfill began at: %w", err)
			}
			return floor, nil
		}
		select {
		case <-ctx.Done():
			return 0, fmt.Errorf("wait for the event log's writers open when the backfill began: %w", ctx.Err())
		case <-time.After(wait):
		}
	}
}

// The backfill's three passes over the runs of the command bar. A process
// of the release before this one, during a roll or after a rollback, records a run with no
// interaction node, appends a frame with no frame value, and deletes a run an hour old, row and
// frames, leaving its node standing. Each pass finds what it owes by an anti-join of one side
// against the other, row by row, and not by a marker: going_requests and going_events carry no
// column naming a node, and a run is history, so nothing about a row says whether its node was
// written. Each writes in a transaction of its own per run, under the lock order the frame path
// takes (api's appendGoingEvent), and reads again under its locks whether the run still owes it,
// so a pass beside a live server, and a second run of the backfill, writes nothing it does not
// owe. A run an erase removed has neither row nor node, so no pass writes it back.
//
// Its input is each row's own content - a run's row and its frames - or, for the third, the
// absence of the row a node projects, which an older process's delete leaves: the store is never
// read to decide what a row should say, nor a row to mend the store.

// runRowHeld is true of an interaction node n whose going_requests row stands. Every writer of an
// interaction keys it by its run's uuid as Postgres writes it, and an id of any other shape is
// read as a run with no row rather than cast (store.CanonicalUUID), so no node can fail the read.
var runRowHeld = `exists (
		select 1 from going_requests r
		where r.id = ` + store.CanonicalUUID("n.id") + `)`

// runOwed is the from and the where of the runs whose row names no interaction node, which
// backfillRuns reads in batches and counts once it is done, $1 the interaction kind.
const runOwed = `
		from going_requests r
		where not exists (select 1 from graph_nodes n where n.kind = $1 and n.id = r.id::text)`

// runRowless is the from and the where of the interaction nodes, not gone, whose row is gone
// (runRowHeld), which backfillGoneRuns reads in batches and counts once it is done, $1 the
// interaction kind and $2 the gone state.
var runRowless = `
		from graph_nodes n
		where n.kind = $1 and n.state <> $2 and not ` + runRowHeld

// backfillRuns gives every run going_requests holds and the store holds no interaction node of its
// node, through the one writer of such a node from its row (graphstore.WriteRun): the node, its
// author's node and tuples, and a frame value of every frame its row holds, in the shape the going
// POST and the frame path write them. It reads the runs owed in batches by id, and writes each in
// a transaction of its own, which takes the run's row first and reads again whether the node is
// there, so a run the frame path or another backfill gave its node meanwhile is left as it is.
func backfillRuns(ctx context.Context, database *store.Store) (KindBackfill, error) {
	done := KindBackfill{Kind: graphstore.KindInteraction}
	after := uuid.Nil.String()
	for {
		owed, err := readOwedRuns(ctx, database, after, backfillBatch)
		if err != nil {
			return done, err
		}
		for _, id := range owed {
			written, err := writeOwedRun(ctx, database, id)
			if err != nil {
				return done, err
			}
			if written.Nodes > 0 {
				done.Rows++
				done.Nodes += written.Nodes
				done.Tuples += written.Tuples
			}
		}
		if len(owed) < backfillBatch {
			break
		}
		after = owed[len(owed)-1]
	}
	if err := database.Pool.QueryRow(ctx, `select count(*)`+runOwed, graphstore.KindInteraction).Scan(&done.Remaining); err != nil {
		return done, fmt.Errorf("count the runs the store holds no node of: %w", err)
	}
	done.Complete = done.Remaining == 0
	return done, nil
}

// readOwedRuns reads the runs past after, in id order, whose row names no interaction node, at
// most limit of them.
func readOwedRuns(ctx context.Context, database *store.Store, after string, limit int) ([]string, error) {
	rows, err := database.Pool.Query(ctx, `
		select r.id::text`+runOwed+` and r.id > $2::uuid
		order by r.id
		limit $3
	`, graphstore.KindInteraction, after, limit)
	if err != nil {
		return nil, fmt.Errorf("read the runs the store holds no node of: %w", err)
	}
	owed, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, fmt.Errorf("read the runs the store holds no node of: %w", err)
	}
	return owed, nil
}

// writeOwedRun writes the node of one run in a transaction of its own (graphstore.WriteRun).
func writeOwedRun(ctx context.Context, database *store.Store, id string) (graphstore.RunWritten, error) {
	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		return graphstore.RunWritten{}, fmt.Errorf("begin the node of run %s: %w", id, err)
	}
	defer tx.Rollback(ctx)
	written, err := graphstore.WriteRun(ctx, tx, id)
	if err != nil {
		return graphstore.RunWritten{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return graphstore.RunWritten{}, fmt.Errorf("commit the node of run %s: %w", id, err)
	}
	return written, nil
}

// backfillRunFrames states a frame value of every going_events row whose run's interaction holds
// no frame value of that sequence (graphstore.FrameHeldSQL): a frame a process of the release
// before appended to a run this build or the passes before gave a node. Each owed run is written in
// a transaction of its own (writeRunFrames). Rows counts the frames it stated and Tuples the
// values, one each, with what graphstore.WriteRun stated of a run that had no node by then.
func backfillRunFrames(ctx context.Context, database *store.Store) (KindBackfill, error) {
	done := KindBackfill{Kind: graphstore.AttributeFrame}
	after := uuid.Nil.String()
	for {
		owed, err := readOwedFrameRuns(ctx, database, after, backfillBatch)
		if err != nil {
			return done, err
		}
		for _, run := range owed {
			frames, written, err := writeRunFrames(ctx, database, run)
			if err != nil {
				return done, err
			}
			done.Rows += frames
			done.Tuples += frames + written.Tuples
			done.Nodes += written.Nodes
		}
		if len(owed) < backfillBatch {
			break
		}
		after = owed[len(owed)-1]
	}
	var left int64
	if err := database.Pool.QueryRow(ctx, `select count(*) from going_events e where not `+graphstore.FrameHeldSQL).Scan(&left); err != nil {
		return done, fmt.Errorf("count the frames the store holds no value of: %w", err)
	}
	done.Remaining = left
	done.Complete = left == 0
	return done, nil
}

// readOwedFrameRuns reads the runs past after, in id order, that hold a frame the store holds no
// value of, at most limit of them. The runs are grouped and ordered by the uuid itself, the order
// `after` compares in, so the read takes going_events' (request_id, sequence) key in its order.
func readOwedFrameRuns(ctx context.Context, database *store.Store, after string, limit int) ([]string, error) {
	rows, err := database.Pool.Query(ctx, `
		select e.request_id::text from going_events e
		where e.request_id > $1::uuid and not `+graphstore.FrameHeldSQL+`
		group by e.request_id
		order by e.request_id
		limit $2
	`, after, limit)
	if err != nil {
		return nil, fmt.Errorf("read the runs holding a frame the store holds no value of: %w", err)
	}
	owed, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, fmt.Errorf("read the runs holding a frame the store holds no value of: %w", err)
	}
	return owed, nil
}

// writeRunFrames states, in one transaction, a frame value of each frame of run the store holds no
// value of, and answers how many. It takes the run's interaction node row first
// (graphstore.LockInteraction), as every frame of this build does (api's appendGoingEvent), so a
// frame this build is appending to the run commits first, value and row together, and the read
// under the lock no longer counts it owed. A run with no node by then gets it from its row through
// graphstore.WriteRun, which states every frame the row holds, and which it answers. The values are
// read off the rows (graphstore.ReadRunFrames), exactly as the frame path states them.
func writeRunFrames(ctx context.Context, database *store.Store, run string) (int64, graphstore.RunWritten, error) {
	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		return 0, graphstore.RunWritten{}, fmt.Errorf("begin the frames of run %s: %w", run, err)
	}
	defer tx.Rollback(ctx)
	locked, err := graphstore.LockInteraction(ctx, tx, run)
	if err != nil {
		return 0, graphstore.RunWritten{}, err
	}
	if !locked {
		written, err := graphstore.WriteRun(ctx, tx, run)
		if err != nil {
			return 0, graphstore.RunWritten{}, err
		}
		if err := tx.Commit(ctx); err != nil {
			return 0, graphstore.RunWritten{}, fmt.Errorf("commit the node of run %s: %w", run, err)
		}
		return 0, written, nil
	}
	frames, err := graphstore.ReadRunFrames(ctx, tx, run, true)
	if err != nil {
		return 0, graphstore.RunWritten{}, err
	}
	if len(frames) == 0 {
		return 0, graphstore.RunWritten{}, nil
	}
	if err := graphstore.Write(ctx, tx, nil, frames); err != nil {
		return 0, graphstore.RunWritten{}, fmt.Errorf("state the frames of run %s: %w", run, err)
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, graphstore.RunWritten{}, fmt.Errorf("commit the frames of run %s: %w", run, err)
	}
	return int64(len(frames)), graphstore.RunWritten{}, nil
}

// backfillGoneRuns states gone every interaction node, not gone already, whose going_requests row
// is gone (runRowHeld): a run a process of the release before deleted an hour old, frames with it,
// whose node it left standing. The store keeps the node, as it keeps every node an erase has not
// removed, and the census reads a gone node with no row as no difference. A run an erase removed
// has no node either, so this states nothing of it.
func backfillGoneRuns(ctx context.Context, database *store.Store) (KindBackfill, error) {
	done := KindBackfill{Kind: "interaction_gone"}
	after := ""
	for {
		owed, err := readRowlessRuns(ctx, database, after, backfillBatch)
		if err != nil {
			return done, err
		}
		for _, id := range owed {
			stated, err := writeGoneRun(ctx, database, id)
			if err != nil {
				return done, err
			}
			if stated {
				done.Rows++
				done.Tuples++
			}
		}
		if len(owed) < backfillBatch {
			break
		}
		after = owed[len(owed)-1]
	}
	if err := database.Pool.QueryRow(ctx, `select count(*)`+runRowless, graphstore.KindInteraction, graphstore.StateGone).Scan(&done.Remaining); err != nil {
		return done, fmt.Errorf("count the runs whose row is gone: %w", err)
	}
	done.Complete = done.Remaining == 0
	return done, nil
}

// readRowlessRuns reads the interaction nodes past after, in id order, not gone and whose row is
// gone, at most limit of them.
func readRowlessRuns(ctx context.Context, database *store.Store, after string, limit int) ([]string, error) {
	rows, err := database.Pool.Query(ctx, `
		select n.id`+runRowless+` and n.id > $3
		order by n.id
		limit $4
	`, graphstore.KindInteraction, graphstore.StateGone, after, limit)
	if err != nil {
		return nil, fmt.Errorf("read the runs whose row is gone: %w", err)
	}
	owed, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, fmt.Errorf("read the runs whose row is gone: %w", err)
	}
	return owed, nil
}

// writeGoneRun states the interaction node id gone where its row is still gone, in a transaction
// of its own, and answers whether it did. It takes the node's row first, as a frame and an erase
// of the run do, then reads under that lock the node and whether its row stands: the going POST
// commits a run's node and its row together, so a node it reads has its row whenever the run has
// one, and a node an erase removed meanwhile is read as none. The write is the node as the store
// holds it but for its state, a plain write that moves the row and appends the state tuple, made
// by the run (backfillActor) now, as api's writeNodeState moves a node's state.
func writeGoneRun(ctx context.Context, database *store.Store, id string) (bool, error) {
	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		return false, fmt.Errorf("begin the end of run %s: %w", id, err)
	}
	defer tx.Rollback(ctx)
	if _, err := graphstore.LockInteraction(ctx, tx, id); err != nil {
		return false, err
	}
	node, held, err := graphstore.ReadNode(ctx, tx, graphstore.KindInteraction, id)
	if err != nil || !held || node.State == graphstore.StateGone {
		return false, err
	}
	var rowHeld bool
	if err := tx.QueryRow(ctx, `select `+runRowHeld+` from graph_nodes n where n.kind = $1 and n.id = $2`,
		graphstore.KindInteraction, id).Scan(&rowHeld); err != nil {
		return false, fmt.Errorf("read whether run %s holds its row: %w", id, err)
	}
	if rowHeld {
		return false, nil
	}
	node.State, node.Restates = graphstore.StateGone, false
	node.MadeAt, node.MadeBy, node.SourceEvent = time.Now().UTC(), backfillActor, graphstore.NoEvent
	if err := graphstore.Write(ctx, tx, []graphstore.Node{node}, nil); err != nil {
		return false, fmt.Errorf("state run %s gone: %w", id, err)
	}
	if err := tx.Commit(ctx); err != nil {
		return false, fmt.Errorf("commit the end of run %s: %w", id, err)
	}
	return true, nil
}

// The backfill's pass over the prevented_by tuples of the checks nobody runs any more. This build
// retracts each prevented_by at a check block a version drops (refs' retractGonePointers) and at
// each check of a document it retires (docs' retiredPreventions), in the transaction that drops or
// retires it. A process of the release before, during a roll or after a rollback, does neither: it
// states the block gone, or retires the document, and leaves each prevented_by at it asserted, so a
// risk reads as prevented by a check of a retired document, and a check a later version restores
// brings its old prevented_by back with it. The pass finds each such tuple by the store's own state
// of the check and its document's row, not by a marker, and retracts it; a second run finds none.

// endedChecks is an SQL array of the ended checks a risk asserted a prevented_by at: each far end of
// such a tuple whose block the store holds gone, which a version dropped, or whose document is
// retired (artifacts.retired_at), whose checks nobody runs. A block's node id names its document's
// ref_key before its first '#', since a ref_key holds none. narrow is a predicate over p, the
// prevented_by tuple, that bounds the checks read. It starts from the risks, through
// graph_tuples_current, since risks are few and a read into blocks would walk every block's tuples.
func endedChecks(narrow string) string {
	return `array(
		select distinct p.to_id from graph_tuples p
		left join graph_nodes b on b.kind = p.to_kind and b.id = p.to_id
		left join artifacts a on a.ref_key = split_part(p.to_id, '#', 1)
		where p.from_kind = '` + graphstore.KindRisk + `' and p.kind = '` + graphstore.TuplePreventedBy + `'
		  and p.to_kind = '` + refs.BlockNodeKind + `' and p.asserted
		  and (b.state = '` + graphstore.StateGone + `' or a.retired_at is not null)
		  and ` + narrow + `)`
}

// backfillEndedPreventions retracts every prevented_by the store asserts now at an ended check
// (endedChecks), through the read the index pass and the retirement retract by
// (refs.PreventionRetractions). It reads the documents that hold such a check in batches by
// ref_key and retracts each batch's in a transaction of its own (writeEndedPreventions). Rows and
// Tuples count the tuples it retracted.
func backfillEndedPreventions(ctx context.Context, database *store.Store) (KindBackfill, error) {
	done := KindBackfill{Kind: "prevented_by_retracted"}
	after := ""
	for {
		owed, err := readEndedPreventionDocuments(ctx, database, after, backfillBatch)
		if err != nil {
			return done, err
		}
		if len(owed) > 0 {
			retracted, err := writeEndedPreventions(ctx, database, owed)
			if err != nil {
				return done, err
			}
			done.Rows += retracted
			done.Tuples += retracted
		}
		if len(owed) < backfillBatch {
			break
		}
		after = owed[len(owed)-1]
	}
	if err := database.Pool.QueryRow(ctx, refs.CurrentPreventionsSQL(`count(*)`, endedChecks(`true`))).Scan(&done.Remaining); err != nil {
		return done, fmt.Errorf("count the prevented_by tuples at checks nobody runs: %w", err)
	}
	done.Complete = done.Remaining == 0
	return done, nil
}

// readEndedPreventionDocuments reads the ref_keys past after, in order, of the documents that hold
// an ended check a risk is prevented by now, at most limit of them.
func readEndedPreventionDocuments(ctx context.Context, database *store.Store, after string, limit int) ([]string, error) {
	rows, err := database.Pool.Query(ctx, `
		`+refs.CurrentPreventionsSQL(`distinct split_part(checked.to_id, '#', 1)`, endedChecks(`split_part(p.to_id, '#', 1) > $1`))+`
		order by 1
		limit $2
	`, after, limit)
	if err != nil {
		return nil, fmt.Errorf("read the documents holding a check nobody runs that a risk is prevented by: %w", err)
	}
	owed, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, fmt.Errorf("read the documents holding a check nobody runs that a risk is prevented by: %w", err)
	}
	return owed, nil
}

// writeEndedPreventions retracts, in one transaction, every prevented_by the store asserts now at an
// ended check of documents, a batch's ref_keys, and answers how many. It takes the owner lock
// (store.LockOwner) of each of the documents that has a row first, each once and in ascending
// owner key (lockDocumentOwners): the lock the index pass that drops a check and the retirement of
// its document hold while they retract, and which api's lockReasoningOwners takes before it
// appends a prevented_by at a check, so such a write beside the pass commits first and the read
// under the lock sees it. It then reads the tuples again under that lock and retracts each, dated
// now and made by the run (backfillActor), caused by no event, as writeGoneRun moves a run's node.
func writeEndedPreventions(ctx context.Context, database *store.Store, documents []string) (int64, error) {
	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("begin the retraction of the prevented_by tuples at checks nobody runs: %w", err)
	}
	defer tx.Rollback(ctx)
	if err := lockDocumentOwners(ctx, tx, `ref_key = any($1)`, documents); err != nil {
		return 0, err
	}
	retracted, err := refs.PreventionRetractions(ctx, tx, endedChecks(`split_part(p.to_id, '#', 1) = any($1)`),
		[]any{documents}, time.Now().UTC(), backfillActor, graphstore.NoEvent)
	if err != nil {
		return 0, fmt.Errorf("read the prevented_by tuples at checks nobody runs: %w", err)
	}
	if len(retracted) == 0 {
		return 0, nil
	}
	if err := graphstore.Write(ctx, tx, nil, retracted); err != nil {
		return 0, fmt.Errorf("retract the prevented_by tuples at checks nobody runs: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("commit the retraction of the prevented_by tuples at checks nobody runs: %w", err)
	}
	return int64(len(retracted)), nil
}
