package graphcensus

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// What an approval request asks approval for is a tuple (R14 of the issue-tree spec): one
// approves tuple out of the request's ask node to each query version node it names, by the
// version node's address (graphstore.VersionID). The tuples are the record, and the asks row's
// approval.documents is their projection for good: each entry holds the artifact uuid and the
// version integer of its version node, written in the transaction that appends or moves the tuple
// (api's openApprovalAsk, docs' rewriteApprovalAsk). The request also pins, as a value of its ask
// node, the store revision it was taken at, the head's newest graph_tuples.seq (refs.HeadCut). The
// names live here because docs and api, which write them, import this package, and the census and
// the backfill below, which read them, live in it.
const (
	TupleApproves           = "approves"
	AttributeRequestReadSeq = "read_seq"
)

// ApprovesTuple is the approves tuple out of the ask node askID to version version of the document
// whose ref_key is refKey, asserted or retracted, dated at and written by who, at the sentinel
// cause, which the writer's cause replaces (graphstore.QueueCaused). ok is false where the version
// node's address is past the store's bound, which is no node (graphstore.AddressFits), so nothing
// is stated of it.
func ApprovesTuple(askID, refKey string, version int, who model.Actor, at time.Time, asserted bool) (graphstore.Tuple, bool) {
	node := graphstore.VersionID(refKey, version)
	if !graphstore.AddressFits(node) {
		return graphstore.Tuple{}, false
	}
	return graphstore.Tuple{
		FromKind: graphstore.KindAsk, FromID: askID, Kind: TupleApproves,
		ToKind: graphstore.KindVersion, ToID: node,
		At: at, Who: who, SourceEvent: graphstore.NoEvent, Asserted: asserted,
	}, true
}

// approvedVersions joins, to the asks row aliased k, approved: each entry of its approval's
// documents, as the artifact id the entry names and the address of the version node it names, its
// document's ref_key, `@v` and its version (graphstore.VersionID). asks_approval_kind_check holds
// every entry to an artifact uuid and an integer version, so each casts.
const approvedVersions = `
		cross join lateral (
			select document ->> 'artifact_id' as artifact_id, d.ref_key || '@v' || (document ->> 'version') as node
			from jsonb_array_elements(k.approval -> 'documents') document
			join artifacts d on d.id = (document ->> 'artifact_id')::uuid
		) approved`

// approvalProjections are the census's kind of an approval request's approves tuples
// (projections), held triple by triple (projection.tuples): each entry of the approval of every
// approval ask whose row names its node, as the triple of that node, approves and the version node
// the entry names, against every approves tuple a present ask node states now, in both directions.
// So a row moved to a version its node does not approve reads as the row it states and the tuple
// it left, and a tuple no entry names reads at the tuple. A row naming no node is one whose ask's
// places pass the store's bound, or one a process of the release before wrote before the asks'
// backfill gave it its node; the census counts it as a class of its own. A deployment that holds
// no approval request holds no row and no tuple of the kind, which is no refusal
// (projection.emptyAgrees).
var approvalProjections = []projection{{
	name:     TupleApproves,
	nodeKind: graphstore.KindAsk,
	table:    "asks.approval",
	note: "an approval request's documents: the row side is every entry of approval.documents of every approval ask" +
		" whose row names its node, as the triple of that node, approves and the version node the entry names" +
		" (<ref_key>@v<version>), against every approves tuple an ask node not gone states now",
	rows: `
		select 'ask ' || k.id::text || ' document ' || approved.artifact_id, k.node_kind, k.node_address,
		       '` + TupleApproves + `', '` + graphstore.KindVersion + `', approved.node
		from asks k` + approvedVersions + `
		where k.kind = 'approval' and k.node_kind is not null and octet_length(approved.node) <= ` + addressBound,
	tuples: heldTuples(`t.kind = '`+TupleApproves+`'`, graphstore.KindAsk),
	pastBound: `
		select count(*) from asks k` + approvedVersions + `
		where k.kind = 'approval' and k.node_kind is not null and octet_length(approved.node) > ` + addressBound,
	unmarked: `
		select count(*) from asks k where k.kind = 'approval' and k.node_kind is null`,
	emptyAgrees: true,
}}

