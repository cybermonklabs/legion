package api

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// replyParentNode names a node as `<kind>:<id>`, the spelling every failure of this file prints a
// parent in.
func replyParentNode(kind, id string) string { return kind + ":" + id }

// replyParentInTree is the parent the tree read of issueKey with no view, GET
// /api/v1/issues/{key}/tree, answers alice for the one node of kind and id it holds, as
// `<kind>:<id>`, and "" where it answers none.
func replyParentInTree(t *testing.T, handler http.Handler, issueKey, kind, id string) string {
	t.Helper()
	node, _ := treeRead(t, handler, issueKey, "", "alice", false).node(t, kind, id)
	if node.Parent == nil {
		return ""
	}
	return replyParentNode(node.Parent.Kind, node.Parent.ID)
}

// replyParentInReferences is the parent member the references read, GET /api/v1/references?to=ref,
// answers for the node ref names, as `<kind>:<id>`, and "" where it answers none.
func replyParentInReferences(t *testing.T, handler http.Handler, ref string) string {
	t.Helper()
	parent := readNode(t, handler, ref).Parent
	if parent == nil {
		return ""
	}
	return replyParentNode(parent.Kind, parent.ID)
}

// replyParentStoredBlock is the replies_to_block column of the row id of table, a comment's or a
// message's, "" where it holds none.
func replyParentStoredBlock(t *testing.T, database *store.Store, table, id string) string {
	t.Helper()
	var block string
	if err := database.Pool.QueryRow(context.Background(),
		`select coalesce(replies_to_block, '') from `+table+` where id = $1::uuid`, id).Scan(&block); err != nil {
		t.Fatalf("read the replies_to_block of %s %s: %v", table, id, err)
	}
	return block
}

// replyParentValue is value, or "null" where it is nil.
func replyParentValue(value *string) string {
	if value == nil {
		return "null"
	}
	return *value
}

// c-reply-parent, its first test (R8 of the issue-tree spec): a reply hangs under what it
// answers, its parent coalesce(answers, reply_to, ask_id). In a thread of a head, a reply to the
// head and a reply to that reply, the write flattens reply_to onto the head and keeps the comment
// the request named in answers. The tree read hangs the reply to the head under the head and the
// reply to a reply under the comment it answers, and the references read answers that comment as
// its parent.
func TestAReplyToAReplyHangsUnderTheCommentItAnswers(t *testing.T) {
	t.Parallel()
	handler, _ := newTestHandlerWithStore(t)
	issue := createInteractionIssue(t, handler, "TEST", "A reply to a reply", "A spec")
	head := createThreadComment(t, handler, issue.Key, map[string]any{"body": "The head."}, "alice")
	middle := createThreadComment(t, handler, issue.Key, map[string]any{
		"body": "On the head.", "reply_to": head.ID,
	}, "bob")
	leaf := createThreadComment(t, handler, issue.Key, map[string]any{
		"body": "On the reply.", "reply_to": middle.ID,
	}, "alice")
	requireReplyTo(t, "the reply to a reply", leaf.ReplyTo, head.ID)
	requireAnswers(t, "the reply to a reply", leaf.Answers, &middle.ID)

	headNode := replyParentNode(graphstore.KindComment, head.ID)
	answered := replyParentNode(graphstore.KindComment, middle.ID)
	if got := replyParentInTree(t, handler, issue.Key, graphstore.KindComment, middle.ID); got != headNode {
		t.Errorf("the tree read hangs the reply to the head %s under %q, want the head %s", middle.ID, got, headNode)
	}
	if got := replyParentInTree(t, handler, issue.Key, graphstore.KindComment, leaf.ID); got != answered {
		t.Errorf("the tree read hangs the reply to a reply %s under %q, want the comment it answers %s, not its thread's head %s",
			leaf.ID, got, answered, headNode)
	}
	leafRef := "dispatch://" + issue.Key + "/comment/" + leaf.ID
	if got := replyParentInReferences(t, handler, leafRef); got != answered {
		t.Errorf("the references read answers the reply to a reply %s with the parent %q, want the comment it answers %s, not its thread's head %s",
			leaf.ID, got, answered, headNode)
	}
}

