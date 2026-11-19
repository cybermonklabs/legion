package graphcensus

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// referenceProjectionKind is the census's and the backfill's name of the references a document
// makes.
const referenceProjectionKind = "artifact_reference"

// refProjections are the census's kinds of the references an artifact makes, the refs rows whose
// source is an artifact (projections), held triple by triple (projection.tuples): each refs row of
// a document whose artifacts row names its node, as the triple of that node, mentions and the
// row's far end, against every mentions a present document node states now, in both directions.
// The tuple is the row's marker, as an anchor's is (anchorProjections): refs carries no node
// column of its own, so a row of a marked document that no tuple states is a row a process of the
// release before wrote after this build named the document's node, or one whose tuple it never
// retracted where it deleted the row, and either is a difference the backfill's pass writes or the
// census names. A row of a document whose row names no node is a row of the release before's,
// which graph_edges' arm over refs answers (store.ReferenceNotStated) and the census counts as a
// class of its own, and a far end past the store's bound is no tuple, counted as a class of its own
// too. A deployment whose documents cite nothing holds no row, no tuple and no unmarked row of the
// kind, which is no refusal (projection.emptyAgrees).
var refProjections = []projection{{
	name:     referenceProjectionKind,
	nodeKind: graphstore.KindArtifact,
	table:    "refs",
	note: "a document's references: the row side is every refs row of a document whose row names its node, as the" +
		" triple of that node, mentions and the row's far end, against every mentions a present document node states now",
	rows: `
		select 'artifact ' || r.from_id, a.node_kind, a.node_address, '` + graphstore.TupleMentions + `', r.to_kind, r.to_id
		from refs r` + markedReferenceSource + `
		  and octet_length(r.to_id) <= ` + addressBound,
	tuples: heldTuples(`t.kind = '`+graphstore.TupleMentions+`'`, graphstore.KindArtifact),
	pastBound: `
		select count(*) from refs r` + markedReferenceSource + `
		  and octet_length(r.to_id) > ` + addressBound,
	unmarked: `
		select count(*) from refs r
		where r.from_kind = '` + graphstore.KindArtifact + `'
		  and not exists (select 1 from artifacts a where a.id::text = r.from_id and a.node_kind is not null)`,
	emptyAgrees: true,
}}

// markedReferenceSource joins, to the refs row aliased r, the artifacts row a of the document whose
// reference it is, while that row names the document's node, and keeps the rows of a document
// alone. It reads the document by its uuid as text, which artifacts_id_text indexes.
var markedReferenceSource = `
		join artifacts a on a.id::text = r.from_id and a.node_kind is not null
		where r.from_kind = '` + graphstore.KindArtifact + `'`

// heldReference is the newest mentions tuple out of the node of the artifacts row aliased a to the
// far end of the refs row aliased r, with whether it asserts (graphstore.CurrentTupleRead), read
// through graph_tuples_current.
var heldReference = graphstore.CurrentTupleRead(`t.from_kind, t.from_id, t.kind, t.to_kind, t.to_id`, `t.asserted`,
	`t.from_kind = a.node_kind and t.from_id = a.node_address and t.kind = '`+graphstore.TupleMentions+`'
		  and t.to_kind = r.to_kind and t.to_id = r.to_id`)

// owedReferences is the statement shape of the refs rows of a document whose row names its node
// that no current mentions tuple out of that node states: the rows of the kind in the store's bound
// with no tuple asserting their triple (heldReference).
var owedReferences = `
		from refs r` + markedReferenceSource + `
		  and octet_length(r.to_id) <= ` + addressBound + `
		  and not exists (select 1 from (` + heldReference + `) held where held.asserted)`

// heldMentions is every mentions tuple the node of the artifacts row aliased a states, the newest
// of each far end with whether it asserts (graphstore.CurrentTupleRead), read through
// graph_tuples_current.
var heldMentions = graphstore.CurrentTupleRead(`t.to_kind, t.to_id`, `t.to_kind, t.to_id, t.asserted`,
	`t.from_kind = a.node_kind and t.from_id = a.node_address and t.kind = '`+graphstore.TupleMentions+`'`)

