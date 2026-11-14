package graphcensus

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"

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

// commentNodeKind is what the node and tuple store calls a comment, and commentSupertag the
// supertag every one of them carries (migration 20261109051232), as the server's own comment
// write states them (api's commentStoreWrite). A comment's address there is its row's own id.
const (
	commentNodeKind = "comment"
	commentSupertag = "comment"
)

// The relations a comment holds beside the two base attributes, as api's commentStored states
// them: the artifact its anchor names and the artifact it belongs to.
const (
	tupleAnchoredTo = "anchored_to"
	tupleOwnedBy    = "owned_by"
	tupleVisibility = "visibility"
	tupleState      = "state"
)

// KindBackfill is what the run did for one kind.
type KindBackfill struct {
	Kind string
	// Rows is how many projection rows it gave a node, Nodes and Tuples how many of each it
	// wrote, and Skipped how many rows it could not do and left as they were.
	Rows, Nodes, Tuples, Skipped int64
	// Owed is how many documents it marked owed a block index, for the block kind alone.
	Owed int64
	// Complete is whether every row of the kind the store can hold now names its node, which
	// is what sets the kind's graph_store_cutover row.
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
// (backfillRoles), since the publisher writes those only for the events it publishes from then on.
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
// rendered version, which no column of document_blocks holds, so the run marks the document
// owed a block index instead - document_block_versions.version to null, as refs.RebuildAll
// marks it - and the reconciliation states the part_of tuple and writes parent_block_id, which
// is the one writer of that column.
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
	// session, which a database-wide backfill belongs to none of. Two runs therefore state such
	// a fact twice, since the repeat keys skip the sentinel, and nothing reads wrong for it: a
	// current read takes the newest asserting tuple and the census counts nodes and projection
	// rows rather than tuples.
	fmt.Fprintf(out, "backfill-graph-store: event=%d\n", graphstore.NoEvent)
	for _, write := range []func(context.Context, *store.Store) (KindBackfill, error){
		backfillBlocks, backfillBlockRefs, backfillComments, backfillRoles,
	} {
		done, err := write(ctx, database)
		if err != nil {
			return report, err
		}
		if done.Complete {
			if _, err := database.Pool.Exec(ctx,
				`update graph_store_cutover set complete = true where kind = $1`, done.Kind); err != nil {
				return report, fmt.Errorf("record the cutover of %s: %w", done.Kind, err)
			}
		}
		report.Kinds = append(report.Kinds, done)
		writeKindBackfill(out, done)
	}
	return report, nil
}

