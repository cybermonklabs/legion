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
// too. A retired document's rows are set aside, as the census of the documents sets the document
// aside (versionProjections): its row stays as the record of what it held, and its node is gone on
// purpose (retiredDocument), so its references are no rows a present node owes its mentions. A
// deployment whose documents cite nothing holds no row, no tuple and no unmarked row of the kind,
// which is no refusal (projection.emptyAgrees).
var refProjections = []projection{{
	name:     referenceProjectionKind,
	nodeKind: graphstore.KindArtifact,
	table:    "refs",
	note: "a document's references: the row side is every refs row of a document whose row names its node and that is not" +
		" retired, as the triple of that node, mentions and the row's far end, against every mentions a present document node states now",
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
}, {
	name:     referenceDateProjectionKind,
	nodeKind: graphstore.KindArtifact,
	table:    "refs",
	note: "a document's references dated: the row side is every refs row of a document whose row names its node and that is" +
		" not retired, whose current mentions tuple asserts its far end, with its created_at and source_seq, against each such" +
		" tuple of a present document node with its time and legacy_source_seq; every other difference is artifact_reference's",
	rows: `
		select 'artifact ' || r.from_id, a.node_kind, a.node_address, '` + graphstore.TupleMentions + `', r.to_kind,
		       r.to_id || ' at ' || ` + referenceDate("r.created_at", "r.source_seq") + `
		from refs r` + markedReferenceSource + `
		  and octet_length(r.to_id) <= ` + addressBound + `
		  and exists (select 1 from (` + heldReference + `) held where held.asserted)`,
	tuples: `
		select a.node_kind, a.node_address, '` + graphstore.TupleMentions + `', held.to_kind,
		       held.to_id || ' at ' || ` + referenceDate("held.at", "held.legacy_source_seq") + `
		from artifacts a
		join graph_nodes node on node.kind = a.node_kind and node.id = a.node_address and node.state <> '` + graphstore.StateGone + `'
		cross join lateral (` + heldDatedMentions + `) held
		where a.node_kind is not null and not ` + retiredDocument("a") + ` and held.asserted
		  and exists (
		    select 1 from refs r
		    where r.from_kind = '` + graphstore.KindArtifact + `' and r.from_id = a.id::text
		      and r.to_kind = held.to_kind and r.to_id = held.to_id)`,
	emptyAgrees: true,
}}

// referenceDateProjectionKind is the census's name of the date a document's mention carries, held
// to the refs row of the pair (refProjections' second kind): graph_edges answers a document's
// mention at its tuple's time and legacy_source_seq once the boot step has deleted the tuple arm's
// read of the row (store.GraphEdgesIssueTreeRetired), so each current mentions tuple whose row
// stands is held to that row's created_at and source_seq, a tuple and a row dated otherwise each a
// difference. A row with no tuple and a tuple with no row are artifact_reference's differences,
// and neither side here holds them, so one defect is counted once.
const referenceDateProjectionKind = "artifact_reference_date"

// referenceDate writes the date of a mention, its time at and its sequence seq, as text both sides
// of the date kind compare: the time in UTC to the microsecond, and the sequence or none.
func referenceDate(at, seq string) string {
	return `to_char(` + at + ` at time zone 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS.US"Z"') || ' seq ' || coalesce(` + seq + `::text, 'none')`
}

// heldDatedMentions is every mentions tuple the node of the artifacts row aliased a states, the
// newest of each far end with whether it asserts, its time and its legacy_source_seq
// (graphstore.CurrentTupleRead), read through graph_tuples_current.
var heldDatedMentions = graphstore.CurrentTupleRead(`t.to_kind, t.to_id`, `t.to_kind, t.to_id, t.asserted, t.at, t.legacy_source_seq`,
	`t.from_kind = a.node_kind and t.from_id = a.node_address and t.kind = '`+graphstore.TupleMentions+`'`)

// markedReferenceSource joins, to the refs row aliased r, the artifacts row a of the document whose
// reference it is, while that row names the document's node and the document is not retired
// (retiredDocument), and keeps the rows of a document alone. It reads the document by its uuid as
// text, which artifacts_id_text indexes.
var markedReferenceSource = `
		join artifacts a on a.id::text = r.from_id and a.node_kind is not null and not ` + retiredDocument("a") + `
		where r.from_kind = '` + graphstore.KindArtifact + `'`