// orphanedReferences is the statement shape of the mentions a present document node states now
// whose refs row is gone: a row a process of the release before deleted, which retracts nothing.
// The census reads each as a tuple no row projects (refProjections), whose holders are the present
// nodes of the kind, so the node is held to be one.
var orphanedReferences = `
		from artifacts a
		join graph_nodes node on node.kind = a.node_kind and node.id = a.node_address and node.state <> '` + graphstore.StateGone + `'
		cross join lateral (` + heldMentions + `) held
		where held.asserted and not exists (
		  select 1 from refs r
		  where r.from_kind = '` + graphstore.KindArtifact + `' and r.from_id = a.id::text
		    and r.to_kind = held.to_kind and r.to_id = held.to_id)`

// owedReferenceMoves is every move that levels a document node's mentions with its refs rows, one
// row each as (document, to_kind, to_id, issue, node, created_at, source_seq, asserted): the
// assertion each row of owedReferences owes, dated and sequenced by that row, and the retraction
// each tuple of orphanedReferences owes, which no row dates. A triple is a row with no tuple or a
// tuple with no row, never both, so the two keep one order between them. The pass and its count
// read the same statement, so the count says what the next run would write.
var owedReferenceMoves = `
		select r.from_id as document, r.to_kind, r.to_id, coalesce(a.issue_key, '') as issue, a.node_address as node,
		       r.created_at, r.source_seq, true as asserted` + owedReferences + `
		union all
		select a.id::text, held.to_kind, held.to_id, coalesce(a.issue_key, ''), a.node_address,
		       null::timestamptz, null::bigint, false` + orphanedReferences

// refBackfills are the backfill's passes over the references an artifact makes (backfillPasses),
// after the project pass, whose object pass names every document's node on its row: what each
// document whose row names its node owes its node's mentions (backfillReferences).
var refBackfills = []backfillPass{oneKind(backfillReferences)}

// referenceMove is one move a document's node owes its mentions (owedReferenceMoves): the triple
// it moves, by the document's uuid, with the owner the document stands under, its issue, or the
// document itself where no issue holds it.
type referenceMove struct {
	document, toKind, toID, issue string
}

// backfillReferences levels the mentions of each document whose row names its node with its refs
// rows (owedReferenceMoves). Each row with no tuple asserting its triple takes its mentions tuple,
// out of the document's node, dated at the row's own created_at and carrying its own source_seq as
// its legacy_source_seq, so the store answers the edge at the moment and with the cause
// graph_edges' arm over refs answered it: a row a process of the release before wrote - before the
// object pass named its document's node, or after, beside a document this build wrote - and a row
// whose tuple that release lost are each such a row. Each mentions tuple the node states now whose
// row that release deleted, retracting nothing, is retracted, dated at the run's time, as the
// anchor pass retracts an anchored_in its row no longer names (backfillAnchoredIn). The refs table
// takes no marker column, so the tuple itself is the marker (anchorProjections): the pass finds a
// move owed by comparing the rows with the node's current mentions, walking the moves batch by
// batch in the order of the triple they move, and a second run reads no move a first one made. It
// appends no event: every tuple it states carries the sentinel and is written by the run
// (backfillActor).
func backfillReferences(ctx context.Context, database *store.Store) (KindBackfill, error) {
	done := KindBackfill{Kind: referenceProjectionKind}
	var after referenceMove
	for {
		rows, err := database.Pool.Query(ctx, `
			select owed.document, owed.to_kind, owed.to_id, owed.issue from (`+owedReferenceMoves+`) owed
			where (owed.document, owed.to_kind, owed.to_id) > ($1, $2, $3)
			order by owed.document, owed.to_kind, owed.to_id
			limit $4
		`, after.document, after.toKind, after.toID, backfillBatch)
		if err != nil {
			return done, fmt.Errorf("read the references their documents' nodes do not state: %w", err)
		}
		batch, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (referenceMove, error) {
			var move referenceMove
			err := row.Scan(&move.document, &move.toKind, &move.toID, &move.issue)
			return move, err
		})
		if err != nil {
			return done, fmt.Errorf("read the references their documents' nodes do not state: %w", err)
		}
		if len(batch) == 0 {
			break
		}
		after = batch[len(batch)-1]
		written, err := writeReferenceBatch(ctx, database, batch)
		if err != nil {
			return done, err
		}
		done.Rows += written
		done.Tuples += written
		if len(batch) < backfillBatch {
			break
		}
	}
	if err := database.Pool.QueryRow(ctx, `select count(*) from (`+owedReferenceMoves+`) owed`).Scan(&done.Remaining); err != nil {
		return done, fmt.Errorf("count the references their documents' nodes do not state: %w", err)
	}
	done.Complete = done.Remaining == 0
	return done, nil
}

