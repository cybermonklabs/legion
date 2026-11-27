package graphcensus

import (
	"strings"

	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
)

// heldTuples is the store side of a kind held triple by triple (projection.tuples): every
// relation a live node of one of holders states now under the tuple kinds the predicate kinds
// selects (written over t.kind), read as a statement reads a current relation, the newest tuple
// per (kind, far end) kept while it asserts (graphstore.CurrentTupleRead), each as (from_kind,
// from_id, kind, to_kind, to_id). Each node's relations are read through graph_tuples_current,
// one probe per node, as currentLinks reads them.
func heldTuples(kinds string, holders ...string) string {
	return `
		select n.kind, n.id, held.kind, held.to_kind, held.to_id
		from graph_nodes n
		cross join lateral (
			` + graphstore.CurrentTupleRead(`t.kind, t.to_kind, t.to_id`, `t.kind, t.to_kind, t.to_id, t.asserted`,
		`t.from_kind = n.kind and t.from_id = n.id
		  and t.to_kind <> '`+graphstore.KindValue+`' and `+kinds) + `
		) held
		where n.kind in ('` + strings.Join(holders, `', '`) + `') and n.state = '` + graphstore.StateLive + `'
		  and held.asserted`
}

// askBlockID is the block id a part_of from the block node node to the ask node at address (each
// an SQL expression) is read as, null where no marked asks row names that ask's node or its row
// names no block. In the document the ask's row names (asks.block_artifact_id), it is the ask's
// own block, the id that row names (asks.block_id). In any other document of its owner it is the
// block node's own parent (document_blocks.parent_block_id) where that parent is a copy of the
// ask: a reading in a copy hangs on the source's node (refs.TypedSources.Shown), and the copy
// keeps the block id it was copied under while the source can take a fresh one (docs' restore,
// R6's collision rule). The parent is a copy of the ask where its node states a current
// copied_from to that ask (docs' claimCopies). The parent's id alone proves nothing, since it is
// the row this census checks the part_of against: a reading whose part_of names another marked
// ask than the one its parent copies reads null here, so the ask's address stays the link and
// reads as a difference. Two states take the parent without that tuple: a document that owes a
// settlement (doc_settlements_pending), since a version's typed-node pass hangs a copy's reading on
// its source before the settlement after it claims the copy, in the write that deletes that row;
// and a parent whose address is past the store's bound, which is no node to state one.
func askBlockID(node, address string) string {
	parent := `split_part(` + node + `, '#', 1) || '#' || d.parent_block_id`
	return `(select case when own.ref_key = split_part(` + node + `, '#', 1) then k.block_id
		             else (select d.parent_block_id from document_blocks d
		                   where d.node_id = ` + node + ` and (
		                       exists (select 1 from (` + graphstore.CurrentTupleRead(`t.to_kind, t.to_id`, `t.asserted`,
		`t.from_kind = '`+refs.BlockNodeKind+`' and t.from_id = `+parent+`
		                           and t.kind = '`+graphstore.TupleCopiedFrom+`' and t.to_kind = '`+graphstore.KindAsk+`'
		                           and t.to_id = `+address) + `
		                       ) copied where copied.asserted)
		                       or exists (select 1 from doc_settlements_pending owed where owed.artifact_id = d.artifact_id)
		                       or octet_length(` + parent + `) > ` + addressBound + `)) end
		from asks k join artifacts own on own.id = k.block_artifact_id
		where k.node_kind = '` + graphstore.KindAsk + `' and k.node_address = ` + address + ` and k.block_id is not null)`
}

