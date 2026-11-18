package graphcensus

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"slices"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/asks"
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
	// the block kind and the drain of every stored document's block index (drainBlockIndexes)
	// alone: for the block kind, those whose rows it named the nodes of and those another release
	// indexed after the store last stated them (markStaleDocuments).
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

// Backfill writes the nodes and tuples of the comments, document blocks and block references
// written before the node and tuple store existed: every row of the three projected tables
// that names no node (migration 20261109022248 added the columns and filled none of them). It
// then mends what a process of the release before writes of a run of the command bar during a
// roll or after a rollback (backfillRuns, backfillRunFrames, backfillGoneRuns): a run it recorded
// with no interaction node, a frame it appended with no frame value, and a run whose row it
// deleted an hour old, whose node it states gone. It then gives every role the store census holds
// to a node and that has none its node and its page (backfillRoles), since the publisher writes
// those only for the events it publishes from then on, and every session the census holds to a
// node and that has none its node (backfillSessions): a session that only asked, followed or was
// reached before the store existed was recorded by no write of the store, and the followed_by
// edge to it would otherwise answer no node.
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
// which reads no row a batch marked, need not find them again.
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
// It is no repair. Its input is each row's own content: it never reads one side of a
// projection to write the other, so a row whose node is already written is left alone and a
// kind whose rows disagree with the store is for the census to report, not for this to paper
// over.
func Backfill(ctx context.Context, database *store.Store, out io.Writer) (BackfillReport, error) {
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
	// The passes run in the order backfillPasses gives them, which says what that order rests on.
	for _, write := range backfillPasses {
		done, err := write(ctx, database)
		report.Kinds = append(report.Kinds, done...)
		for _, kind := range done {
			writeKindBackfill(out, kind)
		}
		if err != nil {
			return report, err
		}
	}
	return report, nil
}

func writeKindBackfill(out io.Writer, done KindBackfill) {
	fmt.Fprintf(out, "backfill-graph-store: kind=%s rows=%d nodes=%d tuples=%d skipped=%d owed=%d remaining=%d complete=%t\n",
		done.Kind, done.Rows, done.Nodes, done.Tuples, done.Skipped, done.Owed, done.Remaining, done.Complete)
}

// blockRow is one document_blocks row the backfill reads: its node's address, the document's
// own address, when the row was written, and the event that recorded the version it was
// written at, or the sentinel where none can be found.
type blockRow struct {
	artifactID, blockID string
	address, refKey     string
	createdAt           time.Time
	event               int64
	// project is the document's project, and session its conversation's, which the block's node
	// carries as its project or its party (refs' blockNode).
	project, session string
}

// backfillBlocks writes a node and an in_document tuple for every block row that names none,
// and marks each document it touched owed this release's index pass (writeBlockBatch). It then
// marks each document another release indexed after the store last stated it
// (markStaleDocuments), whose rows no longer name every block and link the store holds. A block
// whose address is past refs.MaxBlockNodeBytes is no node of the store by design, so it is neither
// written nor counted as skipped: the writers leave it unprojected too. A row a version write
// deleted after its batch was read is not written either (writeBlockBatch), and is counted
// nowhere: it is no row of the database any more.
func backfillBlocks(ctx context.Context, database *store.Store) (KindBackfill, error) {
	done := KindBackfill{Kind: refs.BlockNodeKind}
	// The documents the run marked, each once however many of its batches marked it.
	documents := map[string]struct{}{}
	after := ""
	for {
		rows, err := readBlockRows(ctx, database, after)
		if err != nil {
			return done, err
		}
		if len(rows) == 0 {
			break
		}
		after = rows[len(rows)-1].address
		written, tuples, err := writeBlockBatch(ctx, database, rows)
		if err != nil {
			return done, err
		}
		done.Rows += int64(len(written))
		done.Nodes += int64(len(written))
		done.Tuples += tuples
		for _, row := range written {
			documents[row.artifactID] = struct{}{}
		}
		if len(rows) < backfillBatch {
			break
		}
	}
	stale, err := markStaleDocuments(ctx, database)
	if err != nil {
		return done, err
	}
	for _, document := range stale {
		documents[document] = struct{}{}
	}
	done.Owed = int64(len(documents))
	if err := database.Pool.QueryRow(ctx, `
		select count(*) from document_blocks
		where node_kind is null and octet_length(node_id) <= `+addressBound).Scan(&done.Remaining); err != nil {
		return done, fmt.Errorf("count the blocks still naming no node: %w", err)
	}
	done.Complete = done.Remaining == 0
	return done, nil
}

// readBlockRows reads one batch of the blocks naming no node, by their node address so the
// walk is a keyset over a column every row of the batch carries. Each row's event is the one
// that recorded the version it was last written at (docs' versionEventID), and the store's
// sentinel where that is not found.
//
// The event is resolved once per (document, changed version) of the batch and joined back to
// the rows, not once per row: every block of one version of one document shares that pair and
// would otherwise run the same unindexed lookup over events up to backfillBatch times.
func readBlockRows(ctx context.Context, database *store.Store, after string) ([]blockRow, error) {
	rows, err := database.Pool.Query(ctx, `
		with batch as (
			select d.artifact_id, d.block_id, d.node_id, d.created_at, d.changed_version,
			       a.ref_key, a.issue_key, a.project_key, a.session_id
			from document_blocks d
			join artifacts a on a.id = d.artifact_id
			where d.node_kind is null and octet_length(d.node_id) <= `+addressBound+`
			  and d.node_id > $1
			order by d.node_id
			limit $2
		),
		caused as (
			select version.artifact_id, version.changed_version, found.id
			from (
				select distinct artifact_id, issue_key, project_key, changed_version
				from batch
			) version
			left join lateral (
				select e.id from events e
				where coalesce(e.issue_key, e.artifact_id::text, e.project_key)
				      = coalesce(version.issue_key, version.artifact_id::text, version.project_key)
				  and e.type = 'artifact.version'
				  and e.payload->>'artifact_id' = version.artifact_id::text
				  and e.payload->'version'->>'number' = version.changed_version::text
				order by e.id desc limit 1
			) found on true
		)
		select batch.artifact_id::text, batch.block_id, batch.node_id, batch.ref_key,
		       batch.created_at, coalesce(caused.id, $3), coalesce(batch.project_key, ''), coalesce(batch.session_id, '')
		from batch
		left join caused on caused.artifact_id = batch.artifact_id
			and caused.changed_version is not distinct from batch.changed_version
		order by batch.node_id
	`, after, backfillBatch, graphstore.NoEvent)
	if err != nil {
		return nil, fmt.Errorf("read the blocks naming no node: %w", err)
	}
	defer rows.Close()
	var batch []blockRow
	for rows.Next() {
		var row blockRow
		if err := rows.Scan(&row.artifactID, &row.blockID, &row.address, &row.refKey,
			&row.createdAt, &row.event, &row.project, &row.session); err != nil {
			return nil, fmt.Errorf("read a block naming no node: %w", err)
		}
		batch = append(batch, row)
	}
	return batch, rows.Err()
}

