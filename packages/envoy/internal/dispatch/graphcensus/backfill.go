package graphcensus

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"time"

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

// backfillEventType is the type of the event the backfill appends for itself, and
// backfillActor who it records as the writer of every node and tuple it writes. The run is
// nobody's act: a row written before the store existed has a writer of its own only where the
// row itself records one, and inventing one from anywhere else would state a fact no row holds.
const backfillEventType = "graph.backfilled"

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
	// Event is the id of the event the run appended for itself, which is the cause every
	// node and tuple it wrote carries where the row's own event cannot be found.
	Event int64
	Kinds []KindBackfill
}

// Backfill writes the nodes and tuples of the comments, document blocks and block references
// written before the node and tuple store existed: every row of the three projected tables
// that names no node (migration 20261109022248 added the columns and filled none of them).
//
// It walks each kind in batches, writing a batch's nodes, its tuples and the projection
// columns that name them in one transaction, so a row and its node commit together or not at
// all, exactly as a live write does. It appends one event of its own first and uses it as the
// cause of every fact whose row's own event cannot be found: a comment's is the comment.created
// that stored it, a block's the event that recorded the version its row was written at, and a
// block reference's is its own source_seq, which is that event's id already.
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
	event, err := appendBackfillEvent(ctx, database)
	if err != nil {
		return report, err
	}
	report.Event = event
	fmt.Fprintf(out, "backfill-graph-store: event=%d\n", event)
	for _, kind := range []struct {
		name  string
		write func(context.Context, *store.Store, int64) (KindBackfill, error)
	}{
		{refs.BlockNodeKind, backfillBlocks},
		{"block_ref", backfillBlockRefs},
		{commentNodeKind, backfillComments},
	} {
		done, err := kind.write(ctx, database, event)
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

// appendBackfillEvent writes the run's own event, the cause it gives a fact whose row's own
// event cannot be found. It names no owner, which the events table allows
// (events_one_owner is num_nonnulls(...) <= 1), because the run belongs to no issue, document
// or project; it is published already, so the outbox never claims it, and it notifies nobody.
func appendBackfillEvent(ctx context.Context, database *store.Store) (int64, error) {
	actor, err := json.Marshal(backfillActor)
	if err != nil {
		return 0, fmt.Errorf("encode the backfill's actor: %w", err)
	}
	var id int64
	err = database.Pool.QueryRow(ctx, `
		insert into events (seq, type, actor, payload, notify, published_at)
		values (0, $1, $2, '{}'::jsonb, false, now())
		returning id
	`, backfillEventType, actor).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("append the backfill's own event: %w", err)
	}
	return id, nil
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
// counted as skipped: the writers leave it unprojected too.
func backfillBlocks(ctx context.Context, database *store.Store, event int64) (KindBackfill, error) {
	done := KindBackfill{Kind: refs.BlockNodeKind}
	documents := map[string]struct{}{}
	after := ""
	for {
		rows, err := readBlockRows(ctx, database, after, event)
		if err != nil {
			return done, err
		}
		if len(rows) == 0 {
			break
		}
		after = rows[len(rows)-1].address
		written, err := writeBlockBatch(ctx, database, rows)
		if err != nil {
			return done, err
		}
		done.Rows += int64(len(rows))
		done.Nodes += int64(len(rows))
		done.Tuples += written
		for _, row := range rows {
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
// that recorded the version it was last written at (docs' versionEventID), and the run's own
// event where that is not found.
func readBlockRows(ctx context.Context, database *store.Store, after string, event int64) ([]blockRow, error) {
	rows, err := database.Pool.Query(ctx, `
		select d.artifact_id::text, d.block_id, d.node_id, a.ref_key, d.created_at,
		       coalesce((
			select e.id from events e
			where coalesce(e.issue_key, e.artifact_id::text, e.project_key)
			      = coalesce(a.issue_key, a.id::text, a.project_key)
			  and e.type = 'artifact.version'
			  and e.payload->>'artifact_id' = a.id::text
			  and e.payload->'version'->>'number' = d.changed_version::text
			order by e.id desc limit 1
		       ), $3)
		from document_blocks d
		join artifacts a on a.id = d.artifact_id
		where d.node_kind is null and octet_length(d.node_id) <= `+blockBound+`
		  and d.node_id > $1
		order by d.node_id
		limit $2
	`, after, backfillBatch, event)
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

// writeBlockBatch writes one batch's nodes and in_document tuples and names them on the rows,
// in one transaction. The nodes go in before the projection columns: the foreign key on
// (node_kind, node_address) is checked inside the statement that writes them.
func writeBlockBatch(ctx context.Context, database *store.Store, batch []blockRow) (int64, error) {
	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("begin the block batch: %w", err)
	}
	defer tx.Rollback(ctx)
	nodes := make([]graphstore.Node, 0, len(batch))
	tuples := make([]graphstore.Tuple, 0, len(batch))
	addresses := make([]string, 0, len(batch))
	blockIDs := make([]string, 0, len(batch))
	artifacts := make([]string, 0, len(batch))
	for _, row := range batch {
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
		return 0, fmt.Errorf("write the nodes of a block batch: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		update document_blocks d
		set node_kind = $4, node_address = b.address
		from unnest($1::text[], $2::text[], $3::text[]) as b (artifact_id, block_id, address)
		where d.artifact_id = b.artifact_id::uuid and d.block_id = b.block_id
	`, artifacts, blockIDs, addresses, refs.BlockNodeKind); err != nil {
		return 0, fmt.Errorf("name the nodes of a block batch on their rows: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("commit a block batch: %w", err)
	}
	return int64(len(tuples)), nil
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
//
// The row's own source_seq is the event that stamped it, which is both the cause of its tuple
// and that tuple's legacy_source_seq; a row with none takes the run's own event as its cause
// and keeps no legacy sequence, exactly as the graph_edges arm over block_refs answers null
// for it.
func backfillBlockRefs(ctx context.Context, database *store.Store, event int64) (KindBackfill, error) {
	done := KindBackfill{Kind: "block_ref"}
	after := []any{"", "", "", ""}
	for {
		batch, err := readBlockRefRows(ctx, database, after, event)
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
			if err := writeBlockRefBatch(ctx, database, writable); err != nil {
				return done, err
			}
			done.Rows += int64(len(writable))
			done.Tuples += int64(len(writable))
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
func readBlockRefRows(ctx context.Context, database *store.Store, after []any, event int64) ([]blockRefRow, error) {
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
	`, after[0], after[1], after[2], after[3], backfillBatch, event)
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

// writeBlockRefBatch appends one batch's tuples and names the block's node on each row, in one
// transaction. The nodes are already there - the blocks ran first - so this writes none, and
// the call still goes through the store's own writer, which is what holds the two sides of a
// projection in one transaction.
func writeBlockRefBatch(ctx context.Context, database *store.Store, batch []blockRefRow) error {
	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin the block reference batch: %w", err)
	}
	defer tx.Rollback(ctx)
	tuples := make([]graphstore.Tuple, 0, len(batch))
	artifacts := make([]string, 0, len(batch))
	blockIDs := make([]string, 0, len(batch))
	kinds := make([]string, 0, len(batch))
	toKinds := make([]string, 0, len(batch))
	toIDs := make([]string, 0, len(batch))
	addresses := make([]string, 0, len(batch))
	for _, row := range batch {
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
		return fmt.Errorf("append the tuples of a block reference batch: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		update block_refs r
		set node_kind = $7, node_address = p.address
		from unnest($1::text[], $2::text[], $3::text[], $4::text[], $5::text[], $6::text[])
			as p (artifact_id, block_id, kind, to_kind, to_id, address)
		where r.artifact_id = p.artifact_id::uuid and r.block_id = p.block_id
		  and r.kind = p.kind and r.to_kind = p.to_kind and r.to_id = p.to_id
	`, artifacts, blockIDs, kinds, toKinds, toIDs, addresses, refs.BlockNodeKind); err != nil {
		return fmt.Errorf("name the nodes of a block reference batch on their rows: %w", err)
	}
	return tx.Commit(ctx)
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
// the authored_by tuple of whoever wrote it with that author's own node (graphstore.Authored).
func backfillComments(ctx context.Context, database *store.Store, event int64) (KindBackfill, error) {
	done := KindBackfill{Kind: commentNodeKind}
	after := "00000000-0000-0000-0000-000000000000"
	for {
		batch, err := readCommentRows(ctx, database, after, event)
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

func readCommentRows(ctx context.Context, database *store.Store, after string, event int64) ([]commentRow, error) {
	rows, err := database.Pool.Query(ctx, `
		select c.id::text, c.author, c.created_at,
		       coalesce(anchor.ref_key, ''), coalesce(owner.ref_key, ''),
		       coalesce((
			select e.id from events e
			where coalesce(e.issue_key, e.artifact_id::text, e.project_key)
			      = coalesce(c.issue_key, c.artifact_id::text)
			  and e.type = 'comment.created'
			  and e.payload->'comment'->>'id' = c.id::text
			order by e.id limit 1
		       ), $3)
		from comments c
		left join artifacts anchor on anchor.id = (c.anchor->>'artifact_id')::uuid
		left join artifacts owner on owner.id = c.artifact_id
		where c.node_kind is null and c.id > $1::uuid
		order by c.id
		limit $2
	`, after, backfillBatch, event)
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

// writeCommentBatch writes one batch's comment nodes, the facts each row projects and each
// author, in one transaction.
//
// The authors go in after the comments and in ascending (kind, id), the order every write of
// this store takes its node rows in (graphstore.writeNodes): a person sorts after a comment
// and a session after a person, so the whole transaction takes its node rows in that one
// order and cannot hold what another writer waits for.
func writeCommentBatch(ctx context.Context, database *store.Store, batch []commentRow) (int64, int64, error) {
	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		return 0, 0, fmt.Errorf("begin the comment batch: %w", err)
	}
	defer tx.Rollback(ctx)
	nodes := make([]graphstore.Node, 0, len(batch))
	var tuples []graphstore.Tuple
	ids := make([]string, 0, len(batch))
	addresses := make([]string, 0, len(batch))
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
		ids = append(ids, row.id)
		addresses = append(addresses, row.id)
	}
	if err := graphstore.Write(ctx, tx, nodes, tuples); err != nil {
		return 0, 0, fmt.Errorf("write the nodes of a comment batch: %w", err)
	}
	authored := append([]commentRow(nil), batch...)
	sort.Slice(authored, func(i, j int) bool {
		leftKind, leftID, _ := graphstore.AuthorOf(authored[i].author)
		rightKind, rightID, _ := graphstore.AuthorOf(authored[j].author)
		if leftKind != rightKind {
			return leftKind < rightKind
		}
		return leftID < rightID
	})
	var authors int64
	for _, row := range authored {
		if _, _, named := graphstore.AuthorOf(row.author); !named {
			continue
		}
		if err := graphstore.Authored(ctx, tx, commentNodeKind, row.id, row.author,
			row.createdAt, row.event); err != nil {
			return 0, 0, err
		}
		authors++
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
	// Each author costs a node the store may already hold and one authored_by tuple; the
	// node count is the comments' own plus one per author written, as the upper bound the
	// report states.
	return int64(len(nodes)) + authors, int64(len(tuples)) + authors, nil
}
