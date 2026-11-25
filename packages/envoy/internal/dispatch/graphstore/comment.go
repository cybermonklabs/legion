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