// placedLinks is currentLinks of a block node n under kinds, but a part_of whose far end is an
// ask's node is written as the block of n's own document that holds that ask (askBlockID): the
// ask's own block in the document its row names, and in any other document of its owner the block
// n stands in where that block is a copy of the ask. A reading in an answered ask hangs on the
// ask's node, while its row's parent_block_id keeps the id of the ask block holding it, which is
// that tuple's projection (refs' reconcileBlockParents), so both read as one link to that block.
// An ask no marked row names, or whose row names no block, or a part_of to an ask the block n
// stands in is no copy of, keeps the ask's own address, which no row states. A ref_key holds no
// '#', so n's document is the part of its id before the first one.
func placedLinks(kinds string) string {
	return `(select coalesce(string_agg(placed.link, ',' order by placed.link), '')
	 from (
		select case when held.kind = '` + graphstore.TuplePartOf + `' and held.to_kind = '` + graphstore.KindAsk + `'
		            then coalesce(held.kind || '>` + refs.BlockNodeKind + `:' || split_part(n.id, '#', 1) || '#' || ` +
		askBlockID(`n.id`, `held.to_id`) + `, held.kind || '>' || held.to_kind || ':' || held.to_id)
		            else held.kind || '>' || held.to_kind || ':' || held.to_id end as link
		from (
			` + graphstore.CurrentTupleRead(`t.kind, t.to_kind, t.to_id`, `t.kind, t.to_kind, t.to_id, t.asserted`,
		`t.from_kind = '`+refs.BlockNodeKind+`' and t.from_id = n.id
		  and t.to_kind <> '`+graphstore.KindValue+`' and `+kinds) + `
		) held where held.asserted
	 ) placed)`
}

// readingParents is reading_parent's store side (heldTuples' shape): the part_of every live
// reading's node states now, a part_of to an ask's node read as placedLinks reads it, as the
// block of the reading's own document that holds the ask or the copy of it the reading stands in.
var readingParents = `
		select n.kind, n.id, held.kind,
		       case when placed.block_id is null then held.to_kind else '` + refs.BlockNodeKind + `' end,
		       coalesce(split_part(n.id, '#', 1) || '#' || placed.block_id, held.to_id)
		from graph_nodes n
		cross join lateral (
			` + graphstore.CurrentTupleRead(`t.kind, t.to_kind, t.to_id`, `t.kind, t.to_kind, t.to_id, t.asserted`,
	`t.from_kind = '`+refs.BlockNodeKind+`' and t.from_id = n.id
		  and t.to_kind <> '`+graphstore.KindValue+`' and t.kind = '`+graphstore.TuplePartOf+`'`) + `
		) held
		cross join lateral (
			select case when held.to_kind = '` + graphstore.KindAsk + `' then ` + askBlockID(`n.id`, `held.to_id`) + ` end as block_id
		) placed
		where n.kind = '` + refs.BlockNodeKind + `' and n.supertag = '` + refs.ReadingSupertag + `'
		  and n.state = '` + graphstore.StateLive + `' and held.asserted`

// typedNodeOwed is the owed predicate (projection.owed) of a kind whose node may be a decision's
// or a reading's. A document that still owes its block index (document_block_versions.version
// null) holds no row yet for a block its latest version added, while that version's typed-node
// pass already wrote the block's node: a decision or a reading written above the deferral bound
// (refs.PlanDecisionNodes), the one writer of a block node that writes no row beside it. The
// block-index reconciliation writes the row, so such a node is owed its row and no difference
// until then; a node of any other supertag in such a document is a difference still, since no
// writer leaves one without its row. A ref_key holds no '#', so a block node's document is the
// part of its id before the first one.
var typedNodeOwed = `exists (
		select 1 from graph_nodes typed
		join artifacts a on a.ref_key = split_part(typed.id, '#', 1)
		join document_block_versions owed on owed.artifact_id = a.id
		where typed.kind = '` + refs.BlockNodeKind + `' and typed.id = node.id
		  and typed.supertag in ('` + refs.DecisionSupertag + `', '` + refs.ReadingSupertag + `')
		  and owed.version is null)`

