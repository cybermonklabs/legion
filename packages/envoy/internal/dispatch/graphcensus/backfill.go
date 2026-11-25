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
// then gives every role the store census holds to a node and that has none its node and its page
// (backfillRoles), since the publisher writes those only for the events it publishes from then on,
// and every session the census holds to a node and that has none its node (backfillSessions): a
// session that only asked, followed or was reached before the store existed was recorded by no
// write of the store, and the followed_by edge to it would otherwise answer no node.
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
// node another write made and then marked - a later release's proposed requirement, or a node it
// reserved for its author - keeps what that write gave it, and one stored gone comes back live,
// as the writer whose row it is would bring it back. The roles and the sessions go through
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
	report, moved := backfillBeforeAskNodesAt(ctx, database, out, rule)
	if moved != nil && !onlyProjectMoved(moved) {
		return report, moved
	}
	// The asks and their followers run after the object pass has written the ask nodes.
	// A row that pass left in another project does not hold back these passes.
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

// BackfillBeforeAskNodes runs the passes of backfill-graph-store that precede the ask node
// migration. The migration census uses it to reproduce a store already filled by the earlier
// release, before it judges the migrations still pending on that store.
func BackfillBeforeAskNodes(ctx context.Context, database *store.Store, out io.Writer) (BackfillReport, error) {
	return backfillBeforeAskNodesAt(ctx, database, out, refs.BlockIndexMarkingRule)
}

func backfillBeforeAskNodesAt(ctx context.Context, database *store.Store, out io.Writer, rule int) (BackfillReport, error) {
	report := BackfillReport{}
	if err := requireBackfillIndexes(ctx, database); err != nil {
		return report, err
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
	horizon, err := recordBackfillHorizon(ctx, database, rule)
	if err != nil {
		return report, err
	}
	fmt.Fprintf(out, "backfill-graph-store: horizon=%d\n", horizon)
	if err := analyzeWalkedColumns(ctx, database); err != nil {
		return report, err
	}
	blocks := func(ctx context.Context, database *store.Store) (KindBackfill, error) {
		return backfillBlocks(ctx, database, rule)
	}
	for _, write := range []func(context.Context, *store.Store) (KindBackfill, error){
		blocks, backfillBlockRefs, backfillComments, backfillRoles, backfillSessions,
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
// own proof (Safe, Await) is not taken: Safe answers no H from a fresh Horizon's first observation
// while an event writer older than its observer is open, and Await waits for an observation of
// its own to be proven, where WriterFloor answers from the mark already taken. Both wait on the
// same writers, the event writes of this database, which hold their transactions for one request.
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

// walkedColumnsAnalysis is the statement that gives the planner statistics of the column each walk
// of the backfill reads its rows by, node_kind of the three projected tables.
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
const walkedColumnsAnalysis = `analyze document_blocks (node_kind), block_refs (node_kind), comments (node_kind)`

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
