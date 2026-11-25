package api

import "testing"

// A delete event carries no text of another post (dispatch://INFRA-1523, the architect's ruling
// of 2026-11-19 22:21Z), and a block reply's delete event still says which block of its parent
// the reply names: message.deleted and comment.deleted of such a reply carry reply_block with its
// node and its id and an empty text, beside the fields that route the reply, and message.deleted
// carries no reply_body. The dashboard refreshes the read of the block that lists the reply from
// that node (event-refresh.ts's appendRepliedBlock), so an event without it leaves that read
// stale. Each routing field is read from the reply's creation first, so each check can fail.
func TestABlockReplysDeleteEventNamesTheBlockItAnswersAndNoTextOfIt(t *testing.T) {
	t.Parallel()
	handler := newTestHandler(t)
	issue := createInteractionIssue(t, handler, "TEST", "A block reply's delete event", "A spec")
	parent := createIssueMessage(t, handler, issue.Key, map[string]any{"body": "The standing parent.\n\nIts second paragraph."}, "alice")
	message := createIssueMessage(t, handler, issue.Key, map[string]any{
		"body": "a block reply to withdraw", "in_reply_to": parent.ID, "in_reply_to_block": "b1",
	}, "bob")
	root := createThreadComment(t, handler, issue.Key, map[string]any{"body": "The standing comment."}, "alice")
	comment := createThreadComment(t, handler, issue.Key, map[string]any{
		"body": "a comment reply to withdraw", "reply_to": root.ID, "reply_to_block": "b1",
	}, "bob")

	deleteIssueMessage(t, handler, message.ID, "bob")
	deleteThreadComment(t, handler, comment.ID, "bob")

	events := decodedIssueEvents(t, handler, issue.Key)
	for _, reply := range []struct {
		// created is the event of the reply's creation: message.answered for a reply message,
		// comment.created for a reply comment. routing is each field that routes the reply,
		// which that event carries.
		id, created, deleted string
		routing              []string
	}{
		{id: message.ID, created: "message.answered", deleted: "message.deleted", routing: []string{"in_reply_to"}},
		{id: comment.ID, created: "comment.created", deleted: "comment.deleted", routing: []string{"reply_to", "thread_root_id"}},
	} {
		var created, deleted map[string]any
		for _, event := range events {
			if event.Payload["id"] != reply.id {
				continue
			}
			switch event.Type {
			case reply.created:
				created = event.Payload
			case reply.deleted:
				deleted = event.Payload
			}
		}
		if created == nil || deleted == nil {
			t.Fatalf("the log holds %s %v and %s %v of reply %s, want both", reply.created, created, reply.deleted, deleted, reply.id)
		}
		block, _ := deleted["reply_block"].(map[string]any)
		if block == nil {
			t.Fatalf("the %s of reply %s carries reply_block %v, want the block it names", reply.deleted, reply.id, deleted["reply_block"])
		}
		if node, _ := block["node"].(string); node == "" {
			t.Fatalf("the %s of reply %s carries reply_block.node %v, want the node of the block it names", reply.deleted, reply.id, block["node"])
		}
		if block["block_id"] != "b1" {
			t.Fatalf("the %s of reply %s carries reply_block.block_id %v, want b1", reply.deleted, reply.id, block["block_id"])
		}
		if text, present := block["text"]; !present || text != "" {
			t.Fatalf("the %s of reply %s carries reply_block.text %v (present %t), want \"\"", reply.deleted, reply.id, text, present)
		}
		for _, field := range reply.routing {
			if value, _ := created[field].(string); value == "" {
				t.Fatalf("the %s of reply %s carries %s %v, so its delete event proves nothing", reply.created, reply.id, field, created[field])
			}
			if deleted[field] != created[field] {
				t.Fatalf("the %s of reply %s carries %s %v, want %v as its creation carried it", reply.deleted, reply.id, field, deleted[field], created[field])
			}
		}
		if reply.deleted == "message.deleted" {
			if preview, _ := deleted["reply_body"].(string); preview != "" {
				t.Fatalf("the message.deleted of reply %s carries reply_body %q, want none", reply.id, preview)
			}
		}
	}
}
