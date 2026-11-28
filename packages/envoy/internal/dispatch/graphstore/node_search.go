package graphstore

import (
	"strings"

	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// node_search (migration 20261125160000_issue_tree_node_search) holds two kinds of search row in
// place of document_search (R17 of the issue-tree spec):
//
//   - Block rows (node_kind 'block'): one row for each block node the store holds present whose
//     newest text value (AttributeText) asserts, holding that value, the seq of the tuple that
//     states it and the search vector of it. Search's document arm reads these rows and answers
//     the block node that matched. The row of an ask block, and of each block that stands in one,
//     is held like any other; search leaves those nodes out of what it matches
//     (NodeSearchAskNodesSQL).
//   - Document rows (node_kind 'artifact'): one row for each live document artifact
//     (artifacts.kind = 'doc', retired_at is null, and whose artifact node is not gone) holding a
//     version with markdown, keyed by its ref_key, holding empty text, the newest markdown
//     version's number as text_seq and that version's search vector (artifact_versions.search,
//     built by artifact_versions_search() over the document's own fragment's markdown). The
//     related-item check and the near-copy gate rank documents from these rows, reading one vector
//     per document as document_search let them in the releases before.
//
// QueueHeads keeps both projections in the writer's transaction, before writing the head rows.
// Block owners name the documents of a block pass; artifact and version nodes also name their
// documents, including a decisions query that writes no blocks and an artifact stated gone.

// QueueNodeSearch queues on b the write of the node_search rows of the block nodes ids, read again
// from what the store holds when the statement runs (nodeSearchWriteStatement): the row of each
// one present whose newest text value asserts, written where it holds another tuple's text, and
// the row of each other deleted. Where written is not nil it adds the rows the statement inserts
// or rewrites to it. ids that name nothing queue nothing.
func QueueNodeSearch(b *store.WriteBatch, ids []string, written *int64) {
	if len(ids) == 0 {
		return
	}
	counted(b.Queue("write the search rows of the blocks written", nodeSearchWriteStatement, ids), written)
}

// QueueDocumentSearch queues on b the write of the node_search document rows of the artifacts
// whose ref_keys refKeys names, read again from what the store holds when the statement runs
// (documentSearchWriteStatement): the row of each live doc artifact holding a markdown version,
// written where the stored row is missing or holds a lower version's vector, and the row of each
// removed, retired or gone artifact deleted. Where written is not nil it adds the rows the
// statement inserts or rewrites to it. refKeys that name nothing queue nothing.
func QueueDocumentSearch(b *store.WriteBatch, refKeys []string, written *int64) {
	queueDocumentSearch(b, refKeys, nil, true, written)
}

func queueDocumentSearch(b *store.WriteBatch, refKeys, blockOwners []string, backfill bool, written *int64) {
	if len(refKeys) == 0 && len(blockOwners) == 0 {
		return
	}
	counted(b.Queue("write the search rows of the documents written", documentSearchWriteStatement, refKeys, blockOwners, backfill), written)
}

// queueNodeSearch queues the block rows and names their owners to the document-row write. It
// also names each artifact and the artifact of each version, even if the write holds no blocks.
func queueNodeSearch(b *store.WriteBatch, nodes []NodeKey) {
	var blocks, documents []string
	var owners map[string]struct{}
	for _, node := range nodes {
		switch storedKindOf(node.Kind) {
		case blockKind:
			blocks = append(blocks, node.ID)
			if owner, _, ok := strings.Cut(node.ID, "#"); ok && !strings.HasPrefix(owner, KindLens+"/") {
				if owners == nil {
					owners = make(map[string]struct{})
				}
				owners[owner] = struct{}{}
			}
		case KindArtifact:
			documents = append(documents, node.ID)
		case KindVersion:
			if end := strings.LastIndex(node.ID, "@v"); end > 0 {
				documents = append(documents, node.ID[:end])
			}
		}
	}
	QueueNodeSearch(b, blocks, nil)
	blockOwners := make([]string, 0, len(owners))
	for owner := range owners {
		blockOwners = append(blockOwners, owner)
	}
	queueDocumentSearch(b, documents, blockOwners, false, nil)
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

// documentSearchWriteStatement reads the latest version with markdown under each document's own
// ref_key, including for a fragment of a room. The vector is the one the version trigger already
// computed, exactly what the old document_search trigger copied. No document row duplicates the
// markdown. text_seq records the version that supplied the stored vector; an identical vector
// does not rewrite it, including when the drain writes an unchanged rendering.
//
// A block pass over a room names the tree as its block owner. Expand that owner to its member
// documents once, not once per block. On the live path the version must have passed the block
// fence: a deferred pass leaves the old vector (or no row) until it runs. The decisions query's
// first version stands an empty block index, and the drain stands each subsequent version.
// Backfill can fill the latest vector without that fence, including for a document never indexed
// by this build. Both paths remove the row of a removed, retired or gone document.
var documentSearchWriteStatement = `
	with named as (
		select ref_key from unnest($1::text[]) named(ref_key)
		union
		select ref_key from unnest($2::text[]) owner(ref_key)
		union
		select member.ref_key
		from unnest($2::text[]) owner(ref_key)
		join artifacts room on room.ref_key = owner.ref_key and room.kind = 'tree'
		join document_rooms d on d.room = room.id
		join artifacts member on member.id = d.artifact_id
	),
	said as (
		select named.ref_key, newest.number, newest.search,
		       n.state <> '` + StateGone + `' and art.kind = 'doc'
		         and art.retired_at is null and newest.search is not null as present,
		       ($3::boolean or stand.version >= newest.number) as ready
		from named
		left join lateral (
			select a.id, a.kind, a.retired_at from artifacts a
			where a.ref_key = named.ref_key
			limit 1
		) art on true
		left join graph_nodes n on n.kind = '` + KindArtifact + `' and n.id = named.ref_key
		left join document_block_versions stand on not $3 and stand.artifact_id = art.id
		left join lateral (
			select v.number, v.search from artifact_versions v
			where v.artifact_id = art.id and v.markdown is not null
			order by v.number desc
			limit 1
		) newest on art.id is not null and art.kind = 'doc'
	),
	held as (
		select said.ref_key, said.number, said.search from said
		where said.present and said.ready
	),
	dropped as (
		delete from node_search s
		using said
		where s.node_kind = '` + KindArtifact + `' and s.node_address = said.ref_key
		  and not coalesce(said.present, false)
	)
	insert into node_search (node_kind, node_address, text, text_seq, search)
	select '` + KindArtifact + `', held.ref_key, '', held.number, held.search
	from held
	where not exists (
		select 1 from node_search s
		where s.node_kind = '` + KindArtifact + `' and s.node_address = held.ref_key
		  and ((not $3 and s.text_seq > held.number) or s.text = '' and s.search = held.search)
	)
	order by held.ref_key
	on conflict (node_kind, node_address) do update
	   set text = excluded.text, text_seq = excluded.text_seq, search = excluded.search
	 where ($3 or excluded.text_seq >= node_search.text_seq)
	   and (node_search.text <> '' or excluded.search is distinct from node_search.search)`

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

// NodeSearchAskNodesSQL is a CTE of a WITH RECURSIVE, ask_nodes (id, depth), holding every block
// node that is an ask block or stands in one, of the owners the relation owners names (a CTE or
// table with a column ref_key, the owner part of a node's address, refs.NodeSearchOwnerSQL): each
// node whose own text opens an ask block (askTextSQL), or above which a block, climbed to through
// current block parents (currentBlockTextParentSQL) to askBlockDepth, has a newest text tuple that
// asserts and opens one; read once for a statement rather than once for each node a word matched.
// A reader leaves out of what it matches each node whose address the set holds.
//
// The set is walked down from the ask blocks rather than up from the matches, because the ask
// blocks are few and a common word matches tens of thousands of nodes. Its first rows are the
// node_search rows whose text opens an ask block, read through node_search_ask_fences, the
// partial index of those rows by their owner; each further row is a block whose current block
// parent (currentBlockTextParentSQL) is a row already found, read off the tuples into that row
// (graph_tuples_as_of_in) and kept only where the newest such parent is the row, to askBlockDepth.
//
// The walk down holds the set the climb up defines, node for node:
//
//   - The climb goes from a node through its current block parent, a function: each node has at
//     most one. So a node x is above an ask block f at distance d (d at most askBlockDepth) on
//     that climb exactly where f is d steps up the same function from x, which is where the walk
//     down from f reaches x at depth d. A cycle of text_parent tuples is cut at the same depth
//     both ways, and `union` drops a node found again at a depth it already holds.
//   - The climb tests a node's newest text tuple; the walk starts from node_search's text.
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
// Only block rows of node_search carry block text and ask fences (node_kind = 'block'), which
// matches the partial index node_search_ask_fences; document rows (node_kind = 'artifact') hold
// empty text and are never ask blocks.
func NodeSearchAskNodesSQL(owners string) string {
	return `ask_nodes(id, depth) as (
		select fence.node_address, 0
		from ` + owners + ` fence_owner
		join node_search fence on fence.node_kind = '` + blockKind + `'
		  and split_part(fence.node_address, '#', 1) = fence_owner.ref_key
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
