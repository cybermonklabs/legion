package graphcensus

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

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
	// Owed is how many documents it marked owed this release's block index (store_owed_at), for
	// the block kind alone: those whose rows it named the nodes of, and those no pass of this
	// build's rule had stated at their version (markStaleDocuments).
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
// node stated gone because its row is missing would be a repair. And a build of an earlier rule of
// the block index states none of this build's facts of each block of a version it indexes - its
// rank, its text, its placement, its place in the text (its text_parent and its text_rank), its
// size, the supertag of its type - though it writes store_version for that version as this build's
// pass does. So the run marks every document no pass of this build's rule has stated at its
// version (markStaleDocuments), by the record of that pass the document's row holds
// (stated_version, stated_rule) and by no row of either side, and
// the reconciliation's index pass over the latest version then states what each block owes,
// states each block the version dropped gone and retracts each link it dropped, from the
// version's render (refs.IndexBlocks). No release deletes a comment's row (writeCommentBatch).
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
// Before it reads any row it records where it began reading the event log (StartBackfill,
// recordBackfillHorizon): an id at or below which every event was final by then, so every event the
// run cannot have seen is above it, recorded as the horizon of this build's marking rule. A process
// of the release before this one goes on writing rows that name no node, and versions of documents
// this build's store has stated, after the run, and a build of this code of the same marking rule
// with no event cursor of its rule to start from starts reading the log at that id (docs'
// readEventsAfterCursor), so nothing that release writes between the run and that build's first
// block-index tick is passed over. Backfill waits for that record as long as the writers of the
// event log stay open; a boot of the server records it within a bound and runs the passes apart
// (StartBackfill, BackfillRun.Passes).
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
	run, err := startBackfillAt(ctx, database, out, 0, rule)
	if err != nil {
		return BackfillReport{}, err
	}
	return run.Passes(ctx, out)
}

// OnlyProjectMoved answers whether err holds failures and every one is graphstore.ErrProjectMoved
// (only), the rows backfillObjectNodes leaves while writing every other one, and how many it holds
// (moved). The failures are the members of err's join, or err itself where it joins none. A join
// that holds another failure beside them, errors.Join(moved, err) as Passes answers a pass that
// failed after such rows, is not only that, though errors.Is reads ErrProjectMoved in it.
func OnlyProjectMoved(err error) (moved int, only bool) {
	if err == nil {
		return 0, false
	}
	joined, ok := err.(interface{ Unwrap() []error })
	if !ok {
		if errors.Is(err, graphstore.ErrProjectMoved) {
			return 1, true
		}
		return 0, false
	}
	for _, one := range joined.Unwrap() {
		if !errors.Is(one, graphstore.ErrProjectMoved) {
			return 0, false
		}
		moved++
	}
	return moved, moved > 0
}

func writeKindBackfill(out io.Writer, done KindBackfill) {
	fmt.Fprintf(out, "backfill-graph-store: kind=%s rows=%d nodes=%d tuples=%d skipped=%d owed=%d remaining=%d complete=%t\n",
		done.Kind, done.Rows, done.Nodes, done.Tuples, done.Skipped, done.Owed, done.Remaining, done.Complete)
}

// The waits between two readings of the event log's writers (recordBackfillHorizon): 10 ms,
// doubling to 250 ms, the steps events.Horizon.Await re-observes at. BackfillHorizonWaitBound is the
// longest a boot of the server waits for the record (StartBackfill's bound), so no writer of the
// event log left open holds a boot's readiness longer than that.
const (
	backfillHorizonWaitMin   = 10 * time.Millisecond
	backfillHorizonWaitMax   = 250 * time.Millisecond
	BackfillHorizonWaitBound = 30 * time.Second
)