// writeReferenceBatch makes one batch's moves in one transaction, in the store's lock order
// (api/inbox_read.go's markInboxReadThreads): the owner rows the batch's documents stand under
// first, each once, in ascending key (events.LockOwnerRows), then the batch's moves read again
// under them, then the tuples (graphstore.Write, which writes no node: each document's node is the
// object pass's or its writer's). Every writer of a document's refs rows on this build holds the
// document's owner row - its version index (docs' lockArtifactOwner) and rebuild-refs (refs'
// lockDocumentOwner) - and states the tuple of each row it inserts and the retraction of each it
// deletes in the same transaction, so a move read again under the owner rows is still owed as it
// commits, and one that writer made meanwhile is not read again and states nothing. The read again
// is narrowed to the batch's documents, which each part of the moves reads through an index, and
// keeps the batch's own triples. It answers how many tuples it stated, one per move.
func writeReferenceBatch(ctx context.Context, database *store.Store, batch []referenceMove) (int64, error) {
	var written int64
	if err := pgx.BeginFunc(ctx, database.Pool, func(tx pgx.Tx) error {
		owners := events.OwnerRows{}
		documents := make([]string, 0, len(batch))
		moves := make(map[referenceMove]struct{}, len(batch))
		for _, move := range batch {
			owner := ""
			if move.issue == "" {
				owner = move.document
			}
			owners.Add(move.issue, owner, "the references of document "+move.document)
			if len(documents) == 0 || documents[len(documents)-1] != move.document {
				documents = append(documents, move.document)
			}
			moves[referenceMove{document: move.document, toKind: move.toKind, toID: move.toID}] = struct{}{}
		}
		if err := events.LockOwnerRows(ctx, tx, owners); err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `
			select owed.document, owed.node, owed.to_kind, owed.to_id, owed.created_at, owed.source_seq, owed.asserted
			from (`+owedReferenceMoves+`) owed
			where owed.document = any($1::text[])
			order by owed.document, owed.to_kind, owed.to_id
		`, documents)
		if err != nil {
			return fmt.Errorf("read a batch of references again under their owners: %w", err)
		}
		now := time.Now().UTC()
		var tuples []graphstore.Tuple
		for rows.Next() {
			var document, node, toKind, toID string
			var createdAt *time.Time
			var sourceSeq *int64
			var asserted bool
			if err := rows.Scan(&document, &node, &toKind, &toID, &createdAt, &sourceSeq, &asserted); err != nil {
				rows.Close()
				return fmt.Errorf("read a batch of references again under their owners: %w", err)
			}
			if _, owed := moves[referenceMove{document: document, toKind: toKind, toID: toID}]; !owed {
				continue
			}
			at := now
			if asserted {
				at = *createdAt
			}
			tuple := graphstore.ArtifactMentions(node, toKind, toID, asserted, backfillActor, at)
			tuple.LegacySourceSeq = sourceSeq
			tuples = append(tuples, tuple)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return fmt.Errorf("read a batch of references again under their owners: %w", err)
		}
		if len(tuples) == 0 {
			return nil
		}
		if err := graphstore.Write(ctx, tx, nil, tuples); err != nil {
			return err
		}
		written = int64(len(tuples))
		return nil
	}); err != nil {
		return 0, fmt.Errorf("write a batch of references: %w", err)
	}
	return written, nil
}