// typedRowOwed is the row-side owed predicate (projection.rowOwed) of a kind whose row may name a
// decision's or a reading's node: the node the row names is a decision or a reading the store
// holds gone, in a document that still owes its block index (owesBlockIndex). Above the deferral
// bound no block index runs in the write, and the typed-node pass states each decision and
// reading the version no longer holds gone in that write's own transaction
// (refs.PlanDecisionNodes), and deletes the block_refs rows of its ties - its shaped, from_ask and
// decided_by - there too, since its reconcile reaches every block that held one (refs'
// pairScope.holding); the document_blocks row of its block and the block_refs rows of its
// mentions stay until the block-index reconciliation deletes them. Such a row is owed its deletion,
// and no difference until then; once the document owes nothing, the same row names a gone node
// and is one. A gone node of any other supertag under its row is a difference still, since no
// writer leaves one so. A ref_key holds no '#', so a block node's document is the part of its id
// before the first one.
var typedRowOwed = `node.kind = '` + refs.BlockNodeKind + `' and node.state = '` + graphstore.StateGone + `'
		  and node.supertag in ('` + refs.DecisionSupertag + `', '` + refs.ReadingSupertag + `')
		  and exists (select 1 from artifacts a where a.ref_key = split_part(node.id, '#', 1) and ` + owesBlockIndex("a.id") + `)`

// tieInBound is true of a block_refs row r both of whose ends are inside the store's bound, the one
// row of a tie its writer marks and states a tuple of (refs' queueBlockRefs). Its near end is the
// node the row names: the ask's, by its id, for an ask block's shaped (refs.TypedFacts.pairs),
// whose writer judges the bound on that id and not on the block's own address (refs'
// planBlockRefs), and the block's own address for every other row, a row that names no node among
// them.
var tieInBound = `octet_length(case when r.node_kind = '` + graphstore.KindAsk + `' then r.node_address else r.from_node end) <= ` +
	addressBound + ` and octet_length(r.to_id) <= ` + addressBound

// tiePastBound counts the block_refs rows of the tie kind that are not inside the store's bound
// (tieInBound): the writer leaves each unmarked and states no tuple of it on purpose, and
// graph_edges' block_refs arm answers it as the row alone.
func tiePastBound(kind string) string {
	return `
		select count(*) from block_refs r
		where r.kind = '` + kind + `' and not (` + tieInBound + `)`
}

// tieEdgePastBound is edgeProjection's unmarked predicate of a decision's tie and an ask's shaped:
// the edge's far end is past the store's bound, so the block_refs row it answers is one the writer
// left unmarked and stated no tuple of (tiePastBound), which graph_edges' block_refs arm answers.
// The near end is a node of the store, inside the bound.
var tieEdgePastBound = `(octet_length(edge.to_id) > ` + addressBound + `)`

// tieProjection is the kind of one tie that block_refs projects - a
// decision's shaped, from_ask or decided_by, and an ask's shaped - held triple by triple
// (projection.tuples): each row is the triple of the node that states its tie (node_kind,
// node_address: the decision's block node, or for an ask block's shaped the ask's node,
// refs.TypedFacts.pairs), kind and its far end, against every tuple of kind a live node of one of
// holders states now. So a tuple that reaches the wrong node is a row no tuple states and a tuple
// no row projects, and not a count that still matches, and a row of the same node whose tuple
// stands is no difference. A row that names no node is a difference, as a block_ref row's is:
// every writer of these kinds names it, and a row past the store's bound is counted apart. A row
// of a decision a deferred version removed is owed its deletion (typedRowOwed).
func tieProjection(kind, note string, holders ...string) projection {
	return projection{
		name: kind, nodeKind: strings.Join(holders, ", "), table: "block_refs", note: note, emptyAgrees: true,
		rows: `
		select 'block_ref ' || r.from_node, r.node_kind, r.node_address, r.kind, r.to_kind, r.to_id
		from block_refs r
		where r.kind = '` + kind + `' and ` + tieInBound,
		tuples:    heldTuples(`t.kind = '`+kind+`'`, holders...),
		pastBound: tiePastBound(kind),
		rowOwed:   typedRowOwed,
	}
}

// edgeSuffix ends the name of a kind edgeProjection holds, after the name of the kind of the same
// tie that a table projects: shaped_edge beside shaped.
const edgeSuffix = "_edge"