// heldReference is the newest mentions tuple out of the node of the artifacts row aliased a to the
// far end of the refs row aliased r, with whether it asserts, its time and its legacy_source_seq
// (graphstore.CurrentTupleRead), read through graph_tuples_current.
var heldReference = graphstore.CurrentTupleRead(`t.from_kind, t.from_id, t.kind, t.to_kind, t.to_id`, `t.asserted, t.at, t.legacy_source_seq`,
	`t.from_kind = a.node_kind and t.from_id = a.node_address and t.kind = '`+graphstore.TupleMentions+`'
		  and t.to_kind = r.to_kind and t.to_id = r.to_id`)

// referenceDated is the test that the current mentions tuple aliased held asserts the far end of
// the refs row aliased r at that row's own date: its time is the row's created_at and its
// legacy_source_seq the row's source_seq. graph_edges answers a document's mention at its tuple's
// date once the boot step has deleted the tuple arm's read of the row
// (store.GraphEdgesIssueTreeRetired), so a tuple dated otherwise is owed a statement at the row's
// date (owedReferences), and the census counts it as a difference (refDateProjections).
const referenceDated = `held.asserted and held.at = r.created_at and held.legacy_source_seq is not distinct from r.source_seq`

// owedReferences is the statement shape of the refs rows of a document whose row names its node
// that no current mentions tuple out of that node states at the row's date: the rows of the kind
// in the store's bound with no tuple asserting their triple, and those whose tuple asserts it at
// another time or with another legacy_source_seq (referenceDated) - a row rebuild-refs inserted,
// which a later version's event stamped after its tuple was appended. Each is owed its mentions
// tuple at its own date.
var owedReferences = `
		from refs r` + markedReferenceSource + `
		  and octet_length(r.to_id) <= ` + addressBound + `
		  and not exists (select 1 from (` + heldReference + `) held where ` + referenceDated + `)`

// heldMentions is every mentions tuple the node of the artifacts row aliased a states, the newest
// of each far end with whether it asserts (graphstore.CurrentTupleRead), read through
// graph_tuples_current.
var heldMentions = graphstore.CurrentTupleRead(`t.to_kind, t.to_id`, `t.to_kind, t.to_id, t.asserted`,
	`t.from_kind = a.node_kind and t.from_id = a.node_address and t.kind = '`+graphstore.TupleMentions+`'`)