// writeBlockBatch writes the nodes and in_document tuples of the rows of one batch that are
// still there and still name no node, and names those nodes on the rows, in one transaction. It
// answers the rows it wrote and how many tuples that wrote. The nodes go in before the
// projection columns: the foreign key on (node_kind, node_address) is checked inside the
// statement that writes them.
//
// The batch was read outside this transaction, so a version write may have changed or deleted
// one of its blocks since, and a version write that deletes a block states its node gone
// (refs.IndexBlocks). The backfill is one of the three writers R23 holds to the revision
// fence's locks, so before it writes anything it takes the locks a version write holds while it
// writes a document's blocks (lockBlockDocuments), and then reads the batch's rows again under
// them (heldBlockRows). A row a version write reached in between is either gone or names its
// node already, and only the rows left are written: a node written for a deleted row would bring
// the block back as a live node with an in_document tuple and no projection row.
//
// The same transaction marks each document it wrote a block of, which is how the block's parent
// is reached (Backfill): store_owed_at, the column migration 20261116221539 adds, which only this
// release reads and which its index pass clears once it has stated every block of the document
// (refs' standsAtVersion). The null version the release before this one reads too is left alone,
// since that release would drain it, write the rows and state nothing. The mark is an update of a
// row the document already holds: every pass that writes a document's block rows writes that row
// first, in either release, and it goes only with the artifact, whose delete takes the block rows
// with it. The mark commits with the rows that name their nodes: marked after the whole pass
// instead, a run stopped by an error in a later batch, or killed, would leave the documents of its
// committed batches unmarked, and a second run reads no row a committed batch named, so nothing
// would mark them again. Their document_block_versions rows are the ones lockBlockDocuments
// already holds, so the mark takes no lock out of the store's order.
//
// The count is this batch's own tuples and the two base attributes graphstore.Write states
// for each node its insert makes, which this batch does not state itself: every node here is
// new, since a row that already names one is never written.
func writeBlockBatch(ctx context.Context, database *store.Store, batch []blockRow) ([]blockRow, int64, error) {
	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		return nil, 0, fmt.Errorf("begin the block batch: %w", err)
	}
	defer tx.Rollback(ctx)
	documents := make([]string, 0, len(batch))
	for _, row := range batch {
		documents = append(documents, row.artifactID)
	}
	if err := lockBlockDocuments(ctx, tx, documents); err != nil {
		return nil, 0, err
	}
	held, err := heldBlockRows(ctx, tx, batch)
	if err != nil {
		return nil, 0, err
	}
	if len(held) == 0 {
		return nil, 0, nil
	}
	nodes := make([]graphstore.Node, 0, len(held))
	tuples := make([]graphstore.Tuple, 0, len(held))
	addresses := make([]string, 0, len(held))
	blockIDs := make([]string, 0, len(held))
	artifacts := make([]string, 0, len(held))
	for _, row := range held {
		var parties []string
		if row.project == "" && row.session != "" {
			parties = []string{graphstore.PartySession(row.session)}
		}
		nodes = append(nodes, graphstore.Node{
			Kind: refs.BlockNodeKind, ID: row.address, Supertag: refs.BlockSupertag,
			Visibility: graphstore.VisibilityProject, State: graphstore.StateLive,
			Project: row.project, Parties: parties,
			MadeAt: row.createdAt, MadeBy: backfillActor, SourceEvent: row.event,
			Restates: true,
		})
		tuples = append(tuples, graphstore.Tuple{
			FromKind: refs.BlockNodeKind, FromID: row.address, Kind: refs.TupleInDocument,
			ToKind: "artifact", ToID: row.refKey,
			At: row.createdAt, Who: backfillActor, SourceEvent: row.event, Asserted: true,
		})
		addresses = append(addresses, row.address)
		blockIDs = append(blockIDs, row.blockID)
		artifacts = append(artifacts, row.artifactID)
	}
	if err := graphstore.Write(ctx, tx, nodes, tuples); err != nil {
		return nil, 0, fmt.Errorf("write the nodes of a block batch: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		update document_blocks d
		set node_kind = $4, node_address = b.address
		from unnest($1::text[], $2::text[], $3::text[]) as b (artifact_id, block_id, address)
		where d.artifact_id = b.artifact_id::uuid and d.block_id = b.block_id
	`, artifacts, blockIDs, addresses, refs.BlockNodeKind); err != nil {
		return nil, 0, fmt.Errorf("name the nodes of a block batch on their rows: %w", err)
	}
	// In id order and each once, the order lockBlockDocuments took these rows in.
	owed := slices.Clone(artifacts)
	slices.Sort(owed)
	if _, err := tx.Exec(ctx, `
		update document_block_versions set store_owed_at = now()
		where artifact_id = any($1::uuid[])
	`, slices.Compact(owed)); err != nil {
		return nil, 0, fmt.Errorf("mark the documents of a block batch owed this release's block index: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, 0, fmt.Errorf("commit a block batch: %w", err)
	}
	return held, int64(len(tuples)) + statedOf(nodes), nil
}

// markStaleDocuments marks, a batch at a time, every document another release indexed after this
// release's store last stated its blocks and that holds no mark yet, and answers the documents it
// marked. The release before this one, indexing a version of a document the store has stated,
// deletes the rows of the blocks and the links that version drops and states no node gone and
// retracts no tuple; it leaves store_version, which only this release's index pass writes, below
// the version it wrote (migration 20261116221539). That is the whole of what this reads: the two
// versions the document's row records, and no row of either side of a projection, so no document
// is chosen by reading its blocks against its tuples. The reconciliation's index pass over the
// marked document's latest version then states each such block gone and retracts each such link,
// as its render decides (refs.IndexBlocks).
//
// A document the store has never stated (store_version null) is no document of this pass: the
// store holds nothing of it for a deletion to strand, and its rows name no node, which the block
// pass reaches. A document whose version is null is owed an index already, and the pass that
// indexes it finds the store stale by the same two versions. A second run finds nothing a first
// one marked, and nothing once the reconciliation has indexed the documents it marked, since that
// pass sets store_version to the version it indexed.
func markStaleDocuments(ctx context.Context, database *store.Store) ([]string, error) {
	var marked []string
	after := "00000000-0000-0000-0000-000000000000"
	for {
		rows, err := database.Pool.Query(ctx, `
			select artifact_id::text from document_block_versions
			where store_version < version and store_owed_at is null and artifact_id > $1::uuid
			order by artifact_id
			limit $2
		`, after, backfillBatch)
		if err != nil {
			return marked, fmt.Errorf("read the documents another release indexed after the store: %w", err)
		}
		batch, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			return marked, fmt.Errorf("read the documents another release indexed after the store: %w", err)
		}
		if len(batch) == 0 {
			return marked, nil
		}
		after = batch[len(batch)-1]
		written, err := markStaleBatch(ctx, database, batch)
		if err != nil {
			return marked, err
		}
		marked = append(marked, written...)
		if len(batch) < backfillBatch {
			return marked, nil
		}
	}
}

// markStaleBatch marks the documents of one batch in one transaction, under the locks a version
// write holds while it writes a document's blocks (lockBlockDocuments), and only those whose two
// versions still say so under them: an index pass of this release that reached a document in
// between has stated its blocks at the version it indexed, and owes it nothing. It answers the
// documents it marked.
func markStaleBatch(ctx context.Context, database *store.Store, batch []string) ([]string, error) {
	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin the batch of documents another release indexed: %w", err)
	}
	defer tx.Rollback(ctx)
	if err := lockBlockDocuments(ctx, tx, batch); err != nil {
		return nil, err
	}
	rows, err := tx.Query(ctx, `
		update document_block_versions set store_owed_at = now()
		where artifact_id = any($1::uuid[]) and store_version < version and store_owed_at is null
		returning artifact_id::text
	`, batch)
	if err != nil {
		return nil, fmt.Errorf("mark the documents another release indexed after the store: %w", err)
	}
	marked, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, fmt.Errorf("mark the documents another release indexed after the store: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit the batch of documents another release indexed: %w", err)
	}
	return marked, nil
}

// lockBlockDocuments takes the rows the fence's writers hold while they write a document's blocks
// or its block references, in the order R23 gives and docs' indexOwedDocument takes them for one
// document: the document's owner row, then its document_block_versions row `for no key update`.
//
// Over several documents it takes every owner row first, each once, in the order of its owner
// key (events.LockOwnerRows): the one order over owner rows, in which the erase takes the owner
// rows it writes under (grapherase's lockInStoreOrder) and events.lockOwners the owners of several
// events, "artifact:<id>" for a document no issue owns before "issue:<key>". Only then does it
// take each document's document_block_versions row, in document id order. Taking one document's
// owner and fence rows before the next document's owner would put an issue at a different place
// for each set of its documents: a batch over two documents of two issues whose ids sort the
// other way round from their owner keys, and an erase writing under both, could each hold the
// owner row the other waits for.
//
// A document deleted since its batch was read has no owner row left to take, and its block rows
// and its document_block_versions row went with it (each cascades from artifacts), so there is
// nothing of it to lock. It sorts documents in place.
func lockBlockDocuments(ctx context.Context, tx pgx.Tx, documents []string) error {
	slices.Sort(documents)
	documents = slices.Compact(documents)
	rows, err := tx.Query(ctx, `
		select id::text, coalesce(issue_key, '') from artifacts where id = any($1::uuid[])
	`, documents)
	if err != nil {
		return fmt.Errorf("load the owners of a batch's documents: %w", err)
	}
	owned, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) ([2]string, error) {
		var document [2]string
		err := row.Scan(&document[0], &document[1])
		return document, err
	})
	if err != nil {
		return fmt.Errorf("load the owners of a batch's documents: %w", err)
	}
	owners := events.OwnerRows{}
	for _, document := range owned {
		owners.Add(document[1], document[0], "document "+document[0])
	}
	if err := events.LockOwnerRows(ctx, tx, owners); err != nil {
		return err
	}
	for _, document := range documents {
		if _, err := tx.Exec(ctx, `
			select 1 from document_block_versions where artifact_id = $1 for no key update
		`, document); err != nil {
			return fmt.Errorf("lock the block index row of document %s: %w", document, err)
		}
	}
	return nil
}

// heldBlockRows reads again, under the locks lockBlockDocuments took, which rows of the batch
// are still there and still name no node, locks each of them `for update` in key order, and
// answers them in the batch's own order. The rest of a row is what the batch read: a row that
// still names no node has been written by no version since, because a version names the node
// of every block it indexes (refs.IndexBlocks).
func heldBlockRows(ctx context.Context, tx pgx.Tx, batch []blockRow) ([]blockRow, error) {
	artifacts := make([]string, 0, len(batch))
	blockIDs := make([]string, 0, len(batch))
	for _, row := range batch {
		artifacts = append(artifacts, row.artifactID)
		blockIDs = append(blockIDs, row.blockID)
	}
	rows, err := tx.Query(ctx, `
		select d.artifact_id::text, d.block_id
		from document_blocks d
		join unnest($1::text[], $2::text[]) as b (artifact_id, block_id)
			on d.artifact_id = b.artifact_id::uuid and d.block_id = b.block_id
		where d.node_kind is null
		order by d.artifact_id, d.block_id
		for update of d
	`, artifacts, blockIDs)
	if err != nil {
		return nil, fmt.Errorf("read the blocks of a batch again under their documents' locks: %w", err)
	}
	keys, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) ([2]string, error) {
		var key [2]string
		err := row.Scan(&key[0], &key[1])
		return key, err
	})
	if err != nil {
		return nil, fmt.Errorf("scan the blocks of a batch read again: %w", err)
	}
	still := make(map[[2]string]struct{}, len(keys))
	for _, key := range keys {
		still[key] = struct{}{}
	}
	held := make([]blockRow, 0, len(keys))
	for _, row := range batch {
		if _, ok := still[[2]string{row.artifactID, row.blockID}]; ok {
			held = append(held, row)
		}
	}
	return held, nil
}

// blockRefRow is one block_refs row the backfill reads: the pair it holds, the block node it
// is a link out of, the row's own dates and sequence, and whether the store holds that block's
// node in a state other than gone (blockNodeStands).
type blockRefRow struct {
	artifactID, blockID string
	address             string
	kind, toKind, toID  string
	createdAt           time.Time
	sourceSeq           *int64
	event               int64
	blockNodeStands     bool
}

// backfillBlockRefs states one tuple per block reference written before the store existed. A
// block_refs row is one link out of a block and its node is that block's, so this writes no
// node of its own: it runs after the blocks, and a row whose block has no node in the store, or
// one the store holds gone, is skipped and counted. A row whose far end is past the address bound
// is no tuple the store can key beside the block's own address, as the live writer leaves it out
// too (refs' replaceBlockRefs), so this reads none of those and the row keeps its null node
// columns for the old arm. A row a version write deleted or wrote after its batch was read is not
// written (writeBlockRefBatch), and is counted nowhere.
//
// The guard reads the store and no document_blocks row (blockNodeStands), because the backfill
// reads no row its marker marks, and the block's row is marked once the block pass has run: the
// pass walks only the rows still owed, so it can run again at any time over what a rollback left
// unmarked. The node is what the pointer row's foreign key needs, and its state is graph_edges'
// own test of the tuple arm (migration 20261109215947): after this release drops a block, a build
// of the release before it can restore the block with no node on its rows, and a pointer stamped
// against the gone node would answer from neither arm, since the old arm skips a marked row and
// the tuple arm skips a gone node.
//
// The row's own source_seq is the event that stamped it, which is both the cause of its tuple
// and that tuple's legacy_source_seq; a row with none takes the store's sentinel as its cause
// and keeps no legacy sequence, exactly as the graph_edges arm over block_refs answers null
// for it.
func backfillBlockRefs(ctx context.Context, database *store.Store) (KindBackfill, error) {
	done := KindBackfill{Kind: "block_ref"}
	after := []any{"", "", "", ""}
	for {
		batch, err := readBlockRefRows(ctx, database, after)
		if err != nil {
			return done, err
		}
		if len(batch) == 0 {
			break
		}
		last := batch[len(batch)-1]
		after = []any{last.address, last.kind, last.toKind, last.toID}
		writable := make([]blockRefRow, 0, len(batch))
		for _, row := range batch {
			if !row.blockNodeStands {
				done.Skipped++
				continue
			}
			writable = append(writable, row)
		}
		if len(writable) > 0 {
			written, err := writeBlockRefBatch(ctx, database, writable)
			if err != nil {
				return done, err
			}
			done.Rows += written
			done.Tuples += written
		}
		if len(batch) < backfillBatch {
			break
		}
	}
	if err := database.Pool.QueryRow(ctx, `
		select count(*) from block_refs
		where node_kind is null and octet_length(from_node) <= `+addressBound+`
		  and octet_length(to_id) <= `+addressBound).Scan(&done.Remaining); err != nil {
		return done, fmt.Errorf("count the block references still naming no node: %w", err)
	}
	done.Complete = done.Remaining == 0
	return done, nil
}

// readBlockRefRows reads one batch of the block references naming no node, by the key a row of
// block_refs is keyed on past its artifact, so the walk never reads one row twice. Each row
// carries whether its block's node stands (blockNodeStands), which is the guard
// backfillBlockRefs skips a row by.
func readBlockRefRows(ctx context.Context, database *store.Store, after []any) ([]blockRefRow, error) {
	rows, err := database.Pool.Query(ctx, `
		select r.artifact_id::text, r.block_id, r.from_node, r.kind, r.to_kind, r.to_id,
		       r.created_at, r.source_seq, coalesce(r.source_seq, $6),
		       `+blockNodeStands(`r`)+`
		from block_refs r
		where r.node_kind is null and octet_length(r.from_node) <= `+addressBound+`
		  and octet_length(r.to_id) <= `+addressBound+`
		  and (r.from_node, r.kind, r.to_kind, r.to_id) > ($1, $2, $3, $4)
		order by r.from_node, r.kind, r.to_kind, r.to_id
		limit $5
	`, after[0], after[1], after[2], after[3], backfillBatch, graphstore.NoEvent)
	if err != nil {
		return nil, fmt.Errorf("read the block references naming no node: %w", err)
	}
	defer rows.Close()
	var batch []blockRefRow
	for rows.Next() {
		var row blockRefRow
		if err := rows.Scan(&row.artifactID, &row.blockID, &row.address, &row.kind, &row.toKind,
			&row.toID, &row.createdAt, &row.sourceSeq, &row.event, &row.blockNodeStands); err != nil {
			return nil, fmt.Errorf("read a block reference naming no node: %w", err)
		}
		batch = append(batch, row)
	}
	return batch, rows.Err()
}

// blockNodeStands is the pointer pass's guard over the block_refs row at alias: the store holds
// the node of the block the row is a link out of, in a state other than gone. It is the test
// graph_edges' tuple arm makes of that node, read off graph_nodes alone, so the pass stamps a
// pointer only where the tuple arm will answer it (backfillBlockRefs).
func blockNodeStands(alias string) string {
	return `exists (select 1 from graph_nodes block
		where block.kind = '` + refs.BlockNodeKind + `' and block.id = ` + alias + `.from_node
			and block.state <> '` + graphstore.StateGone + `')`
}

// writeBlockRefBatch appends the tuples of the rows of one batch that are still there and still
// name no node, and names the block's node on each of them, in one transaction, and answers how
// many rows it wrote, each of which is one tuple. The nodes are already there - the blocks ran
// first - so this writes none, and the call still goes through the store's own writer, which is
// what holds the two sides of a projection in one transaction.
//
// The batch was read outside this transaction, so a version write may have deleted one of its
// references since, and a version write that deletes a reference states its tuple retracted
// (refs' replaceBlockRefs). A tuple asserted for that row would be appended after the retraction
// and so be the newest word on the link: the store would answer a mention no row holds. The
// batch is therefore held to the revision fence as writeBlockBatch is: before it writes anything
// it takes the locks a version write holds while it writes a document's references
// (lockBlockDocuments), reads the batch's rows again under them (heldBlockRefRows), and writes
// only the rows left.
func writeBlockRefBatch(ctx context.Context, database *store.Store, batch []blockRefRow) (int64, error) {
	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("begin the block reference batch: %w", err)
	}
	defer tx.Rollback(ctx)
	documents := make([]string, 0, len(batch))
	for _, row := range batch {
		documents = append(documents, row.artifactID)
	}
	if err := lockBlockDocuments(ctx, tx, documents); err != nil {
		return 0, err
	}
	held, err := heldBlockRefRows(ctx, tx, batch)
	if err != nil {
		return 0, err
	}
	if len(held) == 0 {
		return 0, nil
	}
	tuples := make([]graphstore.Tuple, 0, len(held))
	artifacts := make([]string, 0, len(held))
	blockIDs := make([]string, 0, len(held))
	kinds := make([]string, 0, len(held))
	toKinds := make([]string, 0, len(held))
	toIDs := make([]string, 0, len(held))
	addresses := make([]string, 0, len(held))
	for _, row := range held {
		tuples = append(tuples, graphstore.Tuple{
			FromKind: refs.BlockNodeKind, FromID: row.address, Kind: row.kind,
			ToKind: row.toKind, ToID: row.toID,
			At: row.createdAt, Who: backfillActor, SourceEvent: row.event,
			LegacySourceSeq: row.sourceSeq, Asserted: true,
		})
		artifacts = append(artifacts, row.artifactID)
		blockIDs = append(blockIDs, row.blockID)
		kinds = append(kinds, row.kind)
		toKinds = append(toKinds, row.toKind)
		toIDs = append(toIDs, row.toID)
		addresses = append(addresses, row.address)
	}
	if err := graphstore.Write(ctx, tx, nil, tuples); err != nil {
		return 0, fmt.Errorf("append the tuples of a block reference batch: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		update block_refs r
		set node_kind = $7, node_address = p.address
		from unnest($1::text[], $2::text[], $3::text[], $4::text[], $5::text[], $6::text[])
			as p (artifact_id, block_id, kind, to_kind, to_id, address)
		where r.artifact_id = p.artifact_id::uuid and r.block_id = p.block_id
		  and r.kind = p.kind and r.to_kind = p.to_kind and r.to_id = p.to_id
	`, artifacts, blockIDs, kinds, toKinds, toIDs, addresses, refs.BlockNodeKind); err != nil {
		return 0, fmt.Errorf("name the nodes of a block reference batch on their rows: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("commit a block reference batch: %w", err)
	}
	return int64(len(held)), nil
}

// heldBlockRefRows reads again, under the locks lockBlockDocuments took, which rows of the batch
// are still there, still name no node and are still links out of a block whose node stands
// (blockNodeStands), locks each of them `for update` in key order, and answers them in the
// batch's own order. The rest of a row is what the batch read: a version write keeps a surviving
// row's dates and source_seq, and a row it inserts names its node (refs' replaceBlockRefs), so a
// row that still names no node is one no version has written since.
func heldBlockRefRows(ctx context.Context, tx pgx.Tx, batch []blockRefRow) ([]blockRefRow, error) {
	artifacts := make([]string, 0, len(batch))
	blockIDs := make([]string, 0, len(batch))
	kinds := make([]string, 0, len(batch))
	toKinds := make([]string, 0, len(batch))
	toIDs := make([]string, 0, len(batch))
	for _, row := range batch {
		artifacts = append(artifacts, row.artifactID)
		blockIDs = append(blockIDs, row.blockID)
		kinds = append(kinds, row.kind)
		toKinds = append(toKinds, row.toKind)
		toIDs = append(toIDs, row.toID)
	}
	rows, err := tx.Query(ctx, `
		select r.artifact_id::text, r.block_id, r.kind, r.to_kind, r.to_id
		from block_refs r
		join unnest($1::text[], $2::text[], $3::text[], $4::text[], $5::text[])
			as p (artifact_id, block_id, kind, to_kind, to_id)
			on r.artifact_id = p.artifact_id::uuid and r.block_id = p.block_id
			and r.kind = p.kind and r.to_kind = p.to_kind and r.to_id = p.to_id
		where r.node_kind is null and `+blockNodeStands(`r`)+`
		order by r.artifact_id, r.block_id, r.to_kind, r.to_id, r.kind
		for update of r
	`, artifacts, blockIDs, kinds, toKinds, toIDs)
	if err != nil {
		return nil, fmt.Errorf("read the block references of a batch again under their documents' locks: %w", err)
	}
	keys, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) ([5]string, error) {
		var key [5]string
		err := row.Scan(&key[0], &key[1], &key[2], &key[3], &key[4])
		return key, err
	})
	if err != nil {
		return nil, fmt.Errorf("scan the block references of a batch read again: %w", err)
	}
	still := make(map[[5]string]struct{}, len(keys))
	for _, key := range keys {
		still[key] = struct{}{}
	}
	held := make([]blockRefRow, 0, len(keys))
	for _, row := range batch {
		if _, ok := still[[5]string{row.artifactID, row.blockID, row.kind, row.toKind, row.toID}]; ok {
			held = append(held, row)
		}
	}
	return held, nil
}