// edgeProjection is one tie of decisions, readings and asks as graph_edges answers it, held
// triple by triple (projection.tuples) as tieProjection holds block_refs: each row is an edge the
// view answers out of a node of one of holders (graphEdgesOut), as the triple of that node, kind
// and its far end, against every tuple of kind a live node of one of holders states now. So an
// edge the view answers that no tuple states is a row no tuple states, a tuple the view answers no
// edge for is a tuple no row projects, and an edge that reaches the wrong node reads as both.
//
// The view's newest definition (store/migrations/20261126045005_graph_edges_identity_and_tuple_arm_filter.up.sql,
// which states these ties' arms as 20261125163554 does - 20261125163553's with 20261120122700's
// filter of the tuple arm - and the Inbox's arms beside them) answers each of these ties from one
// arm, which the projection row decides. For a row that names its
// node, the tuple arm answers the newest tuple of the triple while it asserts, out of a node not
// gone: shaped, from_ask and decided_by out of a block node, and shaped out of an ask's node, while
// a block_refs row of that node, kind and far end stands (a block's pointer only while its target
// is of a type the attribute lists, and shaped, from_ask and decided_by list none); anchored_to
// and owned_by out of an ask's node while an asks row names that node; and followed_by while the
// ask_followers row of that ask and that session names it. For a row that names no node, which a
// process of the release before wrote, an old arm answers the row alone: the block_refs arm its
// link, the two asks arms its anchored_to and owned_by, and the ask_followers arm its
// followed_by. No arm answers a reading's part_of or an ask's part_of (the tuple arm answers a
// block's in_document, as its part_of to its document), so the parent of a reading has no kind
// here, and an ask's placement is held by its owned_by alone (ask_parent_edge).
//
// unmarked is a predicate over the node n and its edge that holds where an old arm answers the
// edge from a row that names no node - an asks or ask_followers row a process of the release
// before wrote, or one the ask's writer leaves unmarked on purpose because a document it names is
// past the store's bound, while it writes the ask's node all the same, or a block_refs row of a
// decision's tie or an ask's shaped whose far end is past that bound (tieEdgePastBound) - and is
// empty for a kind no old arm answers that way. Such an edge is no row here, as such a row is no
// row of the kind that projects it (unmarked_rows, addresses_past_bound); pastBound is the class
// that kind counts beside it.
func edgeProjection(name, kind, note, unmarked, pastBound string, holders ...string) projection {
	return projection{
		name: name, nodeKind: strings.Join(holders, ", "), table: "graph_edges", note: note, emptyAgrees: true,
		rows:      graphEdgesOut(kind, unmarked, holders...),
		tuples:    heldTuples(`t.kind = '`+kind+`'`, holders...),
		pastBound: pastBound,
	}
}

// askRowUnmarked and followerRowUnmarked are edgeProjection's unmarked predicates of the ask's
// own relations and of its followers: the asks row of the node, or the ask_followers row of the
// node and the edge's session, names no node, so the view answers the edge from its old arm. Each
// probes the index graph_edges' old arms are probed through (asks_id_text, ask_followers_ask_id_text).
const (
	askRowUnmarked      = `exists (select 1 from asks k where k.id::text = n.id and k.node_kind is null)`
	followerRowUnmarked = `exists (select 1 from ask_followers f
		where f.ask_id::text = n.id and f.session_id = edge.to_id and f.node_kind is null)`
)

