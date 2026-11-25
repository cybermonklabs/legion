package graphstore

import (
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// node_search (migration 20261125160000_issue_tree_node_search) is the search row of each block
// node that holds text: one row for each block node the store holds present whose newest text
// value (AttributeText) asserts, holding that value, the seq of the tuple that states it and the
// search vector of it. It is what this build's lexical search reads in place of document_search
// (R17 of the issue-tree spec): a word is found in the node whose text holds it, and the
// answer names that node. The row of an ask block, and of each block that stands in one, is held
// like any other; the readers leave those nodes out of what they match (NodeSearchAskNodesSQL).
//
// A text value is a block's alone: the block index states each block's text (refs'
// planBlockValues), the authored-node pass an authored node's and the reasoning write a lens's
// finding's, each a node of kind block. A node's row moves with its text and with its state, and
// both reach the store through a writer that names the node to the head write (QueueHeads) in its
// own transaction, after the statements that wrote them: so QueueHeads writes the rows of the
// block nodes it names, before the head rows (QueueNodeSearch), and every writer of the store
// writes them with no call of its own.

// QueueNodeSearch queues on b the write of the node_search rows of the block nodes ids, read again
// from what the store holds when the statement runs (nodeSearchWriteStatement): the row of each
// one present whose newest text value asserts, written where it holds another tuple's text, and
// the row of each other deleted. Where written is not nil it adds the rows the statement inserts
// or rewrites to it. ids that name nothing queue nothing.
func QueueNodeSearch(b *store.WriteBatch, ids []string, written *int64) {
	if len(ids) == 0 {
		return
	}
	queued := b.Queue("write the search rows of the blocks written", nodeSearchWriteStatement, ids)
	if written == nil {
		return
	}
	queued.Exec(func(tag pgconn.CommandTag) error {
		*written += tag.RowsAffected()
		return nil
	})
}

// queueNodeSearch is QueueHeads' QueueNodeSearch over the block nodes of nodes, a lens's finding
// among them, which a writer may name by its address kind (storedKindOf): a node of any other kind
// holds no text value.
func queueNodeSearch(b *store.WriteBatch, nodes []NodeKey) {
	var ids []string
	for _, node := range nodes {
		if storedKindOf(node.Kind) == blockKind {
			ids = append(ids, node.ID)
		}
	}
	QueueNodeSearch(b, ids, nil)
}

// nodeSearchWriteStatement writes the node_search rows of the block nodes $1 names. Each node's
// state is one probe of graph_nodes' primary key, and its newest text tuple one probe of
// graph_tuples_values, the index of a node's values by kind and seq, each a lateral read ending in
// `limit 1`, so the planner reads them node by node through the key rather than joining the array
// to either table.
//
// A node present whose newest text tuple asserts is written with that tuple's value, seq and
// vector, unless its row holds that seq already, so a write that names a node whose text did not
// move computes no vector; one whose row holds a higher seq, which a writer that read the store
// after this statement's snapshot committed, keeps it (the conflict arm's where). Every other
// node's row is deleted: a node gone, a node whose newest text tuple retracts, a node with no text
// tuple and a node the store holds no row of. Where the statement read a text tuple, the delete
// takes a row holding a seq no higher than that tuple's, so a row a later write committed stands.
// Where it read none, no seq bounds the delete, and none is needed: a row is written in the
// transaction that states its text tuple, after it, and no writer deletes a node or a tuple, so a
// row the statement's snapshot holds over no node or no text tuple stands over facts the store no
// longer holds, as in a store rewound to a build before it, which the backfill's node_search pass
// reaches (graphcensus' backfillNodeSearch). In any other store that arm meets no row. A node with
// neither a row nor a text tuple reads its two probes and nothing more.
//
// The rows are written in ascending node id, the order the delete's probe of the primary key takes
// them in too: a writer takes the node rows of what it writes first, in ascending (kind, id)
// (queueNodes), so two writes of one node have met there before they reach its row here.
var nodeSearchWriteStatement = `
	with named as (
		select distinct named.id from unnest($1::text[]) as named (id)
	),
	said as (
		select named.id, node.state, newest.seq, newest.asserted, newest.to_id as text
		from named
		left join lateral (
			select n.state from graph_nodes n
			where n.kind = '` + blockKind + `' and n.id = named.id
			limit 1
		) node on true
		left join lateral (
			select t.seq, t.asserted, t.to_id from graph_tuples t
			where t.from_kind = '` + blockKind + `' and t.from_id = named.id
			  and t.kind = '` + AttributeText + `' and t.to_kind = '` + KindValue + `'
			order by t.seq desc
			limit 1
		) newest on true
	),
	held as (
		select said.id, said.seq, said.text from said
		where said.state <> '` + StateGone + `' and said.asserted
	),
	dropped as (
		delete from node_search s
		using said
		where s.node_kind = '` + blockKind + `' and s.node_address = said.id
		  and not coalesce(said.state <> '` + StateGone + `' and said.asserted, false)
		  and (said.seq is null or s.text_seq <= said.seq)
	)
	insert into node_search (node_kind, node_address, text, text_seq, search)
	select '` + blockKind + `', held.id, held.text, held.seq, search_vector('', search_document_text(held.text))
	from held
	where not exists (
		select 1 from node_search s
		where s.node_kind = '` + blockKind + `' and s.node_address = held.id and s.text_seq = held.seq
	)
	order by held.id
	on conflict (node_kind, node_address) do update
	   set text = excluded.text, text_seq = excluded.text_seq, search = excluded.search
	 where excluded.text_seq > node_search.text_seq`

// textParentKind is the kind of tuple a block states its place in its document's text under
// (refs.TupleTextParent, which this package may not import): a tuple out of the block to the block
// whose text holds it, or, where no block of the version does, to the node of its document.
const textParentKind = "text_parent"

// askBlockDepth bounds the ask test's walk through text parents: the most blocks a
// document may open inside one another (pmdoc's maxNesting, which this package may not import).
const askBlockDepth = "100"

// askTextSQL is true where the SQL text text opens a block of the type ask: three colons or more
// and `ask{`, the opening pmdoc's ParseTypedDirectiveOpen reads, a block's text beginning at its
// own fence. The cheap prefix test comes first, so a text that opens no fence is never matched
// against the pattern. Over node_search's own column it is, word for word, the predicate of the
// partial index node_search_ask_fences (migration 20261125160000_issue_tree_node_search), which
// is what lets a read of the fences by that test take the index.
func askTextSQL(text string) string {
	return `case when left(` + text + `, 3) = ':::' then ` + text + ` ~ '^:{3,}ask\{' else false end`
}

// currentBlockTextParentSQL is the block block id stands in by the text_parent tuples the store
// holds now, as a select answering at most one row: each relation read as every current relation
// is (CurrentTupleRead), its newest tuple kept while it asserts, the relations to a block alone,
// and of those the newest. A parent that is the node of a document is never climbed to.
func currentBlockTextParentSQL(id string) string {
	return `select standing.to_id from (
			` + CurrentTupleRead(`t.to_id`, `t.to_id, t.asserted, t.seq`,
		`t.from_kind = '`+blockKind+`' and t.from_id = `+id+` and t.kind = '`+textParentKind+`'
			  and t.to_kind = '`+blockKind+`'`) + `
		) standing
		where standing.asserted
		order by standing.seq desc
		limit 1`
}

// currentBlockAskTextSQL answers one row, opens_ask, for the block id: whether its newest text
// tuple asserts and opens an ask block (askTextSQL), and no row for a block with no text tuple.
func currentBlockAskTextSQL(id string) string {
	return `select newest.asserted and ` + askTextSQL("newest.text") + ` as opens_ask
		from (
			select t.asserted, t.to_id as text from graph_tuples t
			where t.from_kind = '` + blockKind + `' and t.from_id = ` + id + `
			  and t.kind = '` + AttributeText + `' and t.to_kind = '` + KindValue + `'
			order by t.seq desc
			limit 1
		) newest`
}

// NodeSearchAskNodesSQL is a CTE of a WITH RECURSIVE, ask_nodes (id, depth), holding every block
// node that is an ask block or stands in one, of the owners the relation owners names (a CTE or
// table with a column ref_key, the owner part of a node's address, refs.NodeSearchOwnerSQL): the
// set InsideAskBlockSQL is true of, read once for a statement rather than once for each node a
// word matched. A reader leaves out of what it matches each node whose address the set holds.
//
// The set is walked down from the ask blocks rather than up from the matches, because the ask
// blocks are few and a common word matches tens of thousands of nodes. Its first rows are the
// node_search rows whose text opens an ask block, read through node_search_ask_fences, the
// partial index of those rows by their owner; each further row is a block whose current block
// parent (currentBlockTextParentSQL) is a row already found, read off the tuples into that row
// (graph_tuples_as_of_in) and kept only where the newest such parent is the row, to askBlockDepth.
//
// It is InsideAskBlockSQL's set, node for node:
//
//   - InsideAskBlockSQL climbs from a node through its current block parent, a function: each node
//     has at most one. So a node x is above an ask block f at distance d (d at most askBlockDepth)
//     on that climb exactly where f is d steps up the same function from x, which is where the
//     walk down from f reaches x at depth d. A cycle of text_parent tuples is cut at the same depth
//     both ways, and `union` drops a node found again at a depth it already holds.
//   - InsideAskBlockSQL tests a node's newest text tuple; the walk starts from node_search's text.
//     node_search holds the row of each block node present whose newest text tuple asserts, with
//     that tuple's value, written in the transaction that writes the tuple (QueueNodeSearch), and
//     the census kind node_search holds the table to the tuples both ways. So the two differ only
//     for a node gone, or one with no node row, whose newest text opens an ask. Neither is an
//     ancestor of a node present: an ask block is stated gone only where a version drops it (refs'
//     IndexBlocks; the deferred typed-node pass states gone only decisions and readings, whose text
//     opens no ask), and the pass that drops it restates the text_parent of every block it keeps to
//     a block the same version holds (planBlockValues), so no live block's current parent is gone.
//     A node with no text tuple opens no fence on either side. A node of a gone ask block is in
//     node_search on neither side, so it is no match at all.
//   - A retype into or out of an ask block is a new text tuple of the block and, in the same
//     transaction, its node_search row, so both sides read the new text with no write of a node
//     under it.
//   - An ask block whose address is past MaxAddressBytes has no node (refs' blockIDFits), so it is
//     on neither side: no tuple names it, and node_search holds no row of it.
//   - owners only narrows which ask blocks the walk starts from. The text_parent tuples link a block
//     to a block of the same document text under the same ref_key (planBlockValues states both ends
//     with document.RefKey), so an ask block above a node shares the node's owner, and a reader that
//     passes the owners its matches can name loses no ask block above any of them.
//
// Every node_search row is a block node's (QueueNodeSearch writes node_kind block alone), so the
// set is keyed by the address alone, as the readers match a row by it.
func NodeSearchAskNodesSQL(owners string) string {
	return `ask_nodes(id, depth) as (
		select fence.node_address, 0
		from ` + owners + ` fence_owner
		join node_search fence on split_part(fence.node_address, '#', 1) = fence_owner.ref_key
		  and ` + askTextSQL("fence.text") + `
		union
		select child.from_id, ask_nodes.depth + 1
		from ask_nodes
		cross join lateral (
			select distinct t.from_id from graph_tuples t
			where t.to_kind = '` + blockKind + `' and t.to_id = ask_nodes.id and t.kind = '` + textParentKind + `'
			  and t.from_kind = '` + blockKind + `'
		) child
		where ask_nodes.depth < ` + askBlockDepth + `
		  and (` + currentBlockTextParentSQL("child.from_id") + `) = ask_nodes.id
	)`
}

// InsideAskBlockSQL is a boolean SQL expression, true where the block node of the node_search row
// whose address and text the SQL expressions id and text name is an ask block or stands in one
// (NodeSearchAskNodesSQL's set): its own text opens an ask block, or a block above it by its
// current block parents (currentBlockTextParentSQL), to askBlockDepth, has a newest text tuple
// that asserts and opens one. It is the form for a reader that needs one node of a few and stops
// at the first, the related-item check's probe of a lexeme, where the set read whole would cost
// more than the climbs it saves. id and text are spelled by the caller and never a value a
// request carried.
func InsideAskBlockSQL(id, text string) string {
	return `(` + askTextSQL(text) + ` or exists (
		with recursive text_up(id, depth) as (
			select parent.to_id, 1 from (` + currentBlockTextParentSQL(id) + `) parent
			union all
			select parent.to_id, text_up.depth + 1
			from text_up
			cross join lateral (` + currentBlockTextParentSQL("text_up.id") + `) parent
			where text_up.depth < ` + askBlockDepth + `
		)
		select 1 from text_up
		cross join lateral (` + currentBlockAskTextSQL("text_up.id") + `) newest
		where newest.opens_ask
	))`
}
