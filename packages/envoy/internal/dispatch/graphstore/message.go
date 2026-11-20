package graphstore

import (
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// The relations a messages row's pointers become, each a tuple out of the message's node
// (MessageFacts). TupleRepliesTo is what a reply means - the message its in_reply_to names and
// the block its replies_to_block names - under the name graph_edges has always given that edge
// (20261105014808_comments_answers.up.sql). TupleAddressedTo points at the session or the role the
// row's target routes the message to. TupleBroadcastID is a value, the broadcast a copy belongs to,
// because a broadcast is no node kind of the store's closed list. Where a message sits - under
// what it answers, on the issue it was posted on, or on the page of the agent it was sent to - is
// a TuplePartOf, as a page's is.
//
// Only TupleRepliesTo is an arm of graph_edges, and so one of refs.TupleKinds(): the view answered
// a reply's edges before the store did, and answers nothing that would be an addressing or a
// placement.
const (
	TupleRepliesTo   = "replies_to"
	TupleAddressedTo = "addressed_to"
	TupleBroadcastID = "broadcast_id"
)

// KindBlock is the kind of a block's node: a document's, which the block index writes
// (refs.BlockNodeKind), and a comment's, a message's or an ask's, whose address is the node id
// its blocks table holds and a reply's replies_to_block names ('<item kind>/<item id>#<block
// id>'). The closed supertag list holds no comment_block, message_block or ask_block, so those
// three are address forms graph_edges renders and never kinds of the store.
const KindBlock = "block"

// MessageRow is what a messages row says of where its message stands, each column "" where the
// row holds null: the issue it was posted on, the message it answers (in_reply_to), the block it
// answers (replies_to_block, the address of that block's node), the route it was sent to
// (target: `session:<id>` or `role:<topic>`), and the broadcast it is one recipient's copy of.
// Page is no column of the row: it is the page the store gives the agent the target routes to
// (AgentPage), which the writer reads for a message that sits on that page (OnAgentPage).
type MessageRow struct {
	ID, IssueKey, InReplyTo, RepliesToBlock, Target, BroadcastID string
	Page                                                         string
}

// OnAgentPage is whether the message sits on the page of the agent it was sent to: it answers
// nothing, belongs to no issue, and has a target.
func (r MessageRow) OnAgentPage() bool {
	return r.InReplyTo == "" && r.RepliesToBlock == "" && r.IssueKey == "" && r.Target != ""
}

// MessageFacts is what this store holds of one message beside its node (MessageNode) and who
// wrote it (AuthorFacts): one tuple per pointer its row holds, made by author at at and caused
// by event, appended to tuples, and the node of the page it hangs from, where it hangs from one,
// appended to nodes.
//
//   - A reply states replies_to and part_of to the message it answers: what it means and where
//     it sits, a node having one parent and a reply's being what it answers (R7).
//   - replies_to_block states replies_to to the block's node (KindBlock), and where the row names
//     no message, the block is the message's parent as well.
//   - A message that answers nothing sits on the issue it was posted on, whoever it was sent to,
//     and one on no issue (OnAgentPage) on row.Page, the page the store gives the agent it was
//     sent to (AgentPage): the role's page for a role, and for a session the page of the role it
//     claimed or, where it claimed none, its own. That page is made here where the store does not
//     hold it yet (PageNode), so a message that starts a direct thread is a node on the page that
//     is that agent. A writer stating several messages in one write states each page once.
//   - A target states addressed_to the session or the role it routes to (model.ParseRoute).
//   - A broadcast copy states its broadcast_id, as a value.
//
// projected is false, and nothing is appended, where an end graph_edges answers - the message or
// the block a reply answers - is past the store's address bound: the row then names no node, so
// the old arms over messages answer both of its replies_to, as the comment path leaves out whole
// a comment whose relation it cannot key (api's commentStored). Any other end past the bound is
// no tuple and the message keeps the rest, as an author past the bound is no authored_by
// (AuthorFacts).
//
// It is the one statement of these facts: every request path that writes a message (api's
// writeMessageNode) and the seed write it.
func MessageFacts(nodes []Node, tuples []Tuple, row MessageRow, author model.Actor, at time.Time, event int64) ([]Node, []Tuple, bool) {
	if !AddressFits(row.InReplyTo) || !AddressFits(row.RepliesToBlock) {
		return nodes, tuples, false
	}
	stated := func(kind, toKind, toID string) Tuple {
		return Tuple{
			FromKind: KindMessage, FromID: row.ID, Kind: kind, ToKind: toKind, ToID: toID,
			At: at, Who: author, SourceEvent: event, Asserted: true,
		}
	}
	if row.InReplyTo != "" {
		tuples = append(tuples, stated(TupleRepliesTo, KindMessage, row.InReplyTo))
	}
	if row.RepliesToBlock != "" {
		tuples = append(tuples, stated(TupleRepliesTo, KindBlock, row.RepliesToBlock))
	}
	route, routed := routeNode(row.Target)
	switch {
	case row.InReplyTo != "":
		tuples = append(tuples, stated(TuplePartOf, KindMessage, row.InReplyTo))
	case row.RepliesToBlock != "":
		tuples = append(tuples, stated(TuplePartOf, KindBlock, row.RepliesToBlock))
	case row.IssueKey != "":
		if AddressFits(row.IssueKey) {
			tuples = append(tuples, stated(TuplePartOf, KindIssue, row.IssueKey))
		}
	case row.Page != "" && AddressFits(row.Page):
		nodes = append(nodes, PageNode(row.Page, author, at, event))
		tuples = append(tuples, stated(TuplePartOf, KindPage, row.Page))
	}
	if routed {
		tuples = append(tuples, stated(TupleAddressedTo, route.Kind, route.ID))
	}
	if row.BroadcastID != "" {
		tuples = append(tuples, stated(TupleBroadcastID, KindValue, row.BroadcastID))
	}
	return nodes, tuples, true
}

// routeNode is the node a message's target routes it to - a session or a role, whose kinds are
// the route's own words - and false for no target, for a target model.ParseRoute does not read,
// and for an id past the store's address bound, none of which names a node the store can key.
func routeNode(target string) (model.Route, bool) {
	if target == "" {
		return model.Route{}, false
	}
	route, err := model.ParseRoute(target)
	if err != nil || (route.Kind != KindSession && route.Kind != KindRole) || !AddressFits(route.ID) {
		return model.Route{}, false
	}
	return route, true
}
