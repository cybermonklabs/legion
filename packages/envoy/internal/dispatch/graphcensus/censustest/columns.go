// Package censustest holds the columns of each table that projects the node and tuple store that
// the store states (ProjectedColumns). Two tests read it: api's scan of the projected tables'
// writers (TestNoProjectionRowIsWrittenWithoutTheStore) holds every write that makes or unmakes a
// row of one of those tables or sets one of those columns to the store writer's call path, and
// graphcensus's own test (TestEveryColumnTheCensusComparesIsOneTheProjectionScanHolds) holds the
// list to the census, so a column the census compares cannot be left out of it. It is an ordinary
// package rather than a _test.go file because Go cannot share test-only code across packages, and
// it imports nothing of graphcensus, so graphcensus's own tests can import it.
package censustest

// ProjectedColumns are the columns of each projected table that the store states, each with
// what states it. A row's existence and these columns are what the row's node and its tuples
// state - the node's address, when and by whom it was made, and each tuple's near end, kind, far
// end, time and legacy_source_seq - so an insert, a delete and an update that sets one of them
// restates the store, and the store writer's call path must hold it. Every other column - a
// comment's body and its resolution, a block's excerpt - restates nothing the store holds, so an
// update of one is listed with the call path it sits in and passes.
//
// The columns are keyed by their table, because one name says two things in two tables: a
// comment's artifact_id is the artifact that owns it, and a block's and a block link's is the
// document the block is a member of.
//
// The list is read off the writers of the tuples the rows project - graphstore.CommentFacts and
// graphstore.AuthorFacts for a comment, refs' membership, parentTuple and assertedLink for a
// block and its links, and graphcensus's backfill of each table - and written out here, since
// those writers build tuples from values and name no column a test could read. The census reads
// the same columns to hold each projection against the store (the row sides of graphcensus's
// projections), and every column of a projected table it reads must stand here (graphcensus's
// TestEveryColumnTheCensusComparesIsOneTheProjectionScanHolds), so a column the census compares
// cannot be left out of this list.
var ProjectedColumns = map[string]map[string]string{
	"comments": {
		// A comment's node is addressed by the row's own id (graphstore.KindComment), and every
		// tuple of the comment is a tuple out of that address.
		"id":           "the comment's node, as its address",
		"node_kind":    "the row's node",
		"node_address": "the row's node",
		// The node is made at the row's created_at by the actor its author column holds, and
		// each tuple of the comment is dated from that time (graphstore.CommentFacts).
		"created_at": "the comment's node, as its made_at, and its tuples, as their time",
		// The author is the far end of the comment's authored_by tuple, the person or session
		// graphstore.AuthorOf reads off it, and the alias_of and recorded_as tuples
		// graphstore.AuthorFacts states beside it are read off it too; it is the node's made_by.
		"author": "the comment's authored_by tuple, and its node, as its made_by",
		// A comment's anchor is its anchored_to tuple, to the artifact the anchor's artifact_id
		// key names (graphstore.CommentFacts). A jsonb_set of the anchor's other keys alone keeps
		// that artifact and sets no projected column (api's anchorWrite); a write of the whole
		// anchor, or of its artifact_id key, sets it.
		"anchor": "the comment's anchored_to tuple",
		// The artifact that owns the comment is the far end of its owned_by tuple, by that
		// artifact's ref_key (graphstore.CommentFacts).
		"artifact_id": "the comment's owned_by tuple",
		// The comment's issue, with its artifact_id and its id, finds the comment.created event
		// the backfill takes as the cause of every tuple it states for the comment
		// (graphcensus's readCommentRows).
		"issue_key": "the comment's tuples, as the event that is their cause",
		// A comment's pointers are tuples out of its node (graphstore.CommentPointerFacts): reply_to
		// states replies_to the comment, ask_id replies_to the ask, answers states answers, and
		// replies_to_block states replies_to the block's node, with one part_of under what the
		// comment answers. tuples_stored marks the rows whose pointers the store states.
		"reply_to":         "the comment's replies_to tuple, and its part_of",
		"ask_id":           "the comment's replies_to tuple, and its part_of",
		"answers":          "the comment's answers tuple, and its part_of",
		"replies_to_block": "the comment's replies_to tuple",
		"tuples_stored":    "the comment's pointer tuples, as whether the store states them",
	},
	"document_blocks": {
		"node_kind":    "the row's node",
		"node_address": "the row's node",
		// A block's node is addressed by its document's ref_key and its block_id (refs'
		// blockNodeAddress), which node_id writes whole (migration 0121), and every tuple of the
		// block is a tuple out of that address.
		"node_id":  "the block's node, as its address",
		"block_id": "the block's node, as its address",
		// The document is the far end of the block's in_document tuple (refs' membership), and
		// of its part_of tuple where no block holds it (refs' parentTuple).
		"artifact_id": "the block's node, as its address, and its in_document tuple",
		// A document_blocks row's parent_block_id is its block's part_of tuple, to the block of
		// the same document that holds it (refs/blocks.go).
		"parent_block_id": "the block's part_of tuple",
		// The in_document tuple is dated from the row's own created_at (refs.TupleInDocument).
		"created_at": "the block's in_document tuple, as its time",
		// The version the block was last written at finds the artifact.version event the
		// backfill takes as the cause of the block's in_document, visibility and state tuples
		// (graphcensus's readBlockRows).
		"changed_version": "the block's in_document, visibility and state tuples, as the event that is their cause",
	},
	"block_refs": {
		"node_kind":    "the row's node",
		"node_address": "the row's node",
		// A block_refs row is one tuple out of a block (refs' assertedLink): out of the block its
		// document and block_id address, which from_node writes whole, under the link's own kind
		// - a pointer attribute's name, or mentions - to the target to_kind and to_id name, dated
		// from the row's created_at.
		"artifact_id": "the link's tuple, as the block it is out of",
		"block_id":    "the link's tuple, as the block it is out of",
		"from_node":   "the link's tuple, as the block it is out of",
		"kind":        "the link's tuple, as its kind",
		"to_kind":     "the link's tuple, as its far end",
		"to_id":       "the link's tuple, as its far end",
		"created_at":  "the link's tuple, as its time",
		// A block_refs row's source_seq is the event that stamped it, which the backfill reads as
		// the cause of the row's tuple and as that tuple's legacy_source_seq (graphcensus's
		// backfillBlockRefs), and which a pass's own tuple takes from its cause where the row is
		// stamped in the same transaction (graphstore.UncausedTuple's LegacyFromCause).
		"source_seq": "the link's tuple, as its legacy_source_seq",
	},
	"messages": {
		// A message's node is addressed by the row's own id (graphstore.MessageNode), and every
		// tuple of the message is a tuple out of that address.
		"id":           "the message's node, as its address",
		"node_kind":    "the row's node",
		"node_address": "the row's node",
		// The node is made at the row's created_at by the actor its author column holds, and each
		// tuple of the message is dated from that time (graphstore.MessageFacts).
		"created_at": "the message's node, as its made_at, and its tuples, as their time",
		// The author is the far end of the message's authored_by tuple (graphstore.AuthorFacts),
		// and the node's made_by.
		"author": "the message's authored_by tuple, and its node, as its made_by",
		// What the message answers is its replies_to and part_of tuples (graphstore.MessageFacts).
		"in_reply_to":      "the message's replies_to and part_of tuples",
		"replies_to_block": "the message's replies_to tuple, and its part_of where it answers no message",
		// A message that answers nothing sits on the issue it was posted on, or on the page of the
		// agent its target routes to, which is its addressed_to as well.
		"issue_key": "the message's part_of tuple",
		"target":    "the message's addressed_to tuple, and its part_of the agent's page",
		// A broadcast copy states its broadcast_id as a value.
		"broadcast_id": "the message's broadcast_id value",
	},
}