// commentRow is one comments row the backfill reads: its id, its author as the row stored it,
// when it was written, the addresses of the artifact it is anchored to and the artifact that
// owns it, the event that stored it, and the project of the issue or the document it belongs to,
// or the session of the conversation that owns that document (graphstore.ScopeOf's rule).
type commentRow struct {
	id        string
	author    model.Actor
	createdAt time.Time
	anchor    string
	owner     string
	event     int64
	project   string
	parties   []string
}

// backfillComments writes a node and the facts its row is the projection of for every comment
// written before the store existed: the artifact it is anchored to and the artifact that owns it
// where it has them (graphstore.CommentFacts, as api's commentStored states them), the two base
// attributes graphstore.Write states for every node it makes, and who wrote it, as
// graphstore.Authored states that (graphstore.AuthorFacts).
func backfillComments(ctx context.Context, database *store.Store) (KindBackfill, error) {
	done := KindBackfill{Kind: graphstore.KindComment}
	after := "00000000-0000-0000-0000-000000000000"
	for {
		batch, err := readCommentRows(ctx, database, after)
		if err != nil {
			return done, err
		}
		if len(batch) == 0 {
			break
		}
		after = batch[len(batch)-1].id
		nodes, tuples, err := writeCommentBatch(ctx, database, batch)
		if err != nil {
			return done, err
		}
		done.Rows += int64(len(batch))
		done.Nodes += nodes
		done.Tuples += tuples
		if len(batch) < backfillBatch {
			break
		}
	}
	if err := database.Pool.QueryRow(ctx, `
		select count(*) from comments c
		left join artifacts anchor on anchor.id = (c.anchor->>'artifact_id')::uuid
		left join artifacts owner on owner.id = c.artifact_id
		where c.node_kind is null
		  and (anchor.ref_key is null or octet_length(anchor.ref_key) <= `+addressBound+`)
		  and (owner.ref_key is null or octet_length(owner.ref_key) <= `+addressBound+`)
	`).Scan(&done.Remaining); err != nil {
		return done, fmt.Errorf("count the comments still naming no node: %w", err)
	}
	done.Complete = done.Remaining == 0
	return done, nil
}