// orphanedReferences is the statement shape of the mentions a present document node states now
// whose refs row is gone: a row a process of the release before deleted, which retracts nothing.
// The census reads each as a tuple no row projects (refProjections), whose holders are the present
// nodes of the kind, so the node is held to be one. The test that the row is gone is a scalar
// subquery with a limit, which Postgres runs for each mention by refs_pkey rather than pulling it up
// into an anti-join: as a `not exists`, the planner took nearly every mention to have its row, as
// most have, and read every document past the walk's cursor for each batch of
// orphanedReferenceTuplesStatement under a custom plan, sorting them, however many it owed
// (TestEveryTopicPassWalksOneBatchThroughAnIndex).
var orphanedReferences = `
		from artifacts a
		join graph_nodes node on node.kind = a.node_kind and node.id = a.node_address and node.state <> '` + graphstore.StateGone + `'
		cross join lateral (` + heldMentions + `) held
		where held.asserted and coalesce((
		  select false from refs r
		  where r.from_kind = '` + graphstore.KindArtifact + `' and r.from_id = a.id::text
		    and r.to_kind = held.to_kind and r.to_id = held.to_id
		  limit 1), true)`

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
// graph_edges' arm over refs answered it: a row a process of the release before wrote beside a
// document whose row names its node - the object pass states the rows a document holds as it names
// its node (nameArtifactNodes), and a row written after that states none - and a row whose tuple
// that release lost are each such a row. Each mentions tuple the node states now whose
// row that release deleted, retracting nothing, is retracted, dated at the run's time, as the
// anchor pass retracts an anchored_in its row no longer names (backfillAnchoredIn). The refs table
// takes no marker column, so the tuple itself is the marker (anchorProjections): the pass finds a
// move owed by comparing the rows with the node's current mentions, in two walks, each batch by
// batch from the last batch's key (referenceWalks): the rows that owe their assertion, then the
// tuples that owe their retraction. A second run reads no move a first one made. It appends no
// event: every tuple it states carries the sentinel and is written by the run (backfillActor).
func backfillReferences(ctx context.Context, database *store.Store) (KindBackfill, error) {
	done := KindBackfill{Kind: referenceProjectionKind}
	for _, walk := range referenceWalks {
		var after referenceMove
		for {
			rows, err := database.Pool.Query(ctx, walk, after.document, after.toKind, after.toID, backfillBatch)
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
	}
	if err := database.Pool.QueryRow(ctx, `select count(*) from (`+owedReferenceMoves+`) owed`).Scan(&done.Remaining); err != nil {
		return done, fmt.Errorf("count the references their documents' nodes do not state: %w", err)
	}
	done.Complete = done.Remaining == 0
	return done, nil
}

// owedReferenceRowsStatement and orphanedReferenceTuplesStatement are backfillReferences' two
// walks, each a selection of at most $4 moves past the triple ($1, $2, $3), in the order of the
// triple, each with the document's issue: the rows of owedReferences, which read refs_pkey from the
// triple on, and the tuples of orphanedReferences, which read artifacts_id_text from the document
// on and each document's mentions through graph_tuples_as_of_out. Each walk has a cursor of its
// own, so a run reads each row and each document once however many batches it takes. One walk of
// both kinds of move in one order (owedReferenceMoves) read every row and every document past its
// cursor for each batch, and every document's mentions with them, since an order across the two
// parts is one no index keeps (TestEveryTopicPassWalksOneBatchThroughAnIndex).
var (
	owedReferenceRowsStatement = `
	select r.from_id, r.to_kind, r.to_id, coalesce(a.issue_key, '')` + owedReferences + `
	  and (r.from_id, r.to_kind, r.to_id) > ($1, $2, $3)
	order by r.from_id, r.to_kind, r.to_id
	limit $4`
	orphanedReferenceTuplesStatement = `
	select a.id::text, held.to_kind, held.to_id, coalesce(a.issue_key, '')` + orphanedReferences + `
	  and a.id::text >= $1 and (a.id::text, held.to_kind, held.to_id) > ($1, $2, $3)
	order by a.id::text, held.to_kind, held.to_id
	limit $4`
	referenceWalks = []string{owedReferenceRowsStatement, orphanedReferenceTuplesStatement}
)

// writeReferenceBatch makes one batch's moves in one transaction, in the store's lock order
// (api/inbox_read.go's markInboxReadThreads): the owner rows the batch's documents stand under
// first, each once, in ascending key (events.LockOwnerRows), then the batch's moves read again
// under them, then the tuples (graphstore.QueueWrite, which writes no node: each document's node is the
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
			owners.Add(move.issue, owner)
			if len(documents) == 0 || documents[len(documents)-1] != move.document {
				documents = append(documents, move.document)
			}
			moves[referenceMove{document: move.document, toKind: move.toKind, toID: move.toID}] = struct{}{}
		}
		if err := events.LockOwnerRows(ctx, tx, owners); err != nil {
			return err
		}
		tuples, err := documentMentionMoves(ctx, tx, documents, time.Now().UTC(), func(move referenceMove) bool {
			_, owed := moves[move]
			return owed
		})
		if err != nil {
			return fmt.Errorf("read a batch of references again under their owners: %w", err)
		}
		if len(tuples) == 0 {
			return nil
		}
		if err := store.RunWrite(ctx, tx, func(b *store.WriteBatch) error {
			graphstore.QueueWrite(b, nil, tuples)
			graphstore.QueueHeads(b, graphstore.HeadsOf(nil, tuples))
			return nil
		}); err != nil {
			return err
		}
		written = int64(len(tuples))
		return nil
	}); err != nil {
		return 0, fmt.Errorf("write a batch of references: %w", err)
	}
	return written, nil
}

// documentMentionMoves reads, through tx, every move the nodes of documents - their uuids, each
// once - owe their mentions (owedReferenceMoves), where keep, when it is not nil, keeps the move,
// and answers the tuple each makes, as the backfill's pass states it: the assertion a refs row
// with no tuple owes, out of the document's node, dated at the row's own created_at and carrying
// its source_seq as its legacy_source_seq, and the retraction a tuple whose row is gone owes,
// dated at now, each written by the run (backfillActor) at the sentinel. Its caller holds the
// owner rows the documents stand under, so what it reads is what it writes.
func documentMentionMoves(ctx context.Context, tx pgx.Tx, documents []string, now time.Time, keep func(referenceMove) bool) ([]graphstore.Tuple, error) {
	rows, err := tx.Query(ctx, `
		select owed.document, owed.node, owed.to_kind, owed.to_id, owed.created_at, owed.source_seq, owed.asserted
		from (`+owedReferenceMoves+`) owed
		where owed.document = any($1::text[])
		order by owed.document, owed.to_kind, owed.to_id
	`, documents)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var tuples []graphstore.Tuple
	for rows.Next() {
		var document, node, toKind, toID string
		var createdAt *time.Time
		var sourceSeq *int64
		var asserted bool
		if err := rows.Scan(&document, &node, &toKind, &toID, &createdAt, &sourceSeq, &asserted); err != nil {
			return nil, err
		}
		if keep != nil && !keep(referenceMove{document: document, toKind: toKind, toID: toID}) {
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
	return tuples, rows.Err()
}