// recordBackfillHorizon records in graph_backfill_horizon (migrations 20261118115645 and
// 20261120122600), before the run reads any row, an id of the event log at or below which every
// event is final, as the horizon of the marking rule rule, and answers it. It takes a mark of the
// log (events.Horizon.Mark) and waits until no transaction that can still write an event took its
// transaction id before the mark (events.Horizon.WriterFloor): from then every event at or below
// the newest one the mark saw has committed or rolled back for good, and an event that commits
// after the run's first read is above it. That is a proven safe horizon of the log. The Horizon's
// own proof (Safe, Await) is not taken: Safe answers no H from a fresh Horizon's first observation
// while an event writer older than its observer is open, and Await waits for an observation of
// its own to be proven, where WriterFloor answers from the mark already taken. Both wait on the
// same writers, the event writes of this database, which hold their transactions for one request.
//
// The table holds one row for each marking rule (refs.BlockIndexMarkingRule), and each run
// replaces the row of its own rule and leaves the row of every other rule as it was. Whatever the
// release before this one wrote before the newest run of a rule began, that run reads as it reads
// every other row: a row that names no node, and a document no pass of that rule has stated at its
// version (markStaleDocuments), and it marks what that rule owes. So a build of that rule need
// start reading the log no earlier than that run's horizon, and a build of another rule reads
// nothing from it: were one row shared, a backfill of an older rule run after a newer one would
// move the row, and the newer rule's first tick would pass over every event between the two
// horizons, which the older backfill did not mark for it.
//
// A bound above 0 ends the wait, the mark's and the readings', once it has passed: the record then
// names the oldest writer of the event log, the transaction it waited on (events.OldestEventWriter),
// in a HorizonBoundError, and records nothing.
func recordBackfillHorizon(ctx context.Context, database *store.Store, bound time.Duration, rule int) (int64, error) {
	waiting, stop := ctx, context.CancelFunc(func() {})
	if bound > 0 {
		waiting, stop = context.WithTimeout(ctx, bound)
	}
	defer stop()
	horizon := events.NewHorizon(database.Pool)
	mark, err := horizon.Mark(waiting)
	if err != nil {
		return 0, horizonWaitEnded(ctx, waiting, database, bound, err)
	}
	for wait := backfillHorizonWaitMin; ; wait = min(2*wait, backfillHorizonWaitMax) {
		floor, final, err := horizon.WriterFloor(waiting, mark)
		if err != nil {
			return 0, horizonWaitEnded(ctx, waiting, database, bound, err)
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
		case <-waiting.Done():
			return 0, horizonWaitEnded(ctx, waiting, database, bound, waiting.Err())
		case <-time.After(wait):
		}
	}
}

// backfillPass is one pass of the backfill: it writes what the rows of one kind or more owe the
// store and reports what it did for each kind. A pass that fails reports what it did before the
// failure, if it read that far, beside its error.
type backfillPass func(context.Context, *store.Store) ([]KindBackfill, error)

// oneKind is the backfillPass of a pass over one kind, which reports no kind when it fails.
func oneKind(write func(context.Context, *store.Store) (KindBackfill, error)) backfillPass {
	return func(ctx context.Context, database *store.Store) ([]KindBackfill, error) {
		done, err := write(ctx, database)
		if err != nil {
			return nil, err
		}
		return []KindBackfill{done}, nil
	}
}

// BackfillRun is a run of the backfill whose horizon StartBackfill has recorded, its passes still
// to run (Passes), and the marking rule its marks and its horizon are of.
type BackfillRun struct {
	database *store.Store
	rule     int
}

// StartBackfill begins a run of the backfill (Backfill) over database: it refuses the run, before it
// writes anything, while any of BackfillIndexes is absent or invalid (requireBackfillIndexes), then
// prints the run's event line to out, records the run's horizon of the event log
// (recordBackfillHorizon) and prints it, and answers the run, whose passes the caller runs next.
// bound is how long the record waits on the writers of the event log, 0 for as long as they stay
// open; past it the record answers a HorizonBoundError and the run is not begun. The run is of this
// build's marking rule (refs.BlockIndexMarkingRule).
func StartBackfill(ctx context.Context, database *store.Store, out io.Writer, bound time.Duration) (BackfillRun, error) {
	return startBackfillAt(ctx, database, out, bound, refs.BlockIndexMarkingRule)
}