func readCommentRows(ctx context.Context, database *store.Store, after string) ([]commentRow, error) {
	rows, err := database.Pool.Query(ctx, `
		select c.id::text, c.author, c.created_at,
		       coalesce(anchor.ref_key, ''), coalesce(owner.ref_key, ''),
		       coalesce((
			select e.id from events e
			where coalesce(e.issue_key, e.artifact_id::text, e.project_key)
			      = coalesce(c.issue_key, c.artifact_id::text)
			  and e.type = 'comment.created'
			  and e.payload->>'id' = c.id::text
			order by e.id limit 1
		       ), $3),
		       coalesce(issue.project_key, owner.project_key, ''), coalesce(owner.session_id, '')
		from comments c
		left join issues issue on issue.key = c.issue_key
		left join artifacts anchor on anchor.id = (c.anchor->>'artifact_id')::uuid
		left join artifacts owner on owner.id = c.artifact_id
		where c.node_kind is null and c.id > $1::uuid
		  and (anchor.ref_key is null or octet_length(anchor.ref_key) <= `+addressBound+`)
		  and (owner.ref_key is null or octet_length(owner.ref_key) <= `+addressBound+`)
		order by c.id
		limit $2
	`, after, backfillBatch, graphstore.NoEvent)
	if err != nil {
		return nil, fmt.Errorf("read the comments naming no node: %w", err)
	}
	defer rows.Close()
	var batch []commentRow
	for rows.Next() {
		var row commentRow
		var author []byte
		var session string
		if err := rows.Scan(&row.id, &author, &row.createdAt, &row.anchor, &row.owner, &row.event, &row.project, &session); err != nil {
			return nil, fmt.Errorf("read a comment naming no node: %w", err)
		}
		if err := json.Unmarshal(author, &row.author); err != nil {
			return nil, fmt.Errorf("read the author of comment %s: %w", row.id, err)
		}
		if row.project == "" && session != "" {
			row.parties = []string{graphstore.PartySession(session)}
		}
		batch = append(batch, row)
	}
	return batch, rows.Err()
}