// approvalBackfills are the backfill's passes over the approvals (backfillPasses): the approves
// tuples each stored approval request owes (backfillApproves), after the asks' passes gave every
// ask its node, and the pass over carried reviews (backfillReviewCarries).
var approvalBackfills = []backfillPass{oneKind(backfillApproves), oneKind(backfillReviewCarries)}

// heldApproves is every approves tuple the node of the asks row aliased k states, the newest tuple
// of each far end with whether it asserts (graphstore.CurrentTupleRead), read through
// graph_tuples_current.
var heldApproves = graphstore.CurrentTupleRead(`t.kind, t.to_kind, t.to_id`, `t.kind, t.to_kind, t.to_id, t.asserted`,
	`t.from_kind = k.node_kind and t.from_id = k.node_address and t.to_kind <> '`+graphstore.KindValue+`'
		and t.kind = '`+TupleApproves+`'`)

// approvesMoves is, for the asks row aliased k, what levels its node's approves tuples with the
// row, as (to_kind, node, asserted): an approves to each version node an entry of its approval names
// where its node states none to it now, and a retraction of each approves its node states now to a
// version node no entry names. A version node past the store's bound is no node, so it is stated
// nowhere.
var approvesMoves = `
		select '` + graphstore.KindVersion + `' as to_kind, approved.node as node, true as asserted
		from (select 1) one` + approvedVersions + `
		where octet_length(approved.node) <= ` + addressBound + `
		  and not exists (select 1 from (` + heldApproves + `) held
			where held.asserted and held.to_kind = '` + graphstore.KindVersion + `' and held.to_id = approved.node)
		union all
		select held.to_kind, held.to_id, false from (` + heldApproves + `) held
		where held.asserted and not exists (
			select 1 from (select 1) one` + approvedVersions + `
			where held.to_kind = '` + graphstore.KindVersion + `' and approved.node = held.to_id)`

// owedApproves is the statement shape of the approval asks whose node does not state the approves
// tuples their row's documents name: rows naming their node that owe a move (approvesMoves). The
// pass and its count read the same predicate, so the count says what the next run would write.
var owedApproves = `
		from asks k
		where k.kind = 'approval' and k.node_kind is not null and exists (` + approvesMoves + `)`

// owedApprovesRowsStatement is backfillApproves's selection: at most $2 of the approval asks whose
// node owes a move of its approves tuples (owedApproves), past the ask $1, in id order, each with
// its issue or its document. Each batch reads asks_pkey from the id on, so a run reads each ask
// once however many batches it takes.
var owedApprovesRowsStatement = `
	select k.id::text, coalesce(k.issue_key, ''), coalesce(k.artifact_id::text, '') ` + owedApproves + `
	  and k.id > $1::uuid
	order by k.id
	limit $2`

