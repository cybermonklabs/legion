package graphcensus

import (
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

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
// and marks each document it touched owed this release's index pass with a mark of the marking
// rule rule (writeBlockBatch). It then marks each document no pass of that rule has stated at its
// version (markStaleDocuments): one another release indexed after this build stated it, whose
// rows no longer name every block and link the store holds, and one a build of an earlier rule
// indexed, whose blocks hold none of this rule's facts. A block whose address is past
// refs.MaxBlockNodeBytes is no node of the store by design, so it is neither written nor counted
// as skipped: the writers leave it unprojected too. A row a version write deleted after its batch
// was read is not written either (writeBlockBatch), and is counted nowhere: it is no row of the
// database any more. A row whose document is between a party graphstore.PartiesValue refuses is
// skipped (partiesRefused).
func backfillBlocks(ctx context.Context, database *store.Store, rule int) (KindBackfill, error) {
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
		writable := make([]blockRow, 0, len(rows))
		for _, row := range rows {
			if _, parties := graphstore.SessionScope(row.project, row.session); partiesRefused(parties) {
				done.Skipped++
				continue
			}
			writable = append(writable, row)
		}
		written, tuples, err := writeBlockBatch(ctx, database, writable, rule)
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
	stale, err := markStaleDocuments(ctx, database, rule)
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

// readBlockRows reads one batch of the blocks naming no node (BlockRowsStatement), by their node
// address so the walk is a keyset over a column every row of the batch carries. Each row's event
// is the one that recorded the version it was last written at (docs' versionEventID), and the
// store's sentinel where that is not found.
func readBlockRows(ctx context.Context, database *store.Store, after string) ([]blockRow, error) {
	rows, err := database.Pool.Query(ctx, BlockRowsStatement, after, backfillBatch, graphstore.NoEvent)
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

// BlockRowsStatement is readBlockRows' selection: at most $2 of the block rows naming no node
// whose address fits the store's bound, past the address $1, in address order, each with its
// document's ref_key, when it was written, the event that recorded the version it was last
// written at, or $3, the store's sentinel, where none is found, and its document's project and
// its conversation's session. The documents of the rows it selects are the ones the block batch
// marks (writeBlockBatch), so it is one of the statements that decide what this build marks, and
// it is named so the test that holds the marking rule to them reads it
// (refs.BlockIndexMarkingRule).
//
// Each batch reads document_blocks_naming_no_node (migration 20261119020900) from the address $1
// on: that index holds the rows this walk reads, keyed in its order, and its predicate is the
// walk's own two clauses. node_id's own index is a hash, which keeps no order, so with no other
// index each batch read every row still naming no node and sorted them, and a run took time
// growing with the square of the rows (TestTheBackfillsWalksReadOnlyTheirBatch).
//
// The event is resolved once per (document, changed version) of the batch and joined back to
// the rows, not once per row: every block of one version of one document shares that pair. The
// pairs' lookups are a materialized CTE, so the plan runs them once per batch: inlined, the
// generic plan a prepared statement settles on reran them for each row of the batch, 7,200
// lookups for a batch of 120 rows and 60 pairs. The word `materialized` changes the plan and not
// the rows (TestTheBlockBatchAnswersTheSameRowsWithItsLookupsMaterializedOrNot), so the marking
// rule stays, with its digest recorded anew (docs' markingStatementDigests). Each lookup probes
// events_artifact_version_number (migration 20261119020700) by the document and the version, whose
// expressions it compares as that index writes them, and whose predicate its own type clause
// proves: with no index to serve it, each one read the whole of events, so a run took time growing
// with the square of the data (TestTheBackfillsEventReadsProbeTheirIndexes).
var BlockRowsStatement = `
		with batch as (
			select d.artifact_id, d.block_id, d.node_id, d.created_at, d.changed_version,
			       a.ref_key, a.issue_key, a.project_key, a.session_id
			from document_blocks d
			join artifacts a on a.id = d.artifact_id
			where d.node_kind is null and octet_length(d.node_id) <= ` + addressBound + `
			  and d.node_id > $1
			order by d.node_id
			limit $2
		),
		caused as materialized (
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
	`

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
// (refs' queueStand), with the marking rule rule in store_owed_rule (BlockBatchMarkStatement). The
// null version the release before this one reads too is left alone, since that release would
// drain it, write the rows and state nothing. The mark is an update of a row the document already
// holds: every pass that writes a document's block rows writes that row first, in either release,
// and it goes only with the artifact, whose delete takes the block rows with it. A mark already
// standing keeps its time, so the debt stays dated from the write that first owed it. The mark
// commits with the rows that name their nodes: marked after the whole pass instead, a run stopped
// by an error in a later batch, or killed, would leave the documents of its committed batches
// unmarked, and a second run reads no row a committed batch named, so nothing would mark them
// again. Their document_block_versions rows are the ones lockBlockDocuments already holds, so the
// mark takes no lock out of the store's order.
//
// Each node takes the supertag the typed-node pass gives its block (R26 of
// the issue-tree spec), read under the same locks (blockRowSupertags): its type's, but for
// a reading in an ask nobody has answered, which keeps refs.BlockSupertag.
//
// The count is this batch's own tuples and the base attributes graphstore.QueueWrite states for each
// node its insert makes, which this batch does not state itself - its visibility and its state,
// and its project or its parties where its document has one (statedOf): every node here is new,
// since a row that already names one is never written.
func writeBlockBatch(ctx context.Context, database *store.Store, batch []blockRow, rule int) ([]blockRow, int64, error) {
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
	supertags, err := blockRowSupertags(ctx, tx, held)
	if err != nil {
		return nil, 0, err
	}
	nodes := make([]graphstore.Node, 0, len(held))
	tuples := make([]graphstore.Tuple, 0, len(held))
	addresses := make([]string, 0, len(held))
	blockIDs := make([]string, 0, len(held))
	artifacts := make([]string, 0, len(held))
	for position, row := range held {
		project, parties := graphstore.SessionScope(row.project, row.session)
		record := refs.IndexedVersion{RefKey: row.refKey, Writer: backfillActor, Project: project, Parties: parties}
		node, member, fits := refs.BlockInDocument(record, row.blockID, supertags[position], row.createdAt, row.event)
		if !fits {
			// BlockRowsStatement reads no row whose address is past the store's bound, so a row
			// refs holds past it is one the two disagree on, and the batch writes nothing.
			return nil, 0, fmt.Errorf("block %s of %s is past the address bound the batch's read holds its rows to", row.blockID, row.refKey)
		}
		nodes = append(nodes, node)
		tuples = append(tuples, member)
		addresses = append(addresses, node.ID)
		blockIDs = append(blockIDs, row.blockID)
		artifacts = append(artifacts, row.artifactID)
	}
	if err := store.RunWrite(ctx, tx, func(b *store.WriteBatch) error {
		graphstore.QueueWrite(b, nodes, tuples)
		b.Queue("name the nodes of a block batch on their rows", `
			update document_blocks d
			set node_kind = $4, node_address = b.address
			from unnest($1::text[], $2::text[], $3::text[]) as b (artifact_id, block_id, address)
			where d.artifact_id = b.artifact_id::uuid and d.block_id = b.block_id
		`, artifacts, blockIDs, addresses, refs.BlockNodeKind)
		graphstore.QueueHeads(b, graphstore.HeadsOf(nodes, tuples))
		return nil
	}); err != nil {
		return nil, 0, fmt.Errorf("write the nodes of a block batch: %w", err)
	}
	// In id order and each once, the order lockBlockDocuments took these rows in.
	owed := slices.Clone(artifacts)
	slices.Sort(owed)
	if _, err := tx.Exec(ctx, BlockBatchMarkStatement, slices.Compact(owed), rule); err != nil {
		return nil, 0, fmt.Errorf("mark the documents of a block batch owed this release's block index: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, 0, fmt.Errorf("commit a block batch: %w", err)
	}
	return held, int64(len(tuples)) + statedOf(nodes), nil
}

// BlockBatchMarkStatement is writeBlockBatch's mark of the documents $1, the documents of a block
// batch whose rows it named the nodes of, owed this release's block index at the marking rule $2
// (refs.BlockIndexMarkingRule). Every such document is marked, whatever mark it holds: a mark
// standing there keeps its time, so the debt stays dated from the write that first owed it, and
// keeps the higher of its rule and $2, so a mark of a higher rule stays that rule's to drain.
// Where no mark stands the row takes $2 and now, whatever rule a mark cleared before left on it,
// since the rule is read only where a mark stands (refs.OwedRuleKept).
var BlockBatchMarkStatement = `
	update document_block_versions set store_owed_at = coalesce(store_owed_at, now()),
		store_owed_rule = ` + refs.OwedRuleKept("", "$2::integer") + `
	where artifact_id = any($1::uuid[])
`

// heldBlockRows reads again, under the locks lockBlockDocuments took, which rows of the batch
// are still there and still name no node, locks each of them `for update` in key order, and
// answers them in the batch's own order (HeldBlockRowsStatement). The rest of a row is what the
// batch read: a row that still names no node has been written by no version since, because a
// version names the node of every block it indexes (refs.IndexBlocks).
func heldBlockRows(ctx context.Context, tx pgx.Tx, batch []blockRow) ([]blockRow, error) {
	artifacts := make([]string, 0, len(batch))
	blockIDs := make([]string, 0, len(batch))
	for _, row := range batch {
		artifacts = append(artifacts, row.artifactID)
		blockIDs = append(blockIDs, row.blockID)
	}
	rows, err := tx.Query(ctx, HeldBlockRowsStatement, artifacts, blockIDs)
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

// HeldBlockRowsStatement is heldBlockRows' read of the rows of a batch, the document of $1 and the
// block of $2 beside it, that still name no node, each locked `for update`, in key order. The
// documents of the rows it answers are the ones the block batch marks (BlockBatchMarkStatement),
// so it is one of the statements that decide what this build marks, and it is named so the test
// that holds the marking rule to them reads it (refs.BlockIndexMarkingRule).
const HeldBlockRowsStatement = `
		select d.artifact_id::text, d.block_id
		from document_blocks d
		join unnest($1::text[], $2::text[]) as b (artifact_id, block_id)
			on d.artifact_id = b.artifact_id::uuid and d.block_id = b.block_id
		where d.node_kind is null
		order by d.artifact_id, d.block_id
		for update of d
	`

// blockRowSupertags is, beside each row of held, the supertag the typed-node pass gives its
// block's node (R26 of the issue-tree spec), read in tx under the locks the batch holds:
// its type's (refs.TypeSupertag), but for a reading, whose node carries its type only where it
// stands in a decision, or in an ask block whose ask holds an answer (refs.AskBlockAnsweredSQL),
// and refs.BlockSupertag otherwise. A row a build before the store wrote names no parent, so a
// reading's holder is read off the newest stored version of its block's document
// (refs.BlockDocumentSQL), read back and walked as the typed-node pass walks a version's tree
// (refs.ReadingHolders), and only for a batch that holds a reading. A version that does not read
// back holds no holder, and its readings take refs.BlockSupertag: each document the batch writes is
// marked owed, and its index pass, which reads the version's tree and its asks as the typed-node
// pass does, restates every node whose supertag is not the one its type gives (refs.PlanBlocks).
func blockRowSupertags(ctx context.Context, tx pgx.Tx, held []blockRow) ([]string, error) {
	artifacts := make([]string, len(held))
	blockIDs := make([]string, len(held))
	for position, row := range held {
		artifacts[position], blockIDs[position] = row.artifactID, row.blockID
	}
	rows, err := tx.Query(ctx, `
		select b.position, d.type, `+refs.BlockDocumentSQL("d")+`::text
		from unnest($1::text[], $2::text[]) with ordinality as b (artifact_id, block_id, position)
		join document_blocks d on d.artifact_id = b.artifact_id::uuid and d.block_id = b.block_id
	`, artifacts, blockIDs)
	if err != nil {
		return nil, fmt.Errorf("read the types of the blocks of a batch: %w", err)
	}
	supertags := make([]string, len(held))
	for position := range supertags {
		supertags[position] = refs.BlockSupertag
	}
	var readings []int
	var readingDocuments []string
	for rows.Next() {
		var position int64
		var typ, document string
		if err := rows.Scan(&position, &typ, &document); err != nil {
			rows.Close()
			return nil, fmt.Errorf("read the type of a block of a batch: %w", err)
		}
		if typ == refs.ReadingSupertag {
			readings, readingDocuments = append(readings, int(position)-1), append(readingDocuments, document)
			continue
		}
		supertags[position-1] = refs.TypeSupertag(typ)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read the types of the blocks of a batch: %w", err)
	}
	if len(readings) == 0 {
		return supertags, nil
	}
	documents := slices.Compact(slices.Sorted(slices.Values(readingDocuments)))
	versions, err := tx.Query(ctx, `
		select distinct on (v.artifact_id) v.artifact_id::text, v.markdown
		from artifact_versions v
		where v.artifact_id = any($1::uuid[]) and v.markdown is not null
		order by v.artifact_id, v.number desc
	`, documents)
	if err != nil {
		return nil, fmt.Errorf("read the newest versions of the documents of a batch's readings: %w", err)
	}
	holders := make(map[string]map[string]refs.ReadingHolder, len(documents))
	for versions.Next() {
		var document, markdown string
		if err := versions.Scan(&document, &markdown); err != nil {
			versions.Close()
			return nil, fmt.Errorf("read the newest version of a document of a batch's readings: %w", err)
		}
		if tree, err := pmdoc.ParseRendering(markdown); err == nil {
			holders[document] = refs.ReadingHolders(tree)
		}
	}
	versions.Close()
	if err := versions.Err(); err != nil {
		return nil, fmt.Errorf("read the newest versions of the documents of a batch's readings: %w", err)
	}
	// A reading in a decision is a reading; one in an ask block is one while that block's ask holds
	// an answer, which one statement reads for every such reading of the batch.
	var asked []int
	var askArtifacts, askHolders, askAddresses []string
	for index, position := range readings {
		holder, holds := holders[readingDocuments[index]][held[position].blockID]
		switch {
		case !holds:
		case !holder.Ask:
			supertags[position] = refs.ReadingSupertag
		default:
			asked = append(asked, position)
			askArtifacts = append(askArtifacts, held[position].artifactID)
			askHolders = append(askHolders, holder.ID)
			askAddresses = append(askAddresses, refs.BlockAddress(held[position].refKey, holder.ID))
		}
	}
	if len(asked) == 0 {
		return supertags, nil
	}
	answers, err := tx.Query(ctx, `
		select b.position, `+refs.AskBlockAnsweredSQL("b.artifact_id::uuid", "b.holder", "b.address")+`
		from unnest($1::text[], $2::text[], $3::text[]) with ordinality as b (artifact_id, holder, address, position)
	`, askArtifacts, askHolders, askAddresses)
	if err != nil {
		return nil, fmt.Errorf("read whether the asks of a batch's readings hold an answer: %w", err)
	}
	defer answers.Close()
	for answers.Next() {
		var position int64
		var answered bool
		if err := answers.Scan(&position, &answered); err != nil {
			return nil, fmt.Errorf("read whether the ask of a batch's reading holds an answer: %w", err)
		}
		if answered {
			supertags[asked[position-1]] = refs.ReadingSupertag
		}
	}
	return supertags, answers.Err()
}