// startBackfillAt is StartBackfill as a build of the marking rule rule begins it (backfillAt).
func startBackfillAt(ctx context.Context, database *store.Store, out io.Writer, bound time.Duration, rule int) (BackfillRun, error) {
	if err := requireBackfillIndexes(ctx, database); err != nil {
		return BackfillRun{}, err
	}
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
	horizon, err := recordBackfillHorizon(ctx, database, bound, rule)
	if err != nil {
		return BackfillRun{}, err
	}
	fmt.Fprintf(out, "backfill-graph-store: horizon=%d\n", horizon)
	return BackfillRun{database: database, rule: rule}, nil
}

// Passes runs the run's passes, in the order backfillPasses gives them, printing what each did for
// each kind to out, and answers what they did. Before the first it gives the planner statistics of
// the columns the walks read their rows by (analyzeWalkedColumns), on the passes' own path, so a
// boot, which runs them on a goroutine of their own, waits on no analysis.
func (run BackfillRun) Passes(ctx context.Context, out io.Writer) (BackfillReport, error) {
	report := BackfillReport{}
	if err := analyzeWalkedColumns(ctx, run.database); err != nil {
		return report, err
	}
	// The passes run in the order backfillPasses gives them, which says what that order rests on.
	// A row the object pass leaves because the store holds its node in another project
	// (graphstore.ErrProjectMoved), which no second run cures, holds back none of the passes after
	// it - the rest of that pass, the fill, the asks and their followers, and every later topic's:
	// the run answers it after them all.
	var moved error
	for _, write := range backfillPasses(run.rule) {
		done, err := write(ctx, run.database)
		report.Kinds = append(report.Kinds, done...)
		for _, kind := range done {
			writeKindBackfill(out, kind)
		}
		if _, only := OnlyProjectMoved(err); only {
			moved = err
		} else if err != nil {
			return report, errors.Join(moved, err)
		}
	}
	return report, moved
}

// HorizonBoundError is the end of a record of the backfill's horizon at its bound: the oldest
// writer of the event log, the transaction the record waited on, was still open. Pid is its backend,
// 0 for a prepared transaction, and Age how long its transaction had been open. It wraps
// context.DeadlineExceeded.
type HorizonBoundError struct {
	Pid int32
	Age time.Duration
}

// NewHorizonBoundError is the record's error at its bound over the oldest writer's read
// (events.OldestEventWriter): the writer's pid, and its age at now from since, when its transaction
// began. A since Postgres did not show is the zero time, and its age 0.
func NewHorizonBoundError(pid int32, since, now time.Time) *HorizonBoundError {
	bound := &HorizonBoundError{Pid: pid}
	if !since.IsZero() {
		bound.Age = now.Sub(since)
	}
	return bound
}

func (e *HorizonBoundError) Error() string {
	writer := fmt.Sprintf("backend pid %d", e.Pid)
	if e.Pid == 0 {
		writer = "a prepared transaction"
	}
	return fmt.Sprintf("the event log's writers held the backfill's horizon past its bound: the oldest, %s, open %s: %v",
		writer, e.Age, context.DeadlineExceeded)
}

func (e *HorizonBoundError) Unwrap() error {
	return context.DeadlineExceeded
}

// horizonWaitEnded is what recordBackfillHorizon answers where err ended its wait on the writers of
// the event log: the caller's own end of ctx, err itself where waiting, the wait's context, has not
// ended, and the end of the bound, waiting's deadline, as a HorizonBoundError naming the oldest
// writer of the event log then. A bound whose writer has ended by that read, as one can at the very
// bound, is an error that names none.
func horizonWaitEnded(ctx, waiting context.Context, database *store.Store, bound time.Duration, err error) error {
	switch {
	case ctx.Err() != nil:
		return fmt.Errorf("wait for the event log's writers open when the backfill began: %w", ctx.Err())
	case waiting.Err() == nil:
		return err
	}
	pid, since, err := events.OldestEventWriter(ctx, database.Pool)
	switch {
	case errors.Is(err, events.ErrNoEventWriter):
		return fmt.Errorf("the event log's writers held the backfill's horizon past its bound of %s, and none is open now: %w", bound, context.DeadlineExceeded)
	case err != nil:
		return fmt.Errorf("name the writer of the event log that held the backfill's horizon past its bound of %s: %w", bound, err)
	}
	return NewHorizonBoundError(pid, since, time.Now())
}

