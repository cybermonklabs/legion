package graphstore

import (
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// KindComment is what this store calls a comment, and the supertag every one of them carries
// (migration 20261109051232), as a person's kind is its supertag. A comment's address is its
// row's own id, which is what graph_edges names it by and what comments.node_kind and
// comments.node_address hold.
const KindComment = "comment"

// The two relations a comment holds: the artifact its anchor names, and the artifact it belongs
// to. Each is the fact graph_edges reads off a comments row, and a comment has either, both or
// neither.
const (
	TupleAnchoredTo = "anchored_to"
	TupleOwnedBy    = "owned_by"
)

// TupleAnchoredIn is the relation from a comment to the block its anchor names (T17b of
// dispatch://INFRA-1253's plan), beside anchored_to, which names the anchor's document alone: the
// block comments.anchor's block_id names in that document, addressed as the block index addresses
// it (AnchorBlockAddressSQL). comments.anchor's block_id is its projection while the anchor is not
// orphaned: an anchor whose mark the refresh found gone stands in no block. It is a kind
// refs.TupleKinds does not list, so graph_edges answers nothing under it.
const TupleAnchoredIn = "anchored_in"

// AnchoredIn is the anchored_in tuple of comment id to the block node at address block, stating
// the relation where asserted and retracting it otherwise, dated at at, written by who and caused
// by event.
func AnchoredIn(id, block string, asserted bool, who model.Actor, at time.Time, event int64) Tuple {
	return Tuple{
		FromKind: KindComment, FromID: id, Kind: TupleAnchoredIn, ToKind: blockKind, ToID: block,
		At: at, Who: who, SourceEvent: event, Asserted: asserted,
	}
}

// RoomRefKeySQL is the ref_key of the room the document artifact (SQL text answering a uuid) is
// edited in, as SQL text: the issue's tree artifact its document_rooms row names, for a document of
// an issue room (T12a of dispatch://INFRA-1253's plan), and the document's own ref_key otherwise.
// The block index keys a room's blocks under that tree (refs.IndexBlocks), so a block's node
// address starts with it.
func RoomRefKeySQL(artifact string) string {
	return `(select coalesce(room.ref_key, document.ref_key)
		from artifacts document
		left join document_rooms moved on moved.artifact_id = document.id
		left join artifacts room on room.id = moved.room
		where document.id = ` + artifact + `)`
}

// AnchorBlockAddressSQL is the address of the block node the anchor of the comments row aliased
// alias names, as SQL text answering text, whether or not the anchor is orphaned: the ref_key of
// the room its document is edited in (RoomRefKeySQL), '#' and the anchor's block_id, as the block
// index addresses that block. It is null where the row holds no anchor or its anchor names no
// block.
func AnchorBlockAddressSQL(alias string) string {
	anchor := alias + ".anchor"
	address := RoomRefKeySQL(`(`+anchor+`->>'artifact_id')::uuid`) + ` || '#' || (` + anchor + `->>'block_id')`
	return `(case when nullif(` + anchor + `->>'block_id', '') is not null
		then ` + address + ` end)`
}

// CommentAnchoredInSQL is the far end of the anchored_in the comments row aliased alias states,
// as SQL text answering text: its anchor's block (AnchorBlockAddressSQL) while the anchor is not
// orphaned, and null for an orphaned anchor, an anchor that names no block, and a row with no
// anchor. It is the one reading of what a row states that the census of the tuple and its backfill
// read; an address past the store's bound is no node, which each of them leaves out.
func CommentAnchoredInSQL(alias string) string {
	return `(case when not coalesce((` + alias + `.anchor->>'orphaned')::boolean, false)
		then ` + AnchorBlockAddressSQL(alias) + ` end)`
}

// CommentFacts is what this store holds of one comment beside who wrote it (AuthorFacts): its
// node - project-visible, live, in project or between parties (the issue's or the document's it
// belongs to, ScopeOf), made by author at at and caused by event - and the artifacts it is
// anchored to and owned by, appended to tuples. Each relation names its artifact by the
// artifact's address, its ref_key, and is stated only where the comment has one: an empty
// address states none, as graph_edges' own join drops an anchor no artifact row answers.
//
// The request that writes a comment sets on the node the visibility its body asked for, as
// VisibilityFor answered it (api's commentStored); the seed and the backfill keep the project
// visibility. A comment is a kind no node of which the store takes reserved for its author
// (AdmitAuthorOnly), so a comment at VisibilityAuthor is refused by the write it reaches, with a
// VisibilityNotAdmittedError naming the kind, and nothing of it is stored.
//
// It is the one statement of these facts, which the request that writes a comment
// (api.commentStored), the seed and the backfill of the comments written before the store
// existed all write. None of them states the node's base attributes: Write states them for a
// node its insert makes, at the node's own time, writer and cause, its project and parties
// among them. None of them owns the comment's state or visibility either, so the node restates
// the comment (Node.Restates): written again, it keeps the state and the visibility the store
// holds, unless the comment was gone, when it comes back live.
//
// The tuples are appended rather than answered on their own, so a writer stating a batch of
// comments grows one slice rather than making one per comment.
func CommentFacts(tuples []Tuple, id string, author model.Actor, at time.Time, event int64, anchor, owner, project string, parties []string) (Node, []Tuple) {
	node := Node{
		Kind: KindComment, ID: id, Supertag: KindComment,
		Visibility: VisibilityProject, State: StateLive, Project: project, Parties: parties,
		MadeAt: at, MadeBy: author, SourceEvent: event, Restates: true,
	}
	for _, relation := range [...]struct{ kind, address string }{{TupleAnchoredTo, anchor}, {TupleOwnedBy, owner}} {
		if relation.address == "" {
			continue
		}
		tuples = append(tuples, Tuple{
			FromKind: KindComment, FromID: id, Kind: relation.kind,
			ToKind: "artifact", ToID: relation.address,
			At: at, Who: author, SourceEvent: event, Asserted: true,
		})
	}
	return node, tuples
}
