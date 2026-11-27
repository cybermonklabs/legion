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

// TupleAnswers is what a reply that names a comment means: the comment it answers, which
// comments.answers holds beside the head of its thread (api's threadHeadOf), under the name
// graph_edges has always given that edge (20261105014808_comments_answers.up.sql). A reply's
// TupleRepliesTo points at that head - the root comment reply_to names, or the ask ask_id names -
// and at the block replies_to_block names.
const TupleAnswers = "answers"

// TupleDelivery is a comment's urgency: the value tuple out of its node naming the one delivery
// mode - btw, aside or steer - its writer applied to every target it names as a delivery route
// (api's validateCommentMentions), which comment_mentions.delivery holds for each of them.
const TupleDelivery = "delivery"

// CommentPointers is what a comments row says of what its comment answers, each column "" where
// the row holds null: ReplyTo the root comment of its thread (reply_to), AskID the ask its thread
// hangs under (ask_id), Answers the comment its writer answered (answers), and RepliesToBlock the
// block it answers (replies_to_block), which is the address of that block's node:
// `comment/<comment id>#<block id>` for a comment's block and `ask/<ask id>#<part id>` for an ask's
// part, the node id comment_blocks and ask_blocks hold for it (KindBlock).
type CommentPointers struct {
	ReplyTo, AskID, Answers, RepliesToBlock string
}

// CommentPointerFacts is what this store holds of one comment's pointers beside its node and the
// artifacts it names (CommentFacts): one tuple per pointer its row holds, out of the comment's
// node, made by author at at and caused by event, appended to tuples.
//
//   - reply_to states replies_to the comment it names, ask_id replies_to the ask, answers states
//     answers the comment, and replies_to_block states replies_to the block's node (KindBlock),
//     each as the column holds it. A reply that names a comment holds both the head of its thread
//     and the comment it answers, so it states both.
//   - One part_of places the comment under the node it answers, a node having one parent and a
//     reply's being what it answers (R7): the comment answers names, else the comment reply_to
//     names, else the ask. A comment that answers nothing states no part_of here.
//
// stored is false, and nothing is appended, where an end is past the store's address bound: the
// writer then leaves the row's tuples_stored false, so the old arms of graph_edges answer every
// pointer of that row, rather than the store answering it short the pointer the bound dropped.
//
// It is the one statement of these facts, which every request path that writes a comment (api's
// commentStoreWrite) and the seed write.
func CommentPointerFacts(tuples []Tuple, id string, pointers CommentPointers, author model.Actor, at time.Time, event int64) ([]Tuple, bool) {
	for _, end := range [...]string{pointers.ReplyTo, pointers.AskID, pointers.Answers, pointers.RepliesToBlock} {
		if !AddressFits(end) {
			return tuples, false
		}
	}
	stated := func(kind, toKind, toID string) Tuple {
		return Tuple{
			FromKind: KindComment, FromID: id, Kind: kind, ToKind: toKind, ToID: toID,
			At: at, Who: author, SourceEvent: event, Asserted: true,
		}
	}
	if pointers.ReplyTo != "" {
		tuples = append(tuples, stated(TupleRepliesTo, KindComment, pointers.ReplyTo))
	}
	if pointers.AskID != "" {
		tuples = append(tuples, stated(TupleRepliesTo, KindAsk, pointers.AskID))
	}
	if pointers.Answers != "" {
		tuples = append(tuples, stated(TupleAnswers, KindComment, pointers.Answers))
	}
	if pointers.RepliesToBlock != "" {
		tuples = append(tuples, stated(TupleRepliesTo, KindBlock, pointers.RepliesToBlock))
	}
	switch {
	case pointers.Answers != "":
		tuples = append(tuples, stated(TuplePartOf, KindComment, pointers.Answers))
	case pointers.ReplyTo != "":
		tuples = append(tuples, stated(TuplePartOf, KindComment, pointers.ReplyTo))
	case pointers.AskID != "":
		tuples = append(tuples, stated(TuplePartOf, KindAsk, pointers.AskID))
	}
	return tuples, true
}

// CommentRoute is one delivery route a comment names, as its comment_mentions row records it: the
// route its target spells (model.ParseRoute: `session:<id>` or `role:<topic>`), and the session
// that route resolved to, nil where it resolved to none.
type CommentRoute struct {
	Target  string
	Session *string
}

// CommentMentionFacts is what this store holds of the delivery routes one comment names (R5),
// appended to tuples, which hold what the comment's index states already: a mentions tuple out of
// the comment's node to the session or the role each route's target names, a second to the
// session it resolved to, and, where it names any route, the comment's urgency (TupleDelivery),
// delivery, the one mode the comment's writer applies to every route (api's
// validateCommentMentions). A mention tuples holds already - an identity the comment's text names,
// or one an earlier route named - is stated once, and a target no route reads, or an end past the
// store's address bound, is none. Each fact is made by author at at.
//
// Each fact is stated under no event: the writer states it under the event that caused the
// comment (Cause), the request through QueueCaused after the comment's event and the backfill
// through Cause.Append. Each mention is marked LegacyFromCause, as a mention the comment's text
// names is (refs' stateMention), so it takes that event as its legacy_source_seq too, where it is
// an event: graph_edges' identity arm answers that as the edge's source_seq, so a ?since= read of
// References whose window holds the comment's event lists the route. The delivery value is no
// mention and takes the event as its source_event alone; a mention stated at the sentinel carries
// none and stays out of that read.
//
// It is the one statement of these facts, which the request that writes a comment (api's
// commentMentionFacts) and the backfill of the routes an older build's comment names
// (graphcensus' writeCommentMentions) both write.
func CommentMentionFacts(tuples []UncausedTuple, id string, routes []CommentRoute, delivery string, author model.Actor, at time.Time) []UncausedTuple {
	if len(routes) == 0 {
		return tuples
	}
	stated := make(map[[2]string]struct{}, 2*len(routes))
	for _, held := range tuples {
		if held.Kind == TupleMentions && held.FromKind == KindComment && held.FromID == id {
			stated[[2]string{held.ToKind, held.ToID}] = struct{}{}
		}
	}
	state := func(kind, to string) {
		end := [2]string{kind, to}
		if _, held := stated[end]; held || !AddressFits(to) {
			return
		}
		stated[end] = struct{}{}
		tuples = append(tuples, UncausedTuple{Tuple: Tuple{
			FromKind: KindComment, FromID: id, Kind: TupleMentions, ToKind: kind, ToID: to,
			At: at, Who: author, Asserted: true,
		}, LegacyFromCause: true})
	}
	for _, route := range routes {
		if parsed, err := model.ParseRoute(route.Target); err == nil {
			state(parsed.Kind, parsed.ID)
		}
		if route.Session != nil {
			state(KindSession, *route.Session)
		}
	}
	return append(tuples, UncausedTuple{Tuple: Tuple{
		FromKind: KindComment, FromID: id, Kind: TupleDelivery, ToKind: KindValue, ToID: delivery,
		At: at, Who: author, Asserted: true,
	}})
}

// CommentFacts is what this store holds of one comment beside who wrote it (AuthorFacts) and what
// it answers (CommentPointerFacts): its node - project-visible, live, in project or between
// parties (the issue's or the document's it belongs to, ScopeOf), made by author at at and caused
// by event - and the artifacts it is anchored to and owned by, appended to tuples. Each relation
// names its artifact by the artifact's address, its ref_key, and is stated only where the comment
// has one: an empty address states none, as graph_edges' own join drops an anchor no artifact row
// answers.
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
