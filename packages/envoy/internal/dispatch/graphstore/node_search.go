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
// like any other; the readers leave those nodes out of what they match (InsideAskBlockSQL).
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

// askBlockDepth bounds InsideAskBlockSQL's climb through a block's text parents: the most blocks a
// document may open inside one another (pmdoc's maxNesting, which this package may not import).
const askBlockDepth = "100"

// InsideAskBlockSQL is a boolean SQL expression, true where the block node whose id the SQL text id
// names is an ask block or stands in one: the block itself, or a block above it by the text_parent
// tuples the store holds now, opens an ask block. A text_parent is read as every current relation
// is (CurrentTupleRead): the newest tuple of each far end, kept while it asserts, and only a far
// end that is a block is climbed to, so the climb ends at the node of the document, where no block
// holds the text, and at askBlockDepth. A block opens an ask block where its newest text value
// asserts and begins with the fence of a block of the type ask, three colons or more and `ask{`,
// the opening pmdoc's ParseTypedDirectiveOpen reads; a block's text begins at its own fence, in
// whatever container it stands. The cheap prefix test comes first, so a text that opens no fence is
// never matched against the pattern.
//
// It is the test the readers of node_search apply to the rows their word matched (search's
// document arm, the near-copy gate's scoring and the related-item check's probe of a lexeme), so
// an ask block's words, and those of a block standing in one, find the ask and never the document,
// as search_document_text removes the ask blocks of a document's text. The writer, the census and
// the backfill do not apply it: node_search holds a row for every block node that holds text, and
// a retype of a block's ancestor moves what the readers answer with no write of the block. It is a
// correlated read of one block's ancestry, each step one probe of graph_tuples_current and one of
// graph_tuples_values. id is spelled by the caller and is never a value a request carried.
//
// One case is not caught. An ask block whose node address is longer than MaxAddressBytes gets no
// node (refs' blockIDFits). The block index (refs' placeBlocks) puts each of its child blocks under
// the nearest ancestor whose address fits. So no tuple names the ask, and the climb never reaches
// it. A word in such an ask's options is then a document hit as well as the ask hit the asks leg
// gives.
func InsideAskBlockSQL(id string) string {
	return `exists (
		with recursive text_up (id, depth) as (
			select (` + id + `)::text, 0
			union all
			select parent.to_id, text_up.depth + 1
			from text_up
			cross join lateral (
				select standing.to_id from (
					` + CurrentTupleRead(`t.to_kind, t.to_id`, `t.to_kind, t.to_id, t.asserted, t.seq`,
		`t.from_kind = '`+blockKind+`' and t.from_id = text_up.id and t.kind = '`+textParentKind+`'
					  and t.to_kind <> '`+KindValue+`'`) + `
				) standing
				where standing.asserted and standing.to_kind = '` + blockKind + `'
				order by standing.seq desc
				limit 1
			) parent
			where text_up.depth < ` + askBlockDepth + `
		)
		select 1 from text_up
		cross join lateral (
			select t.asserted, t.to_id as text from graph_tuples t
			where t.from_kind = '` + blockKind + `' and t.from_id = text_up.id
			  and t.kind = '` + AttributeText + `' and t.to_kind = '` + KindValue + `'
			order by t.seq desc
			limit 1
		) newest
		where newest.asserted and starts_with(newest.text, ':::') and newest.text ~ '^:{3,}ask\{'
	)`
}