// graphEdgesOut is the row side of an edgeProjection kind: every edge of kind graph_edges answers
// out of a node of one of holders that is not gone - the nodes its tuple arm answers out of -
// read node by node, the view probed with the node's kind and id, as refs.Edges probes it with
// both bound, but an edge unmarked (a predicate over n and edge, empty for none) says an old arm
// answers from a row that names no node. The probe ends in `offset 0`, the fence the view's own
// probes take: without it the planner pulls the probe up into the join, and an arm that is a
// subquery - the tuple arm's newest tuple of each triple - takes the node's kind and id as a
// filter over every tuple of that kind rather than as the keys of its index, read again for each
// node. Fenced, each arm keys its own index with the node's kind and id. An edge an old arm
// answers out of a row whose ask the store holds no node of yet is no row of this read either.
func graphEdgesOut(kind, unmarked string, holders ...string) string {
	read := `
		select 'graph_edges ' || n.kind || ':' || n.id, n.kind, n.id, edge.kind, edge.to_kind, edge.to_id
		from graph_nodes n
		cross join lateral (
			select e.kind, e.to_kind, e.to_id from graph_edges e
			where e.from_kind = n.kind and e.from_id = n.id and e.kind = '` + kind + `'
			offset 0
		) edge
		where n.kind in ('` + strings.Join(holders, `', '`) + `') and n.state <> '` + graphstore.StateGone + `'`
	if unmarked != "" {
		read += `
		  and not ` + unmarked
	}
	return read
}

// askDocuments are the three documents an asks row k names, joined as the ask's writer reads
// them (graphstore.ReadAskPlaces) and the backfill's asks pass joins them (askRowsOwed): anchor,
// the document holding its block or else the one its anchor quotes; owner, the document an ask
// on no issue belongs to; document, the one holding its block. It follows `from asks k`.
const askDocuments = `
		left join artifacts anchor on anchor.id = coalesce(k.block_artifact_id, (k.anchor->>'artifact_id')::uuid)
		left join artifacts owner on owner.id = k.artifact_id
		left join artifacts document on document.id = k.block_artifact_id`

// askFits is true of an asks row none of whose documents' addresses is past the store's bound,
// the one kind of row the ask's writer marks (graphstore.AskPlaces.Fit) and the backfill owes
// (askRowsOwed). It reads askDocuments.
var askFits = `octet_length(coalesce(anchor.ref_key, '')) <= ` + addressBound + `
		  and octet_length(coalesce(owner.ref_key, '')) <= ` + addressBound + `
		  and octet_length(coalesce(document.ref_key, '')) <= ` + addressBound

// askPastBound counts the asks rows some document of which is past the store's bound: the
// writer states none of such an ask's relations and leaves its row unmarked on purpose, and the
// census counts them apart.
var askPastBound = `
		select count(*) from asks k` + askDocuments + `
		where not (` + askFits + `)`

// askUnmarked counts every other asks row that names no node and that member, a predicate over
// the row and askDocuments, makes one of a kind's own population: a row a process of the release
// before wrote and the decisions step's backfill owes, which graph_edges' old arms answer, a class
// of its own and no difference (projection.unmarked). So an ask kind whose every row is unmarked
// holds rows still, and is no kind nothing of which is written yet (KindReport.Empty).
func askUnmarked(member string) string {
	return `
		select count(*) from asks k` + askDocuments + `
		where k.node_kind is null and ` + askFits + ` and ` + member
}

// askProjection is a kind of the relations an ask's node holds that its asks row projects, held
// triple by triple (projection.tuples): links are the relations a marked row states, each written
// `kind, to_kind, to_id` as SQL over the row and askDocuments, a null to_id where the row states
// none, member the predicate that makes a row one of the kind, and kinds the tuple kinds its node
// states them under. A marked row is held by its node, the ask's own, and an unmarked one is
// counted apart.
func askProjection(name, table, note, member string, links []string, kinds string) projection {
	return projection{
		name: name, nodeKind: graphstore.KindAsk, table: table, note: note, emptyAgrees: true,
		rows: `
		select 'ask ' || k.id::text, k.node_kind, k.node_address, stated.kind, stated.to_kind, stated.to_id
		from asks k` + askDocuments + `
		cross join lateral (values (` + strings.Join(links, `), (`) + `)) stated (kind, to_kind, to_id)
		where k.node_kind is not null and stated.to_id is not null`,
		tuples:    heldTuples(kinds, graphstore.KindAsk),
		pastBound: askPastBound,
		unmarked:  askUnmarked(member),
	}
}
