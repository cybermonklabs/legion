package graphcensus

import (
	"context"
	"strings"

	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// decisionProjections are the census's kinds of the decisions-tree spec (projections): the ties of
// decisions, readings and asks that block_refs, document_blocks, asks and ask_followers project,
// each held triple by triple (projection.tuples) but a person's answer; and after them the same
// ties as graph_edges answers them, each one that view answers (edgeProjection).
var decisionProjections = []projection{
	tieProjection(refs.TupleShaped,
		"a decision's shaped, and an ask's, which hangs on the ask's node: the row side is every shaped row of"+
			" block_refs as the triple of the node it names, shaped and its block, against every shaped tuple a"+
			" block or ask node not gone states now", refs.BlockNodeKind, graphstore.KindAsk),
	tieProjection(refs.TupleFromAsk,
		"a decision's from_ask: the row side is every from_ask row of block_refs as the triple of the"+
			" decision's node, from_ask and its ask, against every from_ask a block node not gone states now", refs.BlockNodeKind),
	tieProjection(graphstore.TupleDecidedBy,
		"a decision's decided_by: the row side is every decided_by row of block_refs as the triple of the"+
			" decision's node, decided_by and its person or session, against every decided_by a block node not"+
			" gone states now", refs.BlockNodeKind),
	{
		name:     graphstore.TupleFollowedBy,
		nodeKind: graphstore.KindAsk,
		table:    "ask_followers",
		note: "an ask's followers: the row side is every marked ask_followers row as the triple of the ask's node" +
			" it names, followed_by and its session, against every followed_by an ask node not gone states now",
		rows: `
		select 'ask_follower ' || f.ask_id::text || ' ' || f.session_id, f.node_kind, f.node_address,
		       '` + graphstore.TupleFollowedBy + `', '` + graphstore.KindSession + `', f.session_id
		from ask_followers f
		where f.node_kind is not null and octet_length(f.session_id) <= ` + addressBound,
		tuples:    heldTuples(`t.kind = '`+graphstore.TupleFollowedBy+`'`, graphstore.KindAsk),
		pastBound: followerPastBound,
		unmarked: `
		select count(*) from ask_followers f where f.node_kind is null and octet_length(f.session_id) <= ` + addressBound,
		emptyAgrees: true,
	}, {
		name:     "reading_parent",
		nodeKind: refs.BlockNodeKind,
		table:    "document_blocks.parent_block_id",
		note: "the parent of each reading: the row side is the document_blocks row of every reading's node as the" +
			" triple of that node, part_of and the block its parent_block_id names, or its document's top where it" +
			" names none, against the part_of every reading's node not gone states now, a part_of to an ask's node" +
			" read as the ask's block in the node's own document",
		// A reading no block holds hangs under its document's top (refs.DocumentTopSQL, written
		// kind:id), which the row side splits into the triple's far end.
		rows: `
		select 'reading ' || d.node_id, d.node_kind, d.node_address, '` + graphstore.TuplePartOf + `',
		       case when d.parent_block_id is null then split_part(document_top.at, ':', 1) else '` + refs.BlockNodeKind + `' end,
		       case when d.parent_block_id is null then substr(document_top.at, strpos(document_top.at, ':') + 1)
		            else a.ref_key || '#' || d.parent_block_id end
		from document_blocks d
		join artifacts a on a.id = d.artifact_id
		cross join lateral (select ` + refs.DocumentTopSQL("a") + ` as at) document_top
		join graph_nodes reading on reading.kind = '` + refs.BlockNodeKind + `' and reading.id = d.node_id
		  and reading.supertag = '` + refs.ReadingSupertag + `'`,
		tuples: readingParents,
		owed:   typedNodeOwed,
		// kept for one release: deferred-typed-rows
		rowOwed:     typedRowOwed,
		emptyAgrees: true,
	},
	askProjection("ask_parent", "asks.block_artifact_id, asks.artifact_id",
		"an ask's placement: the row side is every marked asks row as the triples of its node, part_of and the"+
			" document holding its block, and owned_by and the document owning it, against the part_of and owned_by"+
			" every ask node not gone states now",
		`(document.id is not null or owner.id is not null)`,
		[]string{`'` + graphstore.TuplePartOf + `', 'artifact', document.ref_key`, `'` + graphstore.TupleOwnedBy + `', 'artifact', owner.ref_key`},
		`t.kind in ('`+graphstore.TuplePartOf+`', '`+graphstore.TupleOwnedBy+`')`),
	askProjection("ask_anchor", "asks.block_artifact_id, asks.anchor",
		"an ask's anchor: the row side is every marked asks row whose block's document or anchor names a"+
			" document, as the triple of its node, anchored_to and that document, against the anchored_to every"+
			" ask node not gone states now",
		`anchor.id is not null`,
		[]string{`'` + graphstore.TupleAnchoredTo + `', 'artifact', anchor.ref_key`},
		`t.kind = '`+graphstore.TupleAnchoredTo+`'`),
	{
		name:     "ask_answer",
		nodeKind: graphstore.KindAsk,
		table:    "asks.answer",
		note: "a person's answer: the row side is every marked asks row holding an answer, and the digest is its" +
			" words, its selection, its time and its person, character for character, against the answer, selected" +
			" and answered_at values and the answered_by and decided_by its node holds now",
		rows: `
		select 'ask ' || k.id::text, k.node_kind, k.node_address, md5(k.id::text || '|' || ` + statedLinks(
			`'`+graphstore.TupleAnswer+`>value:' || (k.answer->>'text')`,
			`'`+graphstore.TupleSelected+`>value:' || coalesce(case when jsonb_typeof(k.answer->'selected') = 'array'
			      then k.answer->'selected' end, '[]'::jsonb)::text`,
			`'`+graphstore.TupleAnsweredAt+`>value:' || (k.answer->>'at')`,
			`case when `+answerer+` <> '' and octet_length(`+answerer+`) <= `+addressBound+`
			      then '`+graphstore.TupleAnsweredBy+`>`+graphstore.KindPerson+`:' || `+answerer+` end`,
			`case when `+answerer+` <> '' and octet_length(`+answerer+`) <= `+addressBound+`
			      then '`+graphstore.TupleDecidedBy+`>`+graphstore.KindPerson+`:' || `+answerer+` end`) + `)
		from asks k
		where k.node_kind is not null and k.answer is not null`,
		// The values are read as a current value is, the newest tuple of each kind while it asserts,
		// and the two relations as a current relation is; selected is read as the JSON it holds,
		// as the row's is, so the labels are compared character for character whatever spacing
		// either encoding put between them. A value that is no JSON is compared as it is written,
		// and so differs.
		nodes: `
		select held.id, md5(held.id || '|' || held.facts)
		from (
			select n.id, (select coalesce(string_agg(answered.fact, ',' order by answered.fact), '') from (
				select v.kind || '>value:' || case when v.kind = '` + graphstore.TupleSelected + `' and pg_input_is_valid(v.to_id, 'jsonb')
				                                   then v.to_id::jsonb::text else v.to_id end
				from (
					select distinct on (t.kind) t.kind, t.to_id, t.asserted
					from graph_tuples t
					where t.from_kind = '` + graphstore.KindAsk + `' and t.from_id = n.id and t.to_kind = '` + graphstore.KindValue + `'
					  and t.kind in ('` + graphstore.TupleAnswer + `', '` + graphstore.TupleSelected + `', '` + graphstore.TupleAnsweredAt + `')
					order by t.kind, t.seq desc
				) v where v.asserted
				union all
				select r.kind || '>' || r.to_kind || ':' || r.to_id from (
					` + graphstore.CurrentTupleRead(`t.kind, t.to_kind, t.to_id`, `t.kind, t.to_kind, t.to_id, t.asserted`,
			`t.from_kind = '`+graphstore.KindAsk+`' and t.from_id = n.id and t.to_kind <> '`+graphstore.KindValue+`'
					  and t.kind in ('`+graphstore.TupleAnsweredBy+`', '`+graphstore.TupleDecidedBy+`')`) + `
				) r where r.asserted
			) answered (fact)) as facts
			from graph_nodes n where n.kind = '` + graphstore.KindAsk + `'
		) held
		where held.facts <> ''`,
		unmarked:    askUnmarked(`k.answer is not null`),
		pastBound:   askPastBound,
		emptyAgrees: true,
	},
	edgeProjection(refs.TupleShaped+edgeSuffix, refs.TupleShaped,
		"graph_edges' shaped: the row side is every shaped edge graph_edges answers out of a block or ask node not"+
			" gone, read node by node, as the triple of that node, shaped and its far end, against every shaped tuple"+
			" a block or ask node not gone states now, an edge the old arm answers from a row past the bound aside",
		tieEdgePastBound, tiePastBound(refs.TupleShaped), refs.BlockNodeKind, graphstore.KindAsk),
	edgeProjection(refs.TupleFromAsk+edgeSuffix, refs.TupleFromAsk,
		"graph_edges' from_ask: the row side is every from_ask edge graph_edges answers out of a block node not"+
			" gone, read node by node, as the triple of that node, from_ask and its ask, against every from_ask a"+
			" block node not gone states now, an edge the old arm answers from a row past the bound aside",
		tieEdgePastBound, tiePastBound(refs.TupleFromAsk), refs.BlockNodeKind),
	edgeProjection(graphstore.TupleDecidedBy+edgeSuffix, graphstore.TupleDecidedBy,
		"graph_edges' decided_by: the row side is every decided_by edge graph_edges answers out of a block node"+
			" not gone, read node by node, as the triple of that node, decided_by and its person or session, against"+
			" every decided_by a block node not gone states now, an edge the old arm answers from a row past the bound aside",
		tieEdgePastBound, tiePastBound(graphstore.TupleDecidedBy), refs.BlockNodeKind),
	edgeProjection(graphstore.TupleFollowedBy+edgeSuffix, graphstore.TupleFollowedBy,
		"graph_edges' followed_by: the row side is every followed_by edge graph_edges answers out of an ask node"+
			" not gone, read node by node, as the triple of that node, followed_by and its session, against every"+
			" followed_by an ask node not gone states now, an edge the old arm answers from an unmarked row aside",
		followerRowUnmarked, followerPastBound, graphstore.KindAsk),
	edgeProjection("ask_parent"+edgeSuffix, graphstore.TupleOwnedBy,
		"graph_edges' half of an ask's placement, its owned_by: the row side is every owned_by edge graph_edges"+
			" answers out of an ask node not gone, read node by node, as the triple of that node, owned_by and the"+
			" document owning it, against every owned_by an ask node not gone states now; no arm of graph_edges answers"+
			" an ask's part_of, which ask_parent holds against asks alone; an edge the old arm answers from an unmarked"+
			" row aside", askRowUnmarked, askPastBound, graphstore.KindAsk),
	edgeProjection("ask_anchor"+edgeSuffix, graphstore.TupleAnchoredTo,
		"graph_edges' anchor of an ask: the row side is every anchored_to edge graph_edges answers out of an ask"+
			" node not gone, read node by node, as the triple of that node, anchored_to and its document, against"+
			" every anchored_to an ask node not gone states now, an edge the old arm answers from an unmarked row aside",
		askRowUnmarked, askPastBound, graphstore.KindAsk),
}

// decisionBackfills are the backfill's passes over the asks and their followers
// (the decisions-tree spec, backfillPasses), after the object pass wrote the node of every ask
// (backfillObjectNodes): the rest of the boot step's first phase (BackfillAsks), so a database the
// command fills reads as one the boot fills.
var decisionBackfills = []backfillPass{kindSoFar(backfillAsks), kindSoFar(backfillAskFollowers)}

// kindSoFar is the backfillPass of a pass over one kind that reports what it did beside its
// error when it fails: every batch of an asks or follower pass committed before a failure stays,
// and the report says how far it got, as BackfillAsks does.
func kindSoFar(write func(context.Context, *store.Store) (KindBackfill, error)) backfillPass {
	return func(ctx context.Context, database *store.Store) ([]KindBackfill, error) {
		done, err := write(ctx, database)
		return []KindBackfill{done}, err
	}
}

// heldTuples is the store side of a kind held triple by triple (projection.tuples): every
// relation a node of one of holders the store holds present states now under the tuple kinds the
// predicate kinds selects (written over t.kind), read as a statement reads a current relation, the
// newest tuple per (kind, far end) kept while it asserts (graphstore.CurrentTupleRead), each as
// (from_kind, from_id, kind, to_kind, to_id). A node is present at any state but gone (R12 of
// the node-visibility spec, as presentNodes reads one), so a proposed or settled decision's ties
// are held as a live one's are. Each node's relations are read through graph_tuples_current, one
// probe per node, as currentLinks reads them.
func heldTuples(kinds string, holders ...string) string {
	return `
		select n.kind, n.id, held.kind, held.to_kind, held.to_id
		from graph_nodes n
		cross join lateral (
			` + graphstore.CurrentTupleRead(`t.kind, t.to_kind, t.to_id`, `t.kind, t.to_kind, t.to_id, t.asserted`,
		`t.from_kind = n.kind and t.from_id = n.id
		  and t.to_kind <> '`+graphstore.KindValue+`' and `+kinds) + `
		) held
		where ` + kindIn("n.kind", holders) + ` and n.state <> '` + graphstore.StateGone + `'
		  and held.asserted`
}

// askBlockID is the block id a part_of from the block node node to the ask node at address (each
// an SQL expression) is read as, null where no marked asks row names that ask's node or its row
// names no block. In the document the ask's row names (asks.block_artifact_id), it is the ask's
// own block, the id that row names (asks.block_id). In any other document of its owner it is the
// block node's own parent (document_blocks.parent_block_id), a copy of the ask: a reading in a
// copy hangs on the source's node (refs.TypedSources.Shown), and the copy keeps the block id it
// was copied under while the source can take a fresh one (docs' restore, R6's collision rule).
func askBlockID(node, address string) string {
	return `(select case when own.ref_key = split_part(` + node + `, '#', 1) then k.block_id
		             else (select d.parent_block_id from document_blocks d where d.node_id = ` + node + `) end
		from asks k join artifacts own on own.id = k.block_artifact_id
		where k.node_kind = '` + graphstore.KindAsk + `' and k.node_address = ` + address + ` and k.block_id is not null)`
}

// readingParents is reading_parent's store side (heldTuples' shape): the part_of every reading's
// node the store holds present states now, a part_of to an ask's node read as blockPlacedLinks
// reads it, as the block of the reading's own document that holds the ask or the copy of it the
// reading stands in.
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
		  and n.state <> '` + graphstore.StateGone + `' and held.asserted`

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
// bound a process of the release before runs no block index in the write, and its typed-node pass
// states each decision and reading the version no longer holds gone in that write's own
// transaction, while the document_blocks row of its block and the block_refs rows it holds stay
// until the block-index reconciliation deletes them; this build's pass deletes them in the same
// write (refs.PlanDecisionNodes) - the rows of its ties, its shaped, from_ask and decided_by, in
// the reconcile of the ties, which reaches every block that held one (refs' pairScope.holding),
// and its document_blocks row with every other row it holds as the pass states it gone (refs'
// planGone) - so such a row is left only during the roll. Such a row is owed its deletion, and no
// difference until then; once the document owes nothing, the same row names a gone node and is
// one. A gone node of any other supertag under its row is a difference still, since no writer
// leaves one so. A ref_key holds no '#', so a block node's document is the part of its id before
// the first one. The release after this one deletes it, with the rowOwed of each projection that
// reads it, once no process of the release before is live.
// kept for one release: deferred-typed-rows
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

// tieProjection is the kind of one tie that block_refs projects - a decision's shaped, from_ask or
// decided_by, and an ask's shaped - held triple by triple (projection.tuples): each row is the
// triple of the node that states its tie (node_kind, node_address: the decision's block node, or
// for an ask block's shaped the ask's node, refs.TypedFacts.pairs), kind and its far end, against
// every tuple of kind a present node of one of holders states now. So a tuple that reaches the
// wrong node is a row no tuple states and a tuple no row projects, and not a count that still
// matches, and a row of the same node whose tuple stands is no difference. A row that names no
// node is a difference, as a block_ref row's is: every writer of these kinds names it, and a row
// past the store's bound is counted apart. A row of a decision a deferred version removed is owed
// its deletion (typedRowOwed).
func tieProjection(kind, note string, holders ...string) projection {
	return projection{
		name: kind, nodeKind: strings.Join(holders, ", "), table: "block_refs", note: note, emptyAgrees: true,
		rows: `
		select 'block_ref ' || r.from_node, r.node_kind, r.node_address, r.kind, r.to_kind, r.to_id
		from block_refs r
		where r.kind = '` + kind + `' and ` + tieInBound,
		tuples:    heldTuples(`t.kind = '`+kind+`'`, holders...),
		pastBound: tiePastBound(kind),
		// kept for one release: deferred-typed-rows
		rowOwed: typedRowOwed,
	}
}

// edgeSuffix ends the name of a kind edgeProjection holds, after the name of the kind of the same
// tie that a table projects: shaped_edge beside shaped.
const edgeSuffix = "_edge"

// edgeProjection is one tie of decisions, readings and asks as graph_edges answers it, held
// triple by triple (projection.tuples) as tieProjection holds block_refs: each row is an edge the
// view answers out of a node of one of holders (graphEdgesOut), as the triple of that node, kind
// and its far end, against every tuple of kind a node of one of holders that is not gone states
// now. So an edge the view answers that no tuple states is a row no tuple states, a tuple the view
// answers no edge for is a tuple no row projects, and an edge that reaches the wrong node reads as
// both.
//
// The view's last restatement (store/migrations/20261126050009_issue_tree_graph_edges_block_node_listed.up.sql,
// which keeps the arms of 20261126050008_issue_tree_graph_edges_after_store_class_last.up.sql,
// 20261126050006_issue_tree_graph_edges_after_tuple_arm_filter.up.sql's view stated again:
// the arms 20261125163553_graph_edges_asks.up.sql and
// 20261126050002_graph_edges_asks_and_store_class.up.sql give these ties, with 20261120122700's
// filter of the tuple arm) answers each of these ties from one arm, which the projection row
// decides. For a row that names its node, the tuple arm answers the newest tuple of the triple
// while it asserts, out of a node not gone: shaped, from_ask and decided_by out of a block node,
// and shaped out of an ask's node, while a block_refs row of that node, kind and far end stands (a
// block's pointer only while its target is of a type the attribute lists, and shaped, from_ask and
// decided_by list none); anchored_to and owned_by out of an ask's node while an asks row names
// that node; and followed_by while the ask_followers row of that ask and that session names it.
// For a row that names no node, which a process of the release before wrote, an old arm answers
// the row alone: the block_refs arm its link, the two asks arms its anchored_to and owned_by, and
// the ask_followers arm its followed_by. No arm answers a reading's part_of or an ask's part_of
// (the tuple arm answers a block's in_document, as its part_of to its document), so the parent of
// a reading has no kind here, and an ask's placement is held by its owned_by alone
// (ask_parent_edge).
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
// node. Fenced, each arm keys its own index with the node's kind and id. The probe also names the
// holders' kinds as constants, the kinds n.kind takes, so an arm whose from_kind is a constant of
// another kind - the arm out of a finding of a lens, whose from_kind is lens_block - is cut from
// the plan rather than kept in it, scan and all, behind a test of n.kind for each node. An edge an
// old arm answers out of a row whose ask the store holds no node of yet is no row of this read
// either.
func graphEdgesOut(kind, unmarked string, holders ...string) string {
	read := `
		select 'graph_edges ' || n.kind || ':' || n.id, n.kind, n.id, edge.kind, edge.to_kind, edge.to_id
		from graph_nodes n
		cross join lateral (
			select e.kind, e.to_kind, e.to_id from graph_edges e
			where e.from_kind = n.kind and ` + kindIn("e.from_kind", holders) + `
			  and e.from_id = n.id and e.kind = '` + kind + `'
			offset 0
		) edge
		where ` + kindIn("n.kind", holders) + ` and n.state <> '` + graphstore.StateGone + `'`
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

// followerPastBound counts the ask_followers rows whose session is past the store's bound, which
// no node can key: the follower kind and graph_edges' followed_by count them apart.
var followerPastBound = `
		select count(*) from ask_followers f where octet_length(f.session_id) > ` + addressBound

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