// c-reply-parent, its second test (R8): a reply that names a block of the comment it answers hangs
// under that comment, and the block stays its replies_to_block pointer. The tree read and the
// references read answer the comment as the reply's parent; the reply's row, and its read through
// GET /api/v1/comments/{id}, still name the block.
func TestAReplyThatNamesABlockHangsUnderItsComment(t *testing.T) {
	t.Parallel()
	handler, database := newTestHandlerWithStore(t)
	issue := createInteractionIssue(t, handler, "TEST", "A reply that names a block", "A spec")
	head := createThreadComment(t, handler, issue.Key, map[string]any{"body": "The head."}, "alice")
	middle := createThreadComment(t, handler, issue.Key, map[string]any{
		"body": "Alpha.\n\nBeta.", "reply_to": head.ID,
	}, "bob")
	beta := &model.ReplyBlock{
		Node:    "comment/" + middle.ID + "#b2",
		Ref:     "dispatch://" + issue.Key + "/comment/" + middle.ID + "#b-b2",
		BlockID: "b2",
		Text:    "Beta.",
	}
	onBlock := createThreadComment(t, handler, issue.Key, map[string]any{
		"body": "On beta.", "reply_to": middle.ID, "reply_to_block": "b2",
	}, "alice")
	requireAnswers(t, "the reply that names a block", onBlock.Answers, &middle.ID)
	requireReplyBlock(t, "the reply that names a block", onBlock.ReplyBlock, beta)

	answered := replyParentNode(graphstore.KindComment, middle.ID)
	if got := replyParentInTree(t, handler, issue.Key, graphstore.KindComment, onBlock.ID); got != answered {
		t.Errorf("the tree read hangs the reply %s that names the block %s under %q, want the comment it answers %s",
			onBlock.ID, beta.Node, got, answered)
	}
	onBlockRef := "dispatch://" + issue.Key + "/comment/" + onBlock.ID
	if got := replyParentInReferences(t, handler, onBlockRef); got != answered {
		t.Errorf("the references read answers the reply %s that names the block %s with the parent %q, want the comment it answers %s",
			onBlock.ID, beta.Node, got, answered)
	}
	if stored := replyParentStoredBlock(t, database, "comments", onBlock.ID); stored != beta.Node {
		t.Errorf("the reply %s stores replies_to_block %q, want the block it names %s still standing", onBlock.ID, stored, beta.Node)
	}
	requireReplyBlock(t, "GET the reply that names a block", readCommentThread(t, handler, onBlock.ID).Comment.ReplyBlock, beta)
}

// c-reply-parent, its third test (R8): a reply to a part of an ask names that part in
// replies_to_block and no comment in answers, so it hangs under the ask. The tree read and the
// references read answer the ask as its parent, and the part stays its replies_to_block pointer.
func TestAReplyToAPartOfAnAskHangsUnderTheAsk(t *testing.T) {
	t.Parallel()
	handler, database := newTestHandlerWithStore(t)
	issue := createInteractionIssue(t, handler, "TEST", "A reply to a part of an ask", "A spec")
	ask := createPartAsk(t, handler, issue.Key, "Which day do we ship?",
		map[string]string{"label": "Monday", "description": "Ship at the start of the week."},
		map[string]string{"label": "Tuesday"})
	part := &model.ReplyBlock{
		Node: "ask/" + ask.ID + "#b2", Ref: askPartRef(issue.Key, ask.ID, "b2"), BlockID: "b2",
		Text: "Monday — Ship at the start of the week.",
	}
	reply := createThreadComment(t, handler, issue.Key, map[string]any{
		"body": "Monday works for us.", "ask_id": ask.ID, "reply_to_block": "b2",
	}, "alice")
	requireAnswers(t, "the reply to a part of the ask", reply.Answers, nil)
	if reply.ReplyTo != nil {
		t.Fatalf("the reply to a part of the ask has reply_to %s, want none", *reply.ReplyTo)
	}
	requireReplyBlock(t, "the reply to a part of the ask", reply.ReplyBlock, part)

	asked := replyParentNode(graphstore.KindAsk, ask.ID)
	if got := replyParentInTree(t, handler, issue.Key, graphstore.KindComment, reply.ID); got != asked {
		t.Errorf("the tree read hangs the reply %s to the part %s under %q, want the ask %s", reply.ID, part.Node, got, asked)
	}
	replyRef := "dispatch://" + issue.Key + "/comment/" + reply.ID
	if got := replyParentInReferences(t, handler, replyRef); got != asked {
		t.Errorf("the references read answers the reply %s to the part %s with the parent %q, want the ask %s", reply.ID, part.Node, got, asked)
	}
	if stored := replyParentStoredBlock(t, database, "comments", reply.ID); stored != part.Node {
		t.Errorf("the reply %s stores replies_to_block %q, want the part it names %s still standing", reply.ID, stored, part.Node)
	}
}