// walkedColumnsAnalysis is the statement that gives the planner statistics of the column each walk
// of the backfill reads its rows by, node_kind of the three projected tables and of the four tables
// and the documents the topic passes walk.
//
// Migration 20261109022248 adds those columns with the first release that carries this backfill,
// after any analysis of the tables, so until autovacuum next analyzes a table the planner holds no
// statistics of its column and takes `node_kind is null` to match a fraction of a percent of the
// rows. Where that guess is under one batch, a walk's plan reads every row still naming no node
// through a bitmap and sorts them, once per batch, rather than reading one batch in order from the
// cursor through the index that keeps it (document_blocks_naming_no_node,
// block_refs_naming_no_node, comments_pkey): measured on a seeded copy of the schema the deploy
// leaves, every comment batch read all 28,338 comments naming no node, and every block batch of a
// tenth of that copy's blocks read all 27,405 of them. The statement samples each table's pages
// as any analysis does, whatever the table's size, takes SHARE UPDATE EXCLUSIVE, which no read or
// write waits on, and writes only the statistics catalog.
//
// Migration 20261116092210_issue_tree_markers adds the same columns to issue_links, components,
// component_depends, issue_component_members and artifacts, after any analysis of them too. With
// no statistics of them, over a seed of 20,000 rows owed to each walk, a batch read from the
// middle of the walk read every row past its cursor: the issue links' and the issue components'
// walks 9,999 rows of their tables under a custom plan, the component dependencies' walk 9,999
// under a generic one, and the orphaned references' walk 10,001 documents under a custom plan,
// where each reads one batch once the columns hold statistics
// (TestEveryTopicPassWalksOneBatchThroughAnIndex).
const walkedColumnsAnalysis = `analyze document_blocks (node_kind), block_refs (node_kind), comments (node_kind),
	issue_links (node_kind), components (node_kind), component_depends (node_kind), issue_component_members (node_kind),
	artifacts (node_kind)`

// analyzeWalkedColumns runs walkedColumnsAnalysis.
func analyzeWalkedColumns(ctx context.Context, database *store.Store) error {
	if _, err := database.Pool.Exec(ctx, walkedColumnsAnalysis); err != nil {
		return fmt.Errorf("analyze the columns the walks read their rows by: %w", err)
	}
	return nil
}

// BackfillIndex is one index the backfill's reads stand on, and the version of the migration that
// builds it.
type BackfillIndex struct {
	Name    string
	Version int
}

// BackfillIndexes are the indexes the backfill's reads stand on (migrations 20261119020700 to
// 20261119021000): the two its batches probe events through for each row's event
// (BlockRowsStatement, CommentRowsStatement), and the two its walks read one batch through in
// their order (BlockRowsStatement, BlockRefRowsStatement). Without any one of them, each batch of
// that read reads a whole table, and the run takes time growing with the square of the data: on a
// seeded database of 270,539 blocks and 28,338 comments the block and comment passes took 31
// minutes without them.
var BackfillIndexes = []BackfillIndex{
	{Name: "events_artifact_version_number", Version: 20261119020700},
	{Name: "events_comment_created_id", Version: 20261119020800},
	{Name: "document_blocks_naming_no_node", Version: 20261119020900},
	{Name: "block_refs_naming_no_node", Version: 20261119021000},
}

// backfillIndexesNotUsable is the read requireBackfillIndexes makes: the place in $1, counted from
// 1, of each of the indexes $1 names that the schema the runner made the tables in does not hold as
// a valid index, one the planner may read, in their order; whether an index of that name stands
// there invalid, as a concurrent build that failed leaves it; and that schema.
const backfillIndexesNotUsable = `
	select wanted.place::int, i.indexrelid is not null, current_schema()
	from unnest($1::text[]) with ordinality as wanted (name, place)
	left join pg_index i
		on i.indexrelid = to_regclass(quote_ident(current_schema()) || '.' || quote_ident(wanted.name))
	where i.indisvalid is not true
	order by wanted.place`