// writeCommentBatch writes one batch's comment nodes, the facts each row projects and who wrote
// each, in one transaction, and answers the nodes and the tuples it stated, as the upper bound
// the report states: an author's node another write already made moves nothing, and a tuple
// stated again under its event appends nothing. The count is the batch's own tuples and the two
// base attributes graphstore.Write states for each comment node its insert makes, which the
// batch does not state itself: every comment here is new, since a row that already names its
// node is never read.
//
// Every node of the batch goes in its one store write, the comments' and their authors', so the
// transaction takes every node row in ascending (kind, id) before it appends its first tuple, and
// names the nodes on the comments rows only after both: the order of the lock order on
// api/inbox_read.go's markInboxReadThreads. Who wrote a comment is what graphstore.Authored
// states of it (graphstore.AuthorFacts): the author's node, made at the first comment of the
// batch that names them, the comment's authored_by tuple and the aliases the server proved.
// Write makes an author's node as Authored's arms make it and moves nothing on one that is
// already there, since every writer of a person or a session node writes it as AuthorFacts does:
// project-visible, live and carrying no attribute. The batch restates that node too, so a state
// or a visibility another write gave it stays.
//
// The rows the batch read are still the rows it writes, so it reads none of them again: no route
// deletes a comment or changes its owner, its author, its time or the artifact its anchor names,
// and the erase deletes only a row that names its node, which no row of this batch does.
func writeCommentBatch(ctx context.Context, database *store.Store, batch []commentRow) (int64, int64, error) {
	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		return 0, 0, fmt.Errorf("begin the comment batch: %w", err)
	}
	defer tx.Rollback(ctx)
	nodes := make([]graphstore.Node, 0, 2*len(batch))
	var tuples []graphstore.Tuple
	ids := make([]string, 0, len(batch))
	addresses := make([]string, 0, len(batch))
	written := map[[2]string]struct{}{}
	for _, row := range batch {
		// A comment any of whose relation ends is past the store's address bound is left whole
		// out of the store, so readCommentRows never reads it and this batch never writes one:
		// a far end past the bound does not reach here (api's commentStored, outbox's rule).
		var node graphstore.Node
		node, tuples = graphstore.CommentFacts(tuples, row.id, row.author, row.createdAt, row.event,
			row.anchor, row.owner, row.project, row.parties)
		nodes = append(nodes, node)
		// One write writes a node once, so an author several comments of the batch name is made
		// by the first of them, and so is the owner a proved session names; each comment still
		// states its own authored_by and aliases.
		if authors, facts, named := graphstore.AuthorFacts(graphstore.KindComment, row.id, row.author, row.createdAt, row.event); named {
			for _, author := range authors {
				if _, made := written[[2]string{author.Kind, author.ID}]; !made {
					written[[2]string{author.Kind, author.ID}] = struct{}{}
					// The author's node is the person's or the session's, whose state and
					// visibility no comment of theirs owns.
					author.Restates = true
					nodes = append(nodes, author)
				}
			}
			tuples = append(tuples, facts...)
		}
		ids = append(ids, row.id)
		addresses = append(addresses, row.id)
	}
	if err := graphstore.Write(ctx, tx, nodes, tuples); err != nil {
		return 0, 0, fmt.Errorf("write the nodes of a comment batch: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		update comments c
		set node_kind = $3, node_address = p.address
		from unnest($1::text[], $2::text[]) as p (id, address)
		where c.id = p.id::uuid
	`, ids, addresses, graphstore.KindComment); err != nil {
		return 0, 0, fmt.Errorf("name the nodes of a comment batch on their rows: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, 0, fmt.Errorf("commit a comment batch: %w", err)
	}
	// Each comment's node states its visibility and its state, and its project or its parties
	// where it belongs to one.
	stated := 2 * int64(len(batch))
	for _, row := range batch {
		if row.project != "" || len(row.parties) > 0 {
			stated++
		}
	}
	return int64(len(nodes)), int64(len(tuples)) + stated, nil
}

// roleRow is one role the store census holds to a node (addressedRoles) whose node or page the
// store does not hold: its name, the first event that named it and that event's time, or the
// sentinel and the run's own time for a role only a row names, and which of its two nodes the
// store lacks.
type roleRow struct {
	name                 string
	event                int64
	at                   time.Time
	needsRole, needsPage bool
}

// backfillRoles writes the node of every role the store census holds to one and that the store
// holds no node of, its page and the page's part_of the role, as the publisher writes them for a
// role an event it publishes names (outbox.deliveredGraph). Its input is addressedRoles, the
// census's own role row side, so the roles this gives a node are exactly the roles the census
// holds to one: every route, message target and comment mention a database written before the
// publisher wrote them holds, in a row now or in an event.
//
// A role, its page and that part_of have no projection row, so there is no row to name the node
// on and no fence to take. A role the store already holds both nodes of is left alone, and the
// write is safe beside a running publisher and on a second run: a node another write made is
// written `do nothing`, and the part_of is a claim, appended only where the page is not part of
// the role already (graphstore.WriteArms). The writer is the run (backfillActor), and the cause
// and the time are the first event that named the role, or the sentinel and the run's own time
// for a role only a row names, which no event dates.
//
// No arm of graph_edges answers a role, so its Complete switches nothing off: it reads whether
// every role the census names holds its node and its page.
func backfillRoles(ctx context.Context, database *store.Store) (KindBackfill, error) {
	done := KindBackfill{Kind: graphstore.KindRole}
	missing, err := readRoleRows(ctx, database)
	if err != nil {
		return done, err
	}
	for start := 0; start < len(missing); start += backfillBatch {
		batch := missing[start:min(start+backfillBatch, len(missing))]
		nodes, tuples, err := writeRoleBatch(ctx, database, batch)
		if err != nil {
			return done, err
		}
		done.Rows += int64(len(batch))
		done.Nodes += nodes
		done.Tuples += tuples
	}
	left, err := readRoleRows(ctx, database)
	if err != nil {
		return done, err
	}
	done.Remaining = int64(len(left))
	done.Complete = done.Remaining == 0
	return done, nil
}

// readRoleRows reads every role addressedRoles names whose node or page the store does not hold
// and whose address fits the store's bound, in name order. A role name past the bound is no node
// of the store, and the page of a role whose name leaves no room for the `role/` prefix is past
// the bound too: each is owed only where its own address fits, so a role the writers leave out on
// purpose is neither written here nor counted as remaining (outbox.deliveredGraph).
func readRoleRows(ctx context.Context, database *store.Store) ([]roleRow, error) {
	rows, err := database.Pool.Query(ctx, `
		select addressed.name, coalesce(addressed.event, $1), coalesce(e.created_at, now()),
		       role.id is null and octet_length(addressed.name) <= $4,
		       page.id is null and octet_length($2::text || '/' || addressed.name) <= $4
		from (`+addressedRoles+`) addressed
		left join events e on e.id = addressed.event
		left join graph_nodes role on role.kind = $2::text and role.id = addressed.name
		left join graph_nodes page on page.kind = $3::text and page.id = $2::text || '/' || addressed.name
		where (role.id is null and octet_length(addressed.name) <= $4)
		   or (page.id is null and octet_length($2::text || '/' || addressed.name) <= $4)
		order by addressed.name
	`, graphstore.NoEvent, graphstore.KindRole, graphstore.KindPage, graphstore.MaxAddressBytes)
	if err != nil {
		return nil, fmt.Errorf("read the roles the store holds no node of: %w", err)
	}
	defer rows.Close()
	var missing []roleRow
	for rows.Next() {
		var row roleRow
		if err := rows.Scan(&row.name, &row.event, &row.at, &row.needsRole, &row.needsPage); err != nil {
			return nil, fmt.Errorf("read a role the store holds no node of: %w", err)
		}
		missing = append(missing, row)
	}
	return missing, rows.Err()
}

// writeRoleBatch writes one batch's role and page nodes and each page's part_of its role, in one
// statement. It answers the nodes it wrote and, as the upper bound the report states, the tuples:
// the two base attributes of each node and one part_of per page, each of which the write appends
// only where the store does not hold it already.
func writeRoleBatch(ctx context.Context, database *store.Store, batch []roleRow) (int64, int64, error) {
	var nodes []graphstore.Node
	claims := make([]graphstore.Claim, 0, len(batch))
	for _, row := range batch {
		page := graphstore.RolePageID(row.name)
		if row.needsRole {
			nodes = append(nodes, graphstore.RoleNode(row.name, backfillActor, row.at, row.event))
		}
		if row.needsPage {
			nodes = append(nodes, graphstore.PageNode(page, backfillActor, row.at, row.event))
		}
		// The part_of claim keys the page and the role at once, so it is appended only where the
		// page's address fits (and so the role's, which the page contains): a role whose page is
		// past the bound has no page node, and a claim naming it would key an address the store
		// cannot hold.
		if graphstore.AddressFits(page) {
			claims = append(claims, graphstore.Claim{
				FromKind: graphstore.KindPage, FromID: page, Kind: graphstore.TuplePartOf,
				ToKind: graphstore.KindRole, ToID: row.name,
				At: row.at, Who: backfillActor, SourceEvent: row.event,
			})
		}
	}
	arms, params, err := graphstore.WriteArms(1, nodes, nil, claims)
	if err != nil {
		return 0, 0, err
	}
	if _, err := database.Pool.Exec(ctx, "with "+arms+"\nselect 1", params...); err != nil {
		return 0, 0, fmt.Errorf("write the nodes of a role batch: %w", err)
	}
	return int64(len(nodes)), 2*int64(len(nodes)) + int64(len(claims)), nil
}

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
// the cause is the first event that named the session - an ask.follower_added, a comment.delivery
// receipt, or the event that stored a comment the erase took - or the sentinel for a session only
// a row names, and the time is that event's, or the earliest row's that names it.
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
// store, so it is neither written here nor counted as remaining (graphstore.SessionRecorded).
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

// runFrameHeld is true of a going_events row e whose run's interaction node holds a frame value of
// e's sequence: the anti-join backfillRunFrames finds an older pod's frames by.
var runFrameHeld = `exists (
		select 1 from graph_tuples t
		where t.from_kind = '` + graphstore.KindInteraction + `' and t.from_id = e.request_id::text
		  and t.kind = '` + graphstore.AttributeFrame + `' and t.to_kind = '` + graphstore.KindValue + `' and t.asserted
		  and (t.to_id::jsonb->>'sequence')::integer = e.sequence)`

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
// node, through the one writer of such a node from its row (writeRun, WriteRun): the node, its
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
			if written.nodes > 0 {
				done.Rows++
				done.Nodes += written.nodes
				done.Tuples += written.tuples
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

// writeOwedRun writes the node of one run in a transaction of its own (writeRun).
func writeOwedRun(ctx context.Context, database *store.Store, id string) (runWritten, error) {
	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		return runWritten{}, fmt.Errorf("begin the node of run %s: %w", id, err)
	}
	defer tx.Rollback(ctx)
	written, err := writeRun(ctx, tx, id)
	if err != nil {
		return runWritten{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return runWritten{}, fmt.Errorf("commit the node of run %s: %w", id, err)
	}
	return written, nil
}

// backfillRunFrames states a frame value of every going_events row whose run's interaction holds
// no frame value of that sequence (runFrameHeld): a frame a process of the release before
// appended to a run this build or the passes before gave a node. Each owed run is written in a
// transaction of its own (writeRunFrames). Rows counts the frames it stated and Tuples the values,
// one each, with what writeRun stated of a run that had no node by then.
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
			done.Tuples += frames + written.tuples
			done.Nodes += written.nodes
		}
		if len(owed) < backfillBatch {
			break
		}
		after = owed[len(owed)-1]
	}
	var left int64
	if err := database.Pool.QueryRow(ctx, `select count(*) from going_events e where not `+runFrameHeld).Scan(&left); err != nil {
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
		where e.request_id > $1::uuid and not `+runFrameHeld+`
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
// writeRun, which states every frame the row holds, and which it answers. The values are read off
// the rows (readRunFrames), exactly as the frame path states them.
func writeRunFrames(ctx context.Context, database *store.Store, run string) (int64, runWritten, error) {
	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		return 0, runWritten{}, fmt.Errorf("begin the frames of run %s: %w", run, err)
	}
	defer tx.Rollback(ctx)
	locked, err := graphstore.LockInteraction(ctx, tx, run)
	if err != nil {
		return 0, runWritten{}, err
	}
	if !locked {
		written, err := writeRun(ctx, tx, run)
		if err != nil {
			return 0, runWritten{}, err
		}
		if err := tx.Commit(ctx); err != nil {
			return 0, runWritten{}, fmt.Errorf("commit the node of run %s: %w", run, err)
		}
		return 0, written, nil
	}
	frames, err := readRunFrames(ctx, tx, run, true)
	if err != nil {
		return 0, runWritten{}, err
	}
	if len(frames) == 0 {
		return 0, runWritten{}, nil
	}
	if err := graphstore.Write(ctx, tx, nil, frames); err != nil {
		return 0, runWritten{}, fmt.Errorf("state the frames of run %s: %w", run, err)
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, runWritten{}, fmt.Errorf("commit the frames of run %s: %w", run, err)
	}
	return int64(len(frames)), runWritten{}, nil
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

// The asks and their followers (dispatch://INFRA-1150 R3). An ask's node holds the documents it is
// anchored to, owned by and part of, who asked it, its answer and the sessions that follow it, and
// asks and ask_followers are the projection of those tuples from the moment a row names its node
// (20261116094058's node_kind and node_address). A row a build before this release wrote names
// none, and graph_edges' old arms answer it until these two passes state the row on the node and
// mark it, on INFRA-1149's backfill contract: each walks only the rows its marker does not mark,
// states every fact of a row in the transaction that marks it, and reads no marked row, so a run
// after another, or beside a live server, does only the rows still owed. They are the boot step's
// first phase after the object nodes (docs' decision step), and `backfill-graph-store` runs them
// too.
//
// Each batch takes the store's lock order (api/inbox_read.go's markInboxReadThreads): it reads
// what it takes without a lock, then takes the owner row of every ask of the batch, each once and
// in ascending owner key (lockAskOwners), and only then locks its rows, in key order, and reads
// them again under those locks, leaving out a row marked since. A writer of an ask's answer or of
// its followers takes the ask's owner row before the ask's own rows, and settlement takes it
// before the room lock, the document's ask rows and the nodes of their authors (createAskBlock),
// so a batch that locked its asks rows first, in id order and across owners, could hold an
// approval ask a settlement of its issue goes on to answer while it waited for the node of an
// author that settlement held, or hold one ask of a document and wait for another that a
// settlement took in its own order. Holding the owner first, the batch waits for such a writer
// holding nothing it wants, or the writer waits for it.

// askRowsOwed is the asks rows the asks pass states (writeAskBatch): each row that names no node,
// whose node the store holds, and none of whose three documents' addresses is past the store's
// bound - such a row is left out whole, as a comment is (graphstore.AskPlaces.Fit). It is the from
// and the where every read of those rows shares - the batch's read of what it takes
// (askBatchRead), its read again under its locks (askRowsRead), the count of what is left and the
// probe of whether any is owed (AsksOwed) - over asks k and its three documents, anchor, owner and
// document.
var askRowsOwed = `
	from asks k
	join graph_nodes node on node.kind = 'ask' and node.id = k.id::text
	left join artifacts anchor on anchor.id = coalesce(k.block_artifact_id, (k.anchor->>'artifact_id')::uuid)
	left join artifacts owner on owner.id = k.artifact_id
	left join artifacts document on document.id = k.block_artifact_id
	where k.node_kind is null
	  and octet_length(coalesce(anchor.ref_key, '')) <= ` + addressBound + `
	  and octet_length(coalesce(owner.ref_key, '')) <= ` + addressBound + `
	  and octet_length(coalesce(document.ref_key, '')) <= ` + addressBound

// askBatchRead is one batch of the asks rows the asks pass owes (askRowsOwed), after the id $1 and
// at most $2 of them, each with its owner: its issue, or the document an ask on no issue belongs to
// (asks_one_owner). It locks nothing, and a follower's column is empty: writeAskBatch takes the
// owners' rows, then reads the rows again under them (askRowsRead).
var askBatchRead = `
	select k.id::text, '', coalesce(k.issue_key, ''), coalesce(k.artifact_id::text, '')` + askRowsOwed + `
	  and k.id > $1::uuid
	order by k.id
	limit $2`

// askRowsRead reads again, under the owner rows writeAskBatch took, the rows of the batch $1 the
// pass still owes (askRowsOwed), locked in id order as an update of each row locks it: an answer
// this release writes on such a row (asks.Settle) takes the same lock and writes the row alone, so
// the batch reads the answer it marks, and an answer written after it finds the row marked and
// states itself on the node. Each row answers its author, created_at, answer state, answer and
// resolution, the addresses of its three documents (graphstore.AskPlaces), the ask.opened that
// recorded it and the newest event that wrote its answer state, each $2 where none is found.
var askRowsRead = `
	select k.id::text, k.author, k.created_at, k.state, k.answer, k.resolution,
	       coalesce(anchor.ref_key, ''), coalesce(owner.ref_key, ''), coalesce(document.ref_key, ''),
	       coalesce((select e.id from events e
	                 where e.payload->>'id' = k.id::text and e.type = 'ask.opened'
	                 order by e.id limit 1), $2),
	       coalesce((select e.id from events e
	                 where e.payload->>'id' = k.id::text and e.type in ('ask.opened', 'ask.answered', 'ask.resolved')
	                 order by e.id desc limit 1), $2)` + askRowsOwed + `
	  and k.id = any($1::uuid[])
	order by k.id
	for no key update of k`

// askBatchRow is one row of a batch the asks or the follower pass read without a lock
// (askBatchRead, askFollowerBatchRead): the ask, the session that follows it for a follower row,
// and the ask's owner - its issue, or the document an ask on no issue belongs to - which is the
// owner a follower row stands under too.
type askBatchRow struct {
	askID, session  string
	issue, document string
}

// readAskBatch runs read, a pass's read of one batch that locks nothing, on the pool with args.
func readAskBatch(ctx context.Context, database *store.Store, read string, args ...any) ([]askBatchRow, error) {
	rows, err := database.Pool.Query(ctx, read, args...)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (askBatchRow, error) {
		var batched askBatchRow
		err := row.Scan(&batched.askID, &batched.session, &batched.issue, &batched.document)
		return batched, err
	})
}

// lockAskOwners takes the owner row of every ask of batch, each once, in ascending owner key
// (events.LockOwnerRows), the one order over owner rows, which lockBlockDocuments takes a block
// batch's owners in: a batch over the asks of two issues takes the two issues' rows in key order
// whatever order its asks' ids put them in. An owner gone since the batch was read locks nothing,
// and the read under the locks leaves out its rows, which went with it.
func lockAskOwners(ctx context.Context, tx pgx.Tx, batch []askBatchRow) error {
	owners := events.OwnerRows{}
	for _, row := range batch {
		owners.Add(row.issue, row.document, "ask "+row.askID)
	}
	return events.LockOwnerRows(ctx, tx, owners)
}

// backfillAsks states each asks row that names no node on the ask's node and marks the row: the
// documents it is anchored to, owned by and part of and who asked it (graphstore.AskFacts), dated
// at the row's created_at under the ask.opened that recorded it, and its answer state
// (asks.AnswerFacts), dated at the answer's or the resolution's own `at` - the row's created_at for
// an open ask - under the newest event that wrote it, each written by the person the row records
// beside it. An ask whose node the store does not hold yet is left for the object pass that writes
// it (backfillObjectNodes), which runs before this.
func backfillAsks(ctx context.Context, database *store.Store) (KindBackfill, error) {
	done := KindBackfill{Kind: "asks"}
	after := zeroUUID
	for {
		batch, err := readAskBatch(ctx, database, askBatchRead, after, backfillBatch)
		if err != nil {
			return done, fmt.Errorf("read the asks naming no node: %w", err)
		}
		if len(batch) == 0 {
			break
		}
		after = batch[len(batch)-1].askID
		rows, nodes, tuples, err := writeAskBatch(ctx, database, batch)
		if err != nil {
			return done, err
		}
		done.Rows += rows
		done.Nodes += nodes
		done.Tuples += tuples
		if len(batch) < backfillBatch {
			break
		}
	}
	if err := database.Pool.QueryRow(ctx, `select count(*) `+askRowsOwed).Scan(&done.Remaining); err != nil {
		return done, fmt.Errorf("count the asks still naming no node: %w", err)
	}
	done.Complete = done.Remaining == 0
	return done, nil
}

// writeAskBatch takes the owner rows of batch's asks (lockAskOwners), then reads again and locks the
// rows of batch still owed (askRowsRead) and states them, in one transaction, and answers how many
// rows it marked and the nodes and the tuples it stated as the upper bound the report states. Every
// node of the batch - the authors' and the answerers' - goes in its one store write before the
// first tuple, and the rows are marked after both, as writeCommentBatch orders its write. The ask
// nodes themselves are not written again: the object pass wrote each, and this states only the
// facts the row projects.
func writeAskBatch(ctx context.Context, database *store.Store, batch []askBatchRow) (int64, int64, int64, error) {
	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		return 0, 0, 0, fmt.Errorf("begin an ask batch: %w", err)
	}
	defer tx.Rollback(ctx)
	if err := lockAskOwners(ctx, tx, batch); err != nil {
		return 0, 0, 0, err
	}
	read := make([]string, len(batch))
	for index, row := range batch {
		read[index] = row.askID
	}
	rows, err := tx.Query(ctx, askRowsRead, read, graphstore.NoEvent)
	if err != nil {
		return 0, 0, 0, fmt.Errorf("read the asks naming no node: %w", err)
	}
	var ids []string
	var nodes []graphstore.Node
	var tuples []graphstore.Tuple
	written := map[[2]string]struct{}{}
	addNodes := func(more []graphstore.Node) {
		for _, node := range more {
			if _, made := written[[2]string{node.Kind, node.ID}]; !made {
				written[[2]string{node.Kind, node.ID}] = struct{}{}
				nodes = append(nodes, node)
			}
		}
	}
	for rows.Next() {
		var id, state string
		var authorJSON, answerJSON, resolutionJSON []byte
		var createdAt time.Time
		var places graphstore.AskPlaces
		var opened, settled int64
		if err := rows.Scan(&id, &authorJSON, &createdAt, &state, &answerJSON, &resolutionJSON,
			&places.Anchor, &places.Owner, &places.Document, &opened, &settled); err != nil {
			rows.Close()
			return 0, 0, 0, fmt.Errorf("read an ask naming no node: %w", err)
		}
		var author model.Actor
		if err := json.Unmarshal(authorJSON, &author); err != nil {
			rows.Close()
			return 0, 0, 0, fmt.Errorf("read the author of ask %s: %w", id, err)
		}
		var answer *model.AskAnswer
		if len(answerJSON) > 0 {
			answer = &model.AskAnswer{}
			if err := json.Unmarshal(answerJSON, answer); err != nil {
				rows.Close()
				return 0, 0, 0, fmt.Errorf("read the answer of ask %s: %w", id, err)
			}
		}
		var resolution *model.AskResolution
		if len(resolutionJSON) > 0 {
			resolution = &model.AskResolution{}
			if err := json.Unmarshal(resolutionJSON, resolution); err != nil {
				rows.Close()
				return 0, 0, 0, fmt.Errorf("read the resolution of ask %s: %w", id, err)
			}
		}
		// The ask's node is the object pass's, written already; its facts are what this states.
		askNodes, facts, _ := graphstore.AskFacts(id, "", nil, author, createdAt, opened, places)
		addNodes(askNodes[1:])
		tuples = append(tuples, facts...)
		who, at := backfillActor, createdAt
		switch {
		case state == asks.StateResolved && resolution != nil:
			who, at = resolution.Actor, resolution.At
		case answer != nil:
			who, at = model.Actor{Kind: "user", ID: answer.User, Act: answer.Act}, answer.At
		}
		answerNodes, answerFacts, err := asks.AnswerFacts(id, state, answer, resolution, who, at, settled)
		if err != nil {
			rows.Close()
			return 0, 0, 0, err
		}
		addNodes(answerNodes)
		tuples = append(tuples, answerFacts...)
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, 0, 0, fmt.Errorf("read the asks naming no node: %w", err)
	}
	if len(ids) == 0 {
		return 0, 0, 0, nil
	}
	if err := graphstore.Write(ctx, tx, nodes, tuples); err != nil {
		return 0, 0, 0, fmt.Errorf("state a batch of asks: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		update asks set node_kind = $2, node_address = id::text
		where id = any($1::uuid[]) and node_kind is null
	`, ids, graphstore.KindAsk); err != nil {
		return 0, 0, 0, fmt.Errorf("name the nodes of an ask batch on their rows: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, 0, 0, fmt.Errorf("commit an ask batch: %w", err)
	}
	return int64(len(ids)), int64(len(nodes)), int64(len(tuples)) + 2*int64(len(nodes)), nil
}

// askFollowerRowsOwed is the ask_followers rows the follower pass states (writeAskFollowerBatch):
// each row that names no node, of an ask whose node the store holds and a session the store can
// key. It is the from and the where every read of those rows shares, as askRowsOwed is the asks
// pass's, over ask_followers f and the ask k it follows.
var askFollowerRowsOwed = `
	from ask_followers f
	join asks k on k.id = f.ask_id
	join graph_nodes node on node.kind = 'ask' and node.id = f.ask_id::text
	where f.node_kind is null and octet_length(f.session_id) <= ` + addressBound

// askFollowerBatchRead is one batch of the follower rows the follower pass owes
// (askFollowerRowsOwed), after the pair ($1, $2) and at most $3 of them, each with its ask's owner,
// which a follower row stands under: the follow route takes it before the row (api's
// changeAskFollower). It locks nothing, as askBatchRead locks nothing.
var askFollowerBatchRead = `
	select f.ask_id::text, f.session_id, coalesce(k.issue_key, ''), coalesce(k.artifact_id::text, '')` + askFollowerRowsOwed + `
	  and (f.ask_id, f.session_id) > ($1::uuid, $2::text)
	order by f.ask_id, f.session_id
	limit $3`

// askFollowerRowsRead reads again, under the owner rows writeAskFollowerBatch took, the follower
// rows of the batch of pairs ($1, $2) the pass still owes (askFollowerRowsOwed), locked in key
// order as an update locks each, as askRowsRead locks the asks.
var askFollowerRowsRead = `
	select f.ask_id::text, f.session_id, f.since` + askFollowerRowsOwed + `
	  and (f.ask_id, f.session_id) in (select * from unnest($1::uuid[], $2::text[]))
	order by f.ask_id, f.session_id
	for no key update of f`

// backfillAskFollowers states each ask_followers row that names no node as the ask node's
// followed_by to the session, dated at the row's since, and marks the row; the session is a node
// of the store from the same write on, as a follow makes it (asks.Follow). No row records who
// added a follower, so the writer is the run (backfillActor), and no event is found for a row, so
// the cause is the sentinel. A row of an ask the store holds no node of is left for the object
// pass, and one of a session past the store's bound is no relation the store can key.
func backfillAskFollowers(ctx context.Context, database *store.Store) (KindBackfill, error) {
	done := KindBackfill{Kind: "ask_followers"}
	after := [2]string{zeroUUID, ""}
	for {
		batch, err := readAskBatch(ctx, database, askFollowerBatchRead, after[0], after[1], backfillBatch)
		if err != nil {
			return done, fmt.Errorf("read the ask followers naming no node: %w", err)
		}
		if len(batch) == 0 {
			break
		}
		last := batch[len(batch)-1]
		after = [2]string{last.askID, last.session}
		rows, nodes, tuples, err := writeAskFollowerBatch(ctx, database, batch)
		if err != nil {
			return done, err
		}
		done.Rows += rows
		done.Nodes += nodes
		done.Tuples += tuples
		if len(batch) < backfillBatch {
			break
		}
	}
	if err := database.Pool.QueryRow(ctx, `select count(*) `+askFollowerRowsOwed).Scan(&done.Remaining); err != nil {
		return done, fmt.Errorf("count the ask followers still naming no node: %w", err)
	}
	done.Complete = done.Remaining == 0
	return done, nil
}

// writeAskFollowerBatch takes the owner rows of batch's asks (lockAskOwners), then reads again and
// locks the follower rows of batch still owed (askFollowerRowsRead) and states them, in one
// transaction - the sessions' nodes, then each followed_by, then the marks - and answers how many
// rows it marked and the nodes and the tuples it stated as the upper bound the report states.
func writeAskFollowerBatch(ctx context.Context, database *store.Store, batch []askBatchRow) (int64, int64, int64, error) {
	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		return 0, 0, 0, fmt.Errorf("begin an ask follower batch: %w", err)
	}
	defer tx.Rollback(ctx)
	if err := lockAskOwners(ctx, tx, batch); err != nil {
		return 0, 0, 0, err
	}
	readAsks := make([]string, len(batch))
	readSessions := make([]string, len(batch))
	for index, row := range batch {
		readAsks[index], readSessions[index] = row.askID, row.session
	}
	rows, err := tx.Query(ctx, askFollowerRowsRead, readAsks, readSessions)
	if err != nil {
		return 0, 0, 0, fmt.Errorf("read the ask followers naming no node: %w", err)
	}
	var askIDs, sessions []string
	var nodes []graphstore.Node
	var tuples []graphstore.Tuple
	made := map[string]struct{}{}
	for rows.Next() {
		var askID, session string
		var since time.Time
		if err := rows.Scan(&askID, &session, &since); err != nil {
			rows.Close()
			return 0, 0, 0, fmt.Errorf("read an ask follower naming no node: %w", err)
		}
		if _, ok := made[session]; !ok {
			made[session] = struct{}{}
			nodes = append(nodes, graphstore.SessionNode(session, backfillActor, since, graphstore.NoEvent))
		}
		tuples = append(tuples, graphstore.Tuple{
			FromKind: graphstore.KindAsk, FromID: askID, Kind: graphstore.TupleFollowedBy,
			ToKind: graphstore.KindSession, ToID: session,
			At: since, Who: backfillActor, SourceEvent: graphstore.NoEvent, Asserted: true,
		})
		askIDs = append(askIDs, askID)
		sessions = append(sessions, session)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, 0, 0, fmt.Errorf("read the ask followers naming no node: %w", err)
	}
	if len(askIDs) == 0 {
		return 0, 0, 0, nil
	}
	if err := graphstore.Write(ctx, tx, nodes, tuples); err != nil {
		return 0, 0, 0, fmt.Errorf("state a batch of ask followers: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		update ask_followers f set node_kind = $3, node_address = p.ask_id
		from unnest($1::text[], $2::text[]) as p (ask_id, session_id)
		where f.ask_id = p.ask_id::uuid and f.session_id = p.session_id and f.node_kind is null
	`, askIDs, sessions, graphstore.KindAsk); err != nil {
		return 0, 0, 0, fmt.Errorf("name the nodes of an ask follower batch on their rows: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, 0, 0, fmt.Errorf("commit an ask follower batch: %w", err)
	}
	return int64(len(askIDs)), int64(len(nodes)), int64(len(tuples)) + 2*int64(len(nodes)), nil
}

// askPasses are the asks pass and the follower pass, in the order Backfill and BackfillAsks both
// run them.
var askPasses = []func(context.Context, *store.Store) (KindBackfill, error){backfillAsks, backfillAskFollowers}

// BackfillAsks is the boot step's first phase (docs' decision step): the object nodes of every
// issue, document, ask and message written before their writers wrote one (backfillObjectNodes),
// so every ask a build before this release wrote has its node before a mark names it, then the
// asks and the ask_followers rows that name no node, each in committed batches of backfillBatch.
// It answers what each pass did; an error stops it where it stands, and every batch committed
// before it stays.
func BackfillAsks(ctx context.Context, database *store.Store) ([]KindBackfill, error) {
	done, err := backfillObjectNodes(ctx, database)
	if err != nil {
		return done, err
	}
	for _, write := range askPasses {
		report, err := write(ctx, database)
		done = append(done, report)
		if err != nil {
			return done, err
		}
	}
	return done, nil
}

// AsksOwed reports whether BackfillAsks has anything to do: a row of an object kind that names no
// node, or an asks or ask_followers row the passes would mark. Each read stops at its first owed
// row, so a store that owes one costs little, but a store that owes nothing is read to the end: an
// anti-join of issues, artifacts, asks and messages to graph_nodes, then asks and ask_followers,
// which no index keys on a null marker. The decisions step reads it only where another build can
// have written such a row (docs' decision step, probe).
func AsksOwed(ctx context.Context, q graphstore.Queryer) (bool, error) {
	for _, kind := range objectKinds {
		var owed bool
		if err := q.QueryRow(ctx, `select exists (`+kind.read+`)`, kind.start, 1).Scan(&owed); err != nil {
			return false, fmt.Errorf("read whether a %s row names no node: %w", kind.kind, err)
		}
		if owed {
			return true, nil
		}
	}
	var owed bool
	if err := q.QueryRow(ctx, `
		select exists (select 1 `+askRowsOwed+`) or exists (select 1 `+askFollowerRowsOwed+`)
	`).Scan(&owed); err != nil {
		return false, fmt.Errorf("read whether an ask or a follower names no node: %w", err)
	}
	return owed, nil
}