// backfillApproves gives each stored approval request the approves tuples its row's documents name
// (R14 of the issue-tree spec, R21's backfill of every kind the census holds): an approves to each
// version node an entry names that its node does not state now, dated at the row's last rewrite,
// its edited_at, or where it has none its created_at - so a request opened before this release
// takes its tuples at the time it last named those versions - and a retraction, dated at the run's
// time, of each approves its node states to a version node no entry names: a request a process of
// the release before moved to a later version after this build stated the tuple. The row takes no
// marker column, so the tuples themselves are the marker: the pass finds a request owed by comparing
// its row with its node's current approves tuples, walking the rows batch by batch in their primary
// key's order, and a second run reads no request a first one levelled. It appends no event: every
// fact it states carries the sentinel and is written by the run (backfillActor).
func backfillApproves(ctx context.Context, database *store.Store) (KindBackfill, error) {
	done := KindBackfill{Kind: TupleApproves}
	after := zeroUUID
	for {
		rows, err := database.Pool.Query(ctx, owedApprovesRowsStatement, after, backfillBatch)
		if err != nil {
			return done, fmt.Errorf("read the approval requests whose node does not state their documents: %w", err)
		}
		batch, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (askBatchRow, error) {
			var ask askBatchRow
			err := row.Scan(&ask.askID, &ask.issue, &ask.document)
			return ask, err
		})
		if err != nil {
			return done, fmt.Errorf("read the approval requests whose node does not state their documents: %w", err)
		}
		if len(batch) == 0 {
			break
		}
		after = batch[len(batch)-1].askID
		levelled, tuples, err := writeApprovesBatch(ctx, database, batch)
		if err != nil {
			return done, err
		}
		done.Rows += levelled
		done.Tuples += tuples
		if len(batch) < backfillBatch {
			break
		}
	}
	if err := database.Pool.QueryRow(ctx, `select count(*) `+owedApproves).Scan(&done.Remaining); err != nil {
		return done, fmt.Errorf("count the approval requests whose node does not state their documents: %w", err)
	}
	done.Complete = done.Remaining == 0
	return done, nil
}

// writeApprovesBatch levels one batch's approves tuples in one transaction, in the store's lock
// order (api/inbox_read.go's markInboxReadThreads): the owner rows the batch's asks stand under
// first, each once, in ascending key (lockAskOwners), then the batch's moves read again under
// them, then the tuples (graphstore.QueueWrite, which writes no node: each ask's node is the asks'
// pass's or its writer's) and their head rows. Every writer of an approval request's documents on
// this build holds its owner (the request route, a version move and a renewal, each under the
// owner lock), so the moves read under the owner rows are the rows' as they commit. It answers how
// many requests it levelled and how many tuples it stated.
func writeApprovesBatch(ctx context.Context, database *store.Store, batch []askBatchRow) (int64, int64, error) {
	var levelled, stated int64
	if err := pgx.BeginFunc(ctx, database.Pool, func(tx pgx.Tx) error {
		if err := lockAskOwners(ctx, tx, batch); err != nil {
			return err
		}
		ids := make([]string, len(batch))
		for index, ask := range batch {
			ids[index] = ask.askID
		}
		rows, err := tx.Query(ctx, `
			select k.node_address, coalesce(k.edited_at, k.created_at), move.to_kind, move.node, move.asserted
			from asks k
			cross join lateral (`+approvesMoves+`) move
			where k.id = any($1::uuid[]) and k.kind = 'approval' and k.node_kind is not null
			order by k.id, move.asserted, move.to_kind, move.node
		`, ids)
		if err != nil {
			return fmt.Errorf("read a batch's approves tuples again under their owners: %w", err)
		}
		now := time.Now().UTC()
		var tuples []graphstore.Tuple
		asks := map[string]struct{}{}
		for rows.Next() {
			var node, kind, version string
			var named time.Time
			var asserted bool
			if err := rows.Scan(&node, &named, &kind, &version, &asserted); err != nil {
				rows.Close()
				return fmt.Errorf("read a batch's approves tuples again under their owners: %w", err)
			}
			at := now
			if asserted {
				at = named
			}
			tuples = append(tuples, graphstore.Tuple{
				FromKind: graphstore.KindAsk, FromID: node, Kind: TupleApproves, ToKind: kind, ToID: version,
				At: at, Who: backfillActor, SourceEvent: graphstore.NoEvent, Asserted: asserted,
			})
			asks[node] = struct{}{}
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return fmt.Errorf("read a batch's approves tuples again under their owners: %w", err)
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
		levelled, stated = int64(len(asks)), int64(len(tuples))
		return nil
	}); err != nil {
		return 0, 0, fmt.Errorf("write a batch of approves tuples: %w", err)
	}
	return levelled, stated, nil
}