func writeKindBackfill(out io.Writer, done KindBackfill) {
	fmt.Fprintf(out, "backfill-graph-store: kind=%s rows=%d nodes=%d tuples=%d skipped=%d owed=%d remaining=%d cutover=%t\n",
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
}

// backfillBlocks writes a node and an in_document tuple for every block row that names none,
// and marks each document it touched owed a block index. A block whose address is past
// refs.MaxBlockNodeBytes is no node of the store by design, so it is neither written nor
// counted as skipped: the writers leave it unprojected too. A row a version write deleted after
// its batch was read is not written either (writeBlockBatch), and is counted nowhere: it is no
// row of the database any more.
func backfillBlocks(ctx context.Context, database *store.Store) (KindBackfill, error) {
	done := KindBackfill{Kind: refs.BlockNodeKind}
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
	owed, err := markDocumentsOwed(ctx, database, documents)
	if err != nil {
		return done, err
	}
	done.Owed = owed
	if err := database.Pool.QueryRow(ctx, `
		select count(*) from document_blocks
		where node_kind is null and octet_length(node_id) <= `+blockBound).Scan(&done.Remaining); err != nil {
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
			       a.ref_key, a.issue_key, a.project_key
			from document_blocks d
			join artifacts a on a.id = d.artifact_id
			where d.node_kind is null and octet_length(d.node_id) <= `+blockBound+`
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
		       batch.created_at, coalesce(caused.id, $3)
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
			&row.createdAt, &row.event); err != nil {
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
		nodes = append(nodes, graphstore.Node{
			Kind: refs.BlockNodeKind, ID: row.address, Supertag: refs.BlockSupertag,
			Visibility: graphstore.VisibilityProject, State: graphstore.StateLive,
			MadeAt: row.createdAt, MadeBy: backfillActor, SourceEvent: row.event,
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
	if err := tx.Commit(ctx); err != nil {
		return nil, 0, fmt.Errorf("commit a block batch: %w", err)
	}
	return held, int64(len(tuples)) + 2*int64(len(nodes)), nil
}

// lockBlockDocuments takes, for each of documents, the two rows the fence's writers hold while
// they write a document's blocks or its block references, in the order R23 gives and docs'
// indexOwedDocument takes them:
// the document's owner row (lockDocumentOwner), then its document_block_versions row
// `for no key update`. The documents go in id order, the order the erase takes the locks of
// several documents in (Erasure.readDocuments), so two writers that each hold more than one
// document cannot each hold a row the other waits for. It sorts documents in place.
func lockBlockDocuments(ctx context.Context, tx pgx.Tx, documents []string) error {
	slices.Sort(documents)
	for _, document := range slices.Compact(documents) {
		if err := lockDocumentOwner(ctx, tx, document); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `
			select 1 from document_block_versions where artifact_id = $1 for no key update
		`, document); err != nil {
			return fmt.Errorf("lock the block index row of document %s: %w", document, err)
		}
	}
	return nil
}

// lockDocumentOwner takes a document's owner row with the statements docs' lockArtifactOwner
// runs, which the version write and the reconciliation tick take it with: its issue's row, or a
// project document's own artifact row, `for no key update`. This package cannot import docs
// (Stores says why), so the statements are written here again, and a change to how that
// function locks is a change to make here too. A document deleted since its batch was read has
// no owner row left to take, and its block rows went with it (document_blocks cascades from
// artifacts), so there is nothing of it to lock.
func lockDocumentOwner(ctx context.Context, tx pgx.Tx, artifactID string) error {
	var issueKey *string
	switch err := tx.QueryRow(ctx, `select issue_key from artifacts where id = $1`, artifactID).Scan(&issueKey); {
	case errors.Is(err, pgx.ErrNoRows):
		return nil
	case err != nil:
		return fmt.Errorf("load the owner of document %s: %w", artifactID, err)
	}
	if issueKey == nil {
		if _, err := tx.Exec(ctx, `
			select true from artifacts where id = $1 and issue_key is null for no key update
		`, artifactID); err != nil {
			return fmt.Errorf("lock document %s: %w", artifactID, err)
		}
		return nil
	}
	if _, err := tx.Exec(ctx, `
		select closed_at is null from issues where key = $1 for no key update
	`, *issueKey); err != nil {
		return fmt.Errorf("lock the issue of document %s: %w", artifactID, err)
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

// markDocumentsOwed marks every document the run gave blocks nodes owed a block index, which
// is how the parent reaches document_blocks.parent_block_id: the reconciliation is that
// column's one writer, and this is the mark it drains (refs.RebuildAll sets the same one).
func markDocumentsOwed(ctx context.Context, database *store.Store, documents map[string]struct{}) (int64, error) {
	if len(documents) == 0 {
		return 0, nil
	}
	ids := make([]string, 0, len(documents))
	for id := range documents {
		ids = append(ids, id)
	}
	// In id order, so two runs over one database take the rows in one order.
	sort.Strings(ids)
	owed, err := database.Pool.Exec(ctx, `
		insert into document_block_versions (artifact_id)
		select id::uuid from unnest($1::text[]) as document (id)
		on conflict (artifact_id) do update set version = null
	`, ids)
	if err != nil {
		return 0, fmt.Errorf("mark the documents the backfill touched owed a block index: %w", err)
	}
	return owed.RowsAffected(), nil
}

// blockRefRow is one block_refs row the backfill reads: the pair it holds, the block node it
// is a link out of, and the row's own dates and sequence.
type blockRefRow struct {
	artifactID, blockID  string
	address              string
	kind, toKind, toID   string
	createdAt            time.Time
	sourceSeq            *int64
	event                int64
	blockNodeIsProjected bool
}

// backfillBlockRefs states one tuple per block reference written before the store existed. A
// block_refs row is one link out of a block and its node is that block's, so this writes no
// node of its own: it runs after the blocks, and a row whose block carries no node - a block
// past the address bound, or one with no document_blocks row at all - is skipped and counted.
// A row a version write deleted or wrote after its batch was read is not written
// (writeBlockRefBatch), and is counted nowhere.
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
			if !row.blockNodeIsProjected {
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
		where node_kind is null and octet_length(from_node) <= `+blockBound).Scan(&done.Remaining); err != nil {
		return done, fmt.Errorf("count the block references still naming no node: %w", err)
	}
	done.Complete = done.Remaining == 0
	return done, nil
}

// readBlockRefRows reads one batch of the block references naming no node, by the key a row of
// block_refs is keyed on past its artifact, so the walk never reads one row twice.
func readBlockRefRows(ctx context.Context, database *store.Store, after []any) ([]blockRefRow, error) {
	rows, err := database.Pool.Query(ctx, `
		select r.artifact_id::text, r.block_id, r.from_node, r.kind, r.to_kind, r.to_id,
		       r.created_at, r.source_seq, coalesce(r.source_seq, $6),
		       d.node_kind is not null and d.node_address = r.from_node
		from block_refs r
		left join document_blocks d on d.artifact_id = r.artifact_id and d.block_id = r.block_id
		where r.node_kind is null and octet_length(r.from_node) <= `+blockBound+`
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
		var projected *bool
		if err := rows.Scan(&row.artifactID, &row.blockID, &row.address, &row.kind, &row.toKind,
			&row.toID, &row.createdAt, &row.sourceSeq, &row.event, &projected); err != nil {
			return nil, fmt.Errorf("read a block reference naming no node: %w", err)
		}
		row.blockNodeIsProjected = projected != nil && *projected
		batch = append(batch, row)
	}
	return batch, rows.Err()
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
// are still there, still name no node and are still links out of a block whose row names the
// node they are links out of, locks each of them `for update` in key order, and answers them in
// the batch's own order. The rest of a row is what the batch read: a version write keeps a
// surviving row's dates and source_seq, and a row it inserts names its node (refs'
// replaceBlockRefs), so a row that still names no node is one no version has written since.
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
		join document_blocks d on d.artifact_id = r.artifact_id and d.block_id = r.block_id
		where r.node_kind is null and d.node_kind is not null and d.node_address = r.from_node
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
// owns it, and the event that stored it.
type commentRow struct {
	id        string
	author    model.Actor
	createdAt time.Time
	anchor    string
	owner     string
	event     int64
}

// backfillComments writes a node and the facts its row is the projection of for every comment
// written before the store existed: the two base attributes every node carries, the artifact
// it is anchored to and the artifact that owns it where it has them (api's commentStored), and
// who wrote it, as graphstore.Authored states that (graphstore.AuthorFacts).
func backfillComments(ctx context.Context, database *store.Store) (KindBackfill, error) {
	done := KindBackfill{Kind: commentNodeKind}
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
	if err := database.Pool.QueryRow(ctx,
		`select count(*) from comments where node_kind is null`).Scan(&done.Remaining); err != nil {
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
		       ), $3)
		from comments c
		left join artifacts anchor on anchor.id = (c.anchor->>'artifact_id')::uuid
		left join artifacts owner on owner.id = c.artifact_id
		where c.node_kind is null and c.id > $1::uuid
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
		if err := rows.Scan(&row.id, &author, &row.createdAt, &row.anchor, &row.owner, &row.event); err != nil {
			return nil, fmt.Errorf("read a comment naming no node: %w", err)
		}
		if err := json.Unmarshal(author, &row.author); err != nil {
			return nil, fmt.Errorf("read the author of comment %s: %w", row.id, err)
		}
		batch = append(batch, row)
	}
	return batch, rows.Err()
}

// writeCommentBatch writes one batch's comment nodes, the facts each row projects and who wrote
// each, in one transaction, and answers the nodes and the tuples it stated, as the upper bound
// the report states: an author's node another write already made moves nothing, and a tuple
// stated again under its event appends nothing.
//
// Every node of the batch goes in its one store write, the comments' and their authors', so the
// transaction takes every node row in ascending (kind, id) before it appends its first tuple, and
// names the nodes on the comments rows only after both: the order of the lock order on
// api/inbox_read.go's markInboxReadThreads. Who wrote a comment is what graphstore.Authored
// states of it (graphstore.AuthorFacts): the author's node, made at the first comment of the
// batch that names them, the comment's authored_by tuple and the aliases the server proved.
// Write makes an author's node as Authored's arms make it and moves nothing on one that is
// already there, since every writer of a person or a session node writes it as AuthorFacts does:
// project-visible, live and carrying no attribute.
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
	authors := map[[2]string]struct{}{}
	for _, row := range batch {
		node := graphstore.Node{
			Kind: commentNodeKind, ID: row.id, Supertag: commentSupertag,
			Visibility: graphstore.VisibilityProject, State: graphstore.StateLive,
			MadeAt: row.createdAt, MadeBy: row.author, SourceEvent: row.event,
		}
		nodes = append(nodes, node)
		states := func(kind, toKind, toID string) {
			if toID == "" {
				return
			}
			tuples = append(tuples, graphstore.Tuple{
				FromKind: node.Kind, FromID: node.ID, Kind: kind, ToKind: toKind, ToID: toID,
				At: node.MadeAt, Who: node.MadeBy, SourceEvent: node.SourceEvent, Asserted: true,
			})
		}
		states(tupleVisibility, graphstore.KindValue, graphstore.VisibilityProject)
		states(tupleState, graphstore.KindValue, graphstore.StateLive)
		states(tupleAnchoredTo, "artifact", row.anchor)
		states(tupleOwnedBy, "artifact", row.owner)
		// One write writes a node once, so an author several comments of the batch name is made
		// by the first of them; each comment still states its own authored_by and aliases.
		if author, facts, named := graphstore.AuthorFacts(commentNodeKind, row.id, row.author, row.createdAt, row.event); named {
			if _, made := authors[[2]string{author.Kind, author.ID}]; !made {
				authors[[2]string{author.Kind, author.ID}] = struct{}{}
				nodes = append(nodes, author)
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
	`, ids, addresses, commentNodeKind); err != nil {
		return 0, 0, fmt.Errorf("name the nodes of a comment batch on their rows: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, 0, fmt.Errorf("commit a comment batch: %w", err)
	}
	return int64(len(nodes)), int64(len(tuples)), nil
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
// No role has a graph_store_cutover row, so Complete sets none: it reads whether every role the
// census names holds its node and its page.
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

// readRoleRows reads every role addressedRoles names whose node or page the store does not hold,
// in name order.
func readRoleRows(ctx context.Context, database *store.Store) ([]roleRow, error) {
	rows, err := database.Pool.Query(ctx, `
		select addressed.name, coalesce(addressed.event, $1), coalesce(e.created_at, now()),
		       role.id is null, page.id is null
		from (`+addressedRoles+`) addressed
		left join events e on e.id = addressed.event
		left join graph_nodes role on role.kind = $2::text and role.id = addressed.name
		left join graph_nodes page on page.kind = $3::text and page.id = $2::text || '/' || addressed.name
		where role.id is null or page.id is null
		order by addressed.name
	`, graphstore.NoEvent, graphstore.KindRole, graphstore.KindPage)
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
		claims = append(claims, graphstore.Claim{
			FromKind: graphstore.KindPage, FromID: page, Kind: graphstore.TuplePartOf,
			ToKind: graphstore.KindRole, ToID: row.name,
			At: row.at, Who: backfillActor, SourceEvent: row.event,
		})
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
