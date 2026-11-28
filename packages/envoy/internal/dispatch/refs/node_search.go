package refs

import "github.com/sjawhar/envoy/internal/dispatch/graphstore"

// NodeSearchBlockSQL is the block id of the node a node_search row names, alias the row: the part
// of its address after the first '#', since a ref_key holds none (migration 0121).
func NodeSearchBlockSQL(alias string) string {
	return `substr(` + alias + `.node_address, strpos(` + alias + `.node_address, '#') + 1)`
}

// NodeSearchOwnerSQL is the owner part of the address of node_search row alias: the ref_key before
// the first '#', the room's tree artifact's for a block of an issue room and the document's own
// for a document that is its own room (migration 20261125160000_issue_tree_node_search).
func NodeSearchOwnerSQL(alias string) string {
	return `split_part(` + alias + `.node_address, '#', 1)`
}

// NodeSearchOwnersSQL is the body of a CTE over artifacts, alias owner, answering id, ref_key and
// kind of each artifact whose ref_key can be the owner part of a node the reader answers: every
// tree artifact (an issue room), and every doc that docs, an SQL condition over owner, admits. It
// leaves out a document converted into its issue's room (document_rooms): its own ref_key is the
// old owner's address, which this build's search reads none of (R17 of the issue-tree spec).
//
// The readers join a node to its owner here once per owner rather than probing artifacts once per
// node: a common word matches tens of thousands of nodes of a few thousand documents. docs may
// narrow the docs by a column of the document itself, because a node whose owner is a doc is a
// block of that doc (NodeSearchDocumentSQL), so the reader applies the same condition to the
// node's document again and no row moves. A tree is never narrowed here: its nodes are blocks of
// the documents of its fragments, which only the reader's own test of the document decides.
func NodeSearchOwnersSQL(docs string) string {
	return `select owner.id, owner.ref_key, owner.kind from artifacts owner
		where (owner.kind = 'tree' or owner.kind = 'doc' and ` + docs + `)
		  and not exists (select 1 from document_rooms moved where moved.artifact_id = owner.id)`
}

// NodeSearchDocumentSQL is the document of node_search row alias whose owner is the artifacts row
// owner (NodeSearchOwnersSQL), as SQL text answering a uuid: for a node of an issue room, the
// document whose fragment of the room holds the block (graphstore.RoomBlockRowSQL), and the tree
// itself for a block of the room's own fragment; for any other owner, the owner. issue_room_blocks
// keys a row only by a tree artifact (migration 20261117045607_issue_tree_rooms: one row per
// document_blocks row of an artifact of kind tree; its writers, refs' queueRoomBlocks and
// planRoomTypedBlocks, both key by the room's tree), so for an owner of another kind
// RoomBlockRowSQL answers null and the owner is the document, which the case answers without the
// probe and without the block_id_key digest the probe computes for every node it reads.
//
// Search uses this to answer the document of a matching block. The related and duplicate checks
// read document rows directly and need no block-owner resolution.
func NodeSearchDocumentSQL(alias, owner string) string {
	return `coalesce(case when ` + owner + `.kind = 'tree' then ` +
		graphstore.RoomBlockRowSQL(owner+".id", NodeSearchBlockSQL(alias)) + ` end, ` + owner + `.id)`
}

// SearchVisibilityJoin joins a search candidate's artifact address, then joins only a node that
// the registry hides from the acting credential. The caller keeps candidates with no such row.
// This is VisibleEnd's test over search's non-standing kinds: a missing node or a null visibility
// test never hides a candidate. An anti-join lets Postgres choose between indexed lookups for
// small result sets and filtering many candidates together for a broadly matching query.
func SearchVisibilityJoin(kind, id, param string) string {
	visible := VisibilityMarker + `every kind */` + visibleHeldRow("hidden", authorIsOfRow("hidden", param))
	return `
  left join artifacts visible_artifact on ` + kind + ` = 'document' and visible_artifact.id::text = ` + id + `
  left join graph_nodes hidden
    on hidden.kind = case ` + kind + ` when 'document' then 'artifact' else ` + kind + ` end
   and hidden.id = case ` + kind + ` when 'document' then visible_artifact.ref_key else ` + id + ` end
   and not (` + visible + `)`
}