// requireBackfillIndexes refuses the run, naming each, while any of BackfillIndexes is absent or
// invalid: the run would then read a whole table for each batch, for longer the more rows the
// deployment holds, so a refusal before it writes anything costs less than a run that may not end.
//
// The command migrates the database before it runs this, so every index's version is recorded by
// then, and the runner builds a recorded version no more: an index that is absent or invalid here
// went so after its migration was recorded (its down file run by hand, or a concurrent build by
// hand that failed), and no boot builds it again. So the refusal names the remedy that works in
// that state, the build by hand (backfillIndexRemedy).
func requireBackfillIndexes(ctx context.Context, database *store.Store) error {
	names := make([]string, len(BackfillIndexes))
	for i, index := range BackfillIndexes {
		names[i] = index.Name
	}
	rows, err := database.Pool.Query(ctx, backfillIndexesNotUsable, names)
	if err != nil {
		return fmt.Errorf("read whether the indexes the backfill reads through are valid: %w", err)
	}
	type unusable struct {
		place    int
		invalid  bool
		inSchema string
	}
	found, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (unusable, error) {
		var u unusable
		err := row.Scan(&u.place, &u.invalid, &u.inSchema)
		return u, err
	})
	if err != nil {
		return fmt.Errorf("read whether the indexes the backfill reads through are valid: %w", err)
	}
	if len(found) == 0 {
		return nil
	}
	missing := make([]string, 0, len(found))
	for _, u := range found {
		missing = append(missing, BackfillIndexes[u.place-1].Name)
	}
	statements := []string{"set search_path to " + pgx.Identifier{found[0].inSchema}.Sanitize() + ";"}
	for _, u := range found {
		build, err := backfillIndexRemedy(BackfillIndexes[u.place-1], u.invalid)
		if err != nil {
			return fmt.Errorf("refused: the index(es) %s are absent or invalid, and the statements that build them again cannot be read: %w", strings.Join(missing, ", "), err)
		}
		statements = append(statements, build...)
	}
	return fmt.Errorf("refused: the index(es) %s are absent or invalid, and without them each batch reads a whole table. "+
		"Their migrations' versions are recorded, so no boot builds them again: build them by hand with the statements below, "+
		"each run on its own outside a transaction (as psql runs them, one per line, in its default autocommit), then run this again:\n  %s",
		strings.Join(missing, ", "), strings.Join(statements, "\n  "))
}

// backfillIndexRemedy is the statements that build index again by hand: a drop of the index
// where one of its name stands invalid, which its migration's create ... if not exists would
// otherwise leave as it is, and then the migration's own statement, read from the migration the
// runner applies (store.MigrationText) with its comments left out and its words joined by single
// spaces, so it prints on one line.
func backfillIndexRemedy(index BackfillIndex, invalid bool) ([]string, error) {
	text, err := store.MigrationText(index.Version)
	if err != nil {
		return nil, err
	}
	var code []string
	for _, line := range strings.Split(text, "\n") {
		if trimmed := strings.TrimSpace(line); trimmed != "" && !strings.HasPrefix(trimmed, "--") {
			code = append(code, trimmed)
		}
	}
	build := strings.Join(strings.Fields(strings.Join(code, " ")), " ")
	if !strings.HasPrefix(build, "create index concurrently if not exists "+index.Name+" ") || !strings.HasSuffix(build, ";") ||
		strings.Count(build, ";") != 1 {
		return nil, fmt.Errorf("migration %d is not one statement building %s: %q", index.Version, index.Name, build)
	}
	if invalid {
		return []string{"drop index concurrently if exists " + pgx.Identifier{index.Name}.Sanitize() + ";", build}, nil
	}
	return []string{build}, nil
}
