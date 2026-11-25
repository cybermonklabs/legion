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
// answer names that node.
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
