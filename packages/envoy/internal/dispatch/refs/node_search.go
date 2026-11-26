package refs

import "github.com/sjawhar/envoy/internal/dispatch/graphstore"

// NodeSearchBlockSQL is the block id of the node a node_search row names, alias the row: the part
// of its address after the first '#', since a ref_key holds none (migration 0121).
func NodeSearchBlockSQL(alias string) string {
	return `substr(` + alias + `.node_address, strpos(` + alias + `.node_address, '#') + 1)`
}

// NodeSearchDocumentSQL is the document whose block the node of a node_search row is, alias the
// row (migration 20261125160000_issue_tree_node_search), as SQL text answering a uuid, and null for
// a node of no document this build's search reads: the artifact whose ref_key the address starts
// with, one probe of artifacts_ref_key_key, and for a block of an issue room, whose address names
// the room's tree artifact, the document whose fragment of the room holds the block
// (graphstore.RoomBlockRowSQL), the tree artifact itself for a block of the room's own fragment. A
// node under a document converted into its issue's room (document_rooms) is the old owner's
// address, which this build's search reads none of (R17 of the issue-tree spec), and a
// lens's finding, whose address names its lens, is of no document at all.
//
// Every read of node_search that answers a document reads the node's document through it: search's
// document arm, the related-item check's probe of a lexeme and the near-copy gate's scoring.
func NodeSearchDocumentSQL(alias string) string {
	return `(select coalesce(` + graphstore.RoomBlockRowSQL("node_owner.id", NodeSearchBlockSQL(alias)) + `, node_owner.id)
		from artifacts node_owner
		where node_owner.ref_key = split_part(` + alias + `.node_address, '#', 1)
		  and not exists (select 1 from document_rooms moved where moved.artifact_id = node_owner.id))`
}