// c-reply-parent, its fourth test (R8): a message's parent is coalesce(in_reply_to,
// replies_to_block). A message reply that names a block of the message it answers hangs under that
// message in the tree read and in the references read, and the block stays its replies_to_block
// pointer.
func TestAMessageReplyThatNamesABlockHangsUnderItsMessage(t *testing.T) {
	t.Parallel()
	handler, database := newTestHandlerWithStore(t)
	issue := createInteractionIssue(t, handler, "TEST", "A message reply that names a block", "A spec")
	root := createIssueMessage(t, handler, issue.Key, map[string]any{"body": "One.\n\nTwo."}, "alice")
	two := &model.ReplyBlock{
		Node:    "message/" + root.ID + "#b2",
		Ref:     "dispatch://" + issue.Key + "/message/" + root.ID + "#b-b2",
		BlockID: "b2",
		Text:    "Two.",
	}
	reply := createIssueMessage(t, handler, issue.Key, map[string]any{
		"body": "On two.", "in_reply_to": root.ID, "in_reply_to_block": "b2",
	}, "bob")
	requireReplyBlock(t, "the message reply that names a block", reply.ReplyBlock, two)

	answered := replyParentNode(graphstore.KindMessage, root.ID)
	if got := replyParentInTree(t, handler, issue.Key, graphstore.KindMessage, reply.ID); got != answered {
		t.Errorf("the tree read hangs the message reply %s that names the block %s under %q, want the message it answers %s",
			reply.ID, two.Node, got, answered)
	}
	replyRef := "dispatch://" + issue.Key + "/message/" + reply.ID
	if got := replyParentInReferences(t, handler, replyRef); got != answered {
		t.Errorf("the references read answers the message reply %s that names the block %s with the parent %q, want the message it answers %s",
			reply.ID, two.Node, got, answered)
	}
	if stored := replyParentStoredBlock(t, database, "messages", reply.ID); stored != two.Node {
		t.Errorf("the message reply %s stores replies_to_block %q, want the block it names %s still standing", reply.ID, stored, two.Node)
	}
}

// c-reply-parent, its fifth test (R8, R31): a comment that answers nothing keeps its place. A
// person's comment anchored in the paragraph of a decision of the spec, on an issue born converted
// and on one that has not converted, answers no comment and no ask, and the tree read hangs it
// under that decision: the read returns no prose block of a role document, so the comment stands
// under the nearest ancestor of its anchor's block that the read returns.
func TestAnAnchoredCommentStillHangsUnderItsTypedBlock(t *testing.T) {
	handler, database, _ := newTestServer(t, testServerOptions{})
	createReferenceAPIProject(t, handler, "CORE")
	const words = "We ship the urgent list first."
	for _, test := range []struct {
		name        string
		unconverted bool
	}{
		{"an issue born converted", false},
		{"an issue that has not converted", true},
	} {
		t.Run(test.name, func(t *testing.T) {
			issue := createDigestIssue(t, handler, database, "CORE", "A decision with a remark on it, "+test.name,
				"An opening paragraph.\n\n:::decision{#d1}\n"+words+"\n:::\n", test.unconverted)
			spec := readDecisionDocument(t, database, issue.Key, "spec")
			var paragraph string
			for text, id := range blockIDsByText(t, handler, issue.PrimaryArtifactID) {
				if strings.TrimSpace(text) == words {
					paragraph = id
				}
			}
			if paragraph == "" {
				t.Fatalf("the spec of %s lists no block of the decision's paragraph", issue.Key)
			}
			artifact := issue.PrimaryArtifactID
			if test.unconverted {
				artifact = "spec"
			}
			comment := createBlockAnchoredComment(t, handler, issue.Key, map[string]any{
				"artifact": artifact, "quote": "urgent list", "block_id": paragraph,
			})
			if comment.ReplyTo != nil || comment.AskID != nil || comment.Answers != nil {
				t.Fatalf("the anchored comment %s answers reply_to %s, ask_id %s and answers %s, want it to answer nothing",
					comment.ID, replyParentValue(comment.ReplyTo), replyParentValue(comment.AskID), replyParentValue(comment.Answers))
			}

			decision := replyParentNode(refs.BlockNodeKind, spec.address("d1"))
			if got := replyParentInTree(t, handler, issue.Key, graphstore.KindComment, comment.ID); got != decision {
				t.Errorf("the tree read of %s hangs the comment %s, which answers nothing and is anchored inside the decision %s, under %q, want that decision",
					issue.Key, comment.ID, decision, got)
			}
		})
	}
}
