package api

// What the four events of a correction carry, and what a read of an event of an edited or a
// deleted post gives: the post's own marks, the thread a comment's correction is routed by, no
// part of a deleted post's body, and the frozen preview of another post untouched.

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// decodedIssueEvents is the issue's events, oldest first, each payload decoded into a map so a
// test can say which keys the read gives and which it does not.
func decodedIssueEvents(t *testing.T, handler http.Handler, issueKey string) []struct {
	Type    string
	Payload map[string]any
} {
	t.Helper()
	read := []struct {
		Type    string
		Payload map[string]any
	}{}
	for _, event := range issueEvents(t, handler, issueKey) {
		payload := map[string]any{}
		if err := json.Unmarshal(event.Payload, &payload); err != nil {
			t.Fatalf("decode %s payload %s: %v", event.Type, event.Payload, err)
		}
		read = append(read, struct {
			Type    string
			Payload map[string]any
		}{Type: event.Type, Payload: payload})
	}
	return read
}

// onePayloadOfType is the one payload of eventType in the issue's log, failing t on any other
// count.
func onePayloadOfType(t *testing.T, handler http.Handler, issueKey, eventType string) map[string]any {
	t.Helper()
	var found []map[string]any
	for _, event := range decodedIssueEvents(t, handler, issueKey) {
		if event.Type == eventType {
			found = append(found, event.Payload)
		}
	}
	if len(found) != 1 {
		t.Fatalf("the issue holds %d %s events, want the one the write appended", len(found), eventType)
	}
	return found[0]
}

// An edit and a delete of each kind append their own event, in the order they were written. An
// edit carries the new body; a delete carries no body and the fields the outbox reads to compute
// its destinations, beside the mark.
func TestTheFourCorrectionEventsCarryTheirBodiesAndTheirMarks(t *testing.T) {
	t.Parallel()
	handler := newTestHandler(t)
	issue := createInteractionIssue(t, handler, "TEST", "The correction events", "A spec")
	comment := createThreadComment(t, handler, issue.Key, map[string]any{"body": "the first comment body"}, "alice")
	message := createIssueMessage(t, handler, issue.Key, map[string]any{"body": "the first message body"}, "alice")

	edited := dispatchRequest(t, handler, http.MethodPatch, "/api/v1/comments/"+comment.ID,
		map[string]any{"body": "the comment body that replaces it"}, "alice")
	if edited.Code != http.StatusOK {
		t.Fatalf("edit the comment: status=%d body=%s", edited.Code, edited.Body.String())
	}
	// Each edit's payload is read while its post stands: once the post is deleted no read of its
	// events gives a body at all, which is what the two tests below prove.
	if payload := onePayloadOfType(t, handler, issue.Key, "comment.edited"); payload["body"] != "the comment body that replaces it" {
		t.Fatalf("the comment.edited payload body = %v, want the new body", payload["body"])
	}
	deleteThreadComment(t, handler, comment.ID, "alice")
	editIssueMessage(t, handler, message.ID, "the message body that replaces it", "alice")
	if payload := onePayloadOfType(t, handler, issue.Key, "message.edited"); payload["body"] != "the message body that replaces it" {
		t.Fatalf("the message.edited payload body = %v, want the new body", payload["body"])
	}
	deleteIssueMessage(t, handler, message.ID, "alice")

	var order []string
	corrections := map[string]bool{
		"comment.edited": true, "comment.deleted": true, "message.edited": true, "message.deleted": true,
	}
	for _, event := range decodedIssueEvents(t, handler, issue.Key) {
		if corrections[event.Type] {
			order = append(order, event.Type)
		}
	}
	want := []string{"comment.edited", "comment.deleted", "message.edited", "message.deleted"}
	if !slices.Equal(order, want) {
		t.Fatalf("the issue's correction events = %v, want %v in the order they were written", order, want)
	}

	// A delete carries the fields of the post's own create event except every body: they are
	// what the outbox reads to compute its destinations. These posts are short and the comment
	// has no suggestion, so no event of either ever carried body_truncated or a suggestion, and
	// the body is all there is to check here. The two tests below check body_truncated on a
	// deleted post's events, and a suggestion on a deleted comment's, with posts whose events
	// carried them while they stood.
	commentDeleted := onePayloadOfType(t, handler, issue.Key, "comment.deleted")
	requireNoBody(t, "comment.deleted", commentDeleted)
	requirePayloadKeys(t, "comment.deleted", commentDeleted, "id", "issue_key", "author", "reply_to", "ask_id")
	if commentDeleted["id"] != comment.ID {
		t.Fatalf("the comment.deleted payload names %v, want the comment %s", commentDeleted["id"], comment.ID)
	}
	requireDeletedMark(t, "comment.deleted", commentDeleted, "alice")

	messageDeleted := onePayloadOfType(t, handler, issue.Key, "message.deleted")
	requireNoBody(t, "message.deleted", messageDeleted)
	requirePayloadKeys(t, "message.deleted", messageDeleted, "id", "issue_key", "author", "in_reply_to", "target")
	if messageDeleted["id"] != message.ID {
		t.Fatalf("the message.deleted payload names %v, want the message %s", messageDeleted["id"], message.ID)
	}
	requireDeletedMark(t, "message.deleted", messageDeleted, "alice")
}

// requireNoBody fails t when the payload gives the post's body, or carries any of the keys named
// in more, each of which gives part of the body too: body_truncated says the body was cut short,
// and a comment's suggestion holds replacement text its author wrote. A caller names only the
// keys its post's own events carried while the post stood: a key no event of the post ever
// carried is absent whatever the read does, so a check of it could never fail.
func requireNoBody(t *testing.T, eventType string, payload map[string]any, more ...string) {
	t.Helper()
	if body, present := payload["body"]; present && body != "" {
		t.Fatalf("the %s payload body = %v, want no body at all", eventType, body)
	}
	for _, key := range more {
		if value, present := payload[key]; present && value != nil && value != false {
			t.Fatalf("the %s payload carries %s = %v, which gives part of the body it must not carry", eventType, key, value)
		}
	}
}

// requirePayloadKeys fails t when the payload leaves out a field the outbox reads.
func requirePayloadKeys(t *testing.T, eventType string, payload map[string]any, keys ...string) {
	t.Helper()
	for _, key := range keys {
		if _, present := payload[key]; !present {
			t.Fatalf("the %s payload = %v, want the routing field %s", eventType, payload, key)
		}
	}
}

// requireDeletedMark fails t unless the payload says that the post is deleted, and by whom.
func requireDeletedMark(t *testing.T, eventType string, payload map[string]any, by string) {
	t.Helper()
	if at, _ := payload["deleted_at"].(string); at == "" {
		t.Fatalf("the %s payload deleted_at = %v, want the time of the delete", eventType, payload["deleted_at"])
	}
	actor, _ := payload["deleted_by"].(map[string]any)
	if actor == nil || actor["id"] != by {
		t.Fatalf("the %s payload deleted_by = %v, want the actor %s", eventType, payload["deleted_by"], by)
	}
}

// storedThreadRootID is the thread_root_id the newest stored eventType event about the record id
// carries, and whether its payload carries the field at all.
func storedThreadRootID(t *testing.T, database *store.Store, eventType, id string) (string, bool) {
	t.Helper()
	var root *string
	if err := database.Pool.QueryRow(context.Background(), `
		select payload->>'thread_root_id' from events where type = $1 and payload->>'id' = $2 order by id desc limit 1
	`, eventType, id).Scan(&root); err != nil {
		t.Fatalf("read the %s event of %s: %v", eventType, id, err)
	}
	if root == nil {
		return "", false
	}
	return *root, true
}

// An edit and a delete of a comment are routed as its creation was (commentCorrectionPayload): a
// reply's comment.edited and comment.deleted carry its reply_to as thread_root_id, as its
// comment.created did, and the thread_logins on its thread as the correction is written (here the
// same as its creation's, since nobody joined between), so an Inbox reads its Replies rows again for the
// logins with a row on the thread and no other. A root comment heads its own thread, so its
// corrections carry no thread_root_id, and they name the logins with a row on that thread when
// the correction is written, bob's reply among them: his row draws the root.
func TestACommentsCorrectionsCarryItsThreadRootAndTheLoginsOnItsThread(t *testing.T) {
	t.Parallel()
	handler, database := newTestHandlerWithStore(t)
	issue := createInteractionIssue(t, handler, "TEST", "A corrected thread", "A spec")
	root := createThreadComment(t, handler, issue.Key, map[string]any{"body": "Who reviews this?"}, "alice")
	reply := createThreadComment(t, handler, issue.Key, map[string]any{"body": "I will.", "reply_to": root.ID}, "bob")
	created, carried := threadLoginsOf(t, database, "comment.created", reply.ID)
	if !carried || !slices.Equal(created, []string{"alice", "bob"}) {
		t.Fatalf("the reply's comment.created thread_logins = %#v (carried %v), want alice and bob", created, carried)
	}
	if rootID, _ := storedThreadRootID(t, database, "comment.created", reply.ID); rootID != root.ID {
		t.Fatalf("the reply's comment.created thread_root_id = %q, want its reply_to %s", rootID, root.ID)
	}

	editThreadComment(t, handler, reply.ID, "I will, on Friday.", "bob")
	deleteThreadComment(t, handler, reply.ID, "bob")
	editThreadComment(t, handler, root.ID, "Who reviews this today?", "alice")
	deleteThreadComment(t, handler, root.ID, "alice")

	for _, eventType := range []string{"comment.edited", "comment.deleted"} {
		if rootID, carried := storedThreadRootID(t, database, eventType, reply.ID); rootID != root.ID {
			t.Fatalf("the reply's %s thread_root_id = %q (carried %v), want its reply_to %s, as its comment.created carried", eventType, rootID, carried, root.ID)
		}
		requireThreadLogins(t, database, eventType, reply.ID, created...)
		if rootID, carried := storedThreadRootID(t, database, eventType, root.ID); carried {
			t.Fatalf("the root comment's %s thread_root_id = %q, want none: a root heads its own thread", eventType, rootID)
		}
		requireThreadLogins(t, database, eventType, root.ID, "alice", "bob")
	}
}

// Every event of a deleted comment reads with the mark and no part of its body, however many
// events it has. The one copy of the text that stays is the reply's own frozen preview of the
// block it answers, which is part of the event of the reply and not of the deleted comment.
//
// The comment is a suggestion whose body is too long for one event, so while it stands its
// events carry each thing requireNoBody looks for: a body, the body_truncated a cut body earns,
// and the suggestion. The test reads that before the delete, so each of those checks can fail.
func TestEveryEventOfADeletedCommentGivesNoBodyAndKeepsTheReplysFrozenPreview(t *testing.T) {
	t.Parallel()
	handler := newTestHandler(t)
	issue := createInteractionIssue(t, handler, "TEST", "A deleted comment's events", "A spec")
	const phrase = "the withdrawn claim no reader may meet again"
	comment := createThreadComment(t, handler, issue.Key, map[string]any{
		"body":       phrase + "\n\n" + paragraphBody(3, 15_000),
		"anchor":     map[string]any{"artifact": "spec", "quote": "A spec"},
		"suggestion": map[string]string{"replace_with": "the withdrawn replacement"},
	}, "alice")
	// A second event of the same comment, so the read is proven over more than its creation.
	resolved := dispatchRequest(t, handler, http.MethodPost, "/api/v1/comments/"+comment.ID+"/resolve", nil, "alice")
	if resolved.Code != http.StatusOK {
		t.Fatalf("resolve the comment: status=%d body=%s", resolved.Code, resolved.Body.String())
	}
	reply := createThreadComment(t, handler, issue.Key, map[string]any{
		"body": "the reply that quotes it", "reply_to": comment.ID, "reply_to_block": "b1",
	}, "bob")
	if reply.ReplyBlock == nil || reply.ReplyBlock.Text != phrase {
		t.Fatalf("the reply's block preview = %#v, want the deleted comment's own text frozen into it", reply.ReplyBlock)
	}
	carried := map[string]bool{}
	for _, event := range decodedIssueEvents(t, handler, issue.Key) {
		if event.Payload["id"] != comment.ID {
			continue
		}
		if body, _ := event.Payload["body"].(string); body != "" {
			carried["body"] = true
		}
		if event.Payload["body_truncated"] == true {
			carried["body_truncated"] = true
		}
		if event.Payload["suggestion"] != nil {
			carried["suggestion"] = true
		}
	}
	if !carried["body"] || !carried["body_truncated"] || !carried["suggestion"] {
		t.Fatalf("the standing comment's events carry %v, want a body, body_truncated and a suggestion", carried)
	}

	deleteThreadComment(t, handler, comment.ID, "alice")

	frozen := 0
	for _, event := range decodedIssueEvents(t, handler, issue.Key) {
		if event.Payload["id"] == comment.ID {
			requireNoBody(t, event.Type, event.Payload, "body_truncated", "suggestion")
			requireDeletedMark(t, event.Type, event.Payload, "alice")
			continue
		}
		// The reply's own creation keeps the preview the write froze into it: it is part of the
		// event of the reply, which the delete of another post does not rewrite.
		block, _ := event.Payload["reply_block"].(map[string]any)
		if block != nil && block["text"] == phrase {
			frozen++
		}
	}
	if frozen != 1 {
		t.Fatalf("the reply's frozen block preview is in %d events, want the one comment.created that froze it", frozen)
	}
	// Nowhere else does the log give the text: one copy, in that one payload.
	listed := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+issue.Key+"/events?limit=200", nil, "alice")
	if count := strings.Count(listed.Body.String(), phrase); count != 1 {
		t.Fatalf("the issue's events hold the deleted text %d times, want the reply's one frozen preview", count)
	}
}

// The message twin: every event of a deleted message reads with the mark and no body, and the
// reply_body a reply's own event froze stays.
//
// The message's body is too long for one event, so while it stands its creation carries a body
// and body_truncated. A message has no suggestion, so those two are every key of its events that
// gives the body. The test reads both before the delete, so each check after it can fail.
func TestEveryEventOfADeletedMessageGivesNoBodyAndKeepsTheReplysFrozenPreview(t *testing.T) {
	t.Parallel()
	handler := newTestHandler(t)
	issue := createInteractionIssue(t, handler, "TEST", "A deleted message's events", "A spec")
	const phrase = "the withdrawn message no reader may meet again"
	message := createIssueMessage(t, handler, issue.Key, map[string]any{"body": phrase + "\n\n" + paragraphBody(3, 15_000)}, "alice")
	editIssueMessage(t, handler, message.ID, phrase+", corrected once", "alice")
	reply := createIssueMessage(t, handler, issue.Key, map[string]any{
		"body": "the reply that quotes it", "in_reply_to": message.ID,
	}, "bob")
	carried := map[string]bool{}
	for _, event := range decodedIssueEvents(t, handler, issue.Key) {
		if event.Payload["id"] != message.ID {
			continue
		}
		if body, _ := event.Payload["body"].(string); body != "" {
			carried["body"] = true
		}
		if event.Payload["body_truncated"] == true {
			carried["body_truncated"] = true
		}
	}
	if !carried["body"] || !carried["body_truncated"] {
		t.Fatalf("the standing message's events carry %v, want a body and body_truncated", carried)
	}

	deleteIssueMessage(t, handler, message.ID, "alice")

	frozen := 0
	for _, event := range decodedIssueEvents(t, handler, issue.Key) {
		if event.Payload["id"] == message.ID {
			requireNoBody(t, event.Type, event.Payload, "body_truncated")
			requireDeletedMark(t, event.Type, event.Payload, "alice")
			continue
		}
		if event.Payload["id"] == reply.ID {
			if preview, _ := event.Payload["reply_body"].(string); strings.Contains(preview, phrase) {
				frozen++
			}
		}
	}
	if frozen != 1 {
		t.Fatalf("the reply's frozen preview of the deleted message is in %d of the reply's events, want the one that froze it", frozen)
	}
}

// A delete event carries no body, whoever's body it is (dispatch://INFRA-1523, the architect's
// decision of 2026-11-16, and chk-events: "the payload of each delete event carries no body"). A
// reply's own creation froze a preview of the post it answers, which the event log keeps. Its
// delete is a new event, written after the fact, so it writes no new copy of that other post's
// text: comment.deleted and message.deleted of a reply carry no reply_body and no reply_block
// text, for a plain reply and for one naming a block, with the parent still standing; a block
// reply's reply_block keeps the fields that name the block and route the event. The control is
// the reply's creation, which keeps the preview.
func TestAReplysDeleteEventCarriesNoTextOfThePostItAnswers(t *testing.T) {
	t.Parallel()
	handler := newTestHandler(t)
	issue := createInteractionIssue(t, handler, "TEST", "A reply's delete event", "A spec")
	const phrase = "the standing parent whose words its replies quote"
	parent := createIssueMessage(t, handler, issue.Key, map[string]any{"body": phrase + "\n\nIts second paragraph."}, "alice")
	plain := createIssueMessage(t, handler, issue.Key, map[string]any{
		"body": "a plain reply to withdraw", "in_reply_to": parent.ID,
	}, "bob")
	block := createIssueMessage(t, handler, issue.Key, map[string]any{
		"body": "a block reply to withdraw", "in_reply_to": parent.ID, "in_reply_to_block": "b1",
	}, "bob")
	comment := createThreadComment(t, handler, issue.Key, map[string]any{"body": phrase}, "alice")
	commentReply := createThreadComment(t, handler, issue.Key, map[string]any{
		"body": "a comment reply to withdraw", "reply_to": comment.ID, "reply_to_block": "b1",
	}, "bob")
	quotes := func(payload map[string]any) bool {
		preview, _ := payload["reply_body"].(string)
		blockText := ""
		if quoted, _ := payload["reply_block"].(map[string]any); quoted != nil {
			blockText, _ = quoted["text"].(string)
		}
		return strings.Contains(preview, phrase) || strings.Contains(blockText, phrase)
	}
	replies := []string{plain.ID, block.ID, commentReply.ID}
	for _, id := range replies {
		froze := false
		for _, event := range decodedIssueEvents(t, handler, issue.Key) {
			if event.Payload["id"] == id && quotes(event.Payload) {
				froze = true
			}
		}
		if !froze {
			t.Fatalf("no event of reply %s quotes its parent before the delete, so its delete event proves nothing", id)
		}
	}

	deleteIssueMessage(t, handler, plain.ID, "bob")
	deleteIssueMessage(t, handler, block.ID, "bob")
	deleteThreadComment(t, handler, commentReply.ID, "bob")

	for _, event := range decodedIssueEvents(t, handler, issue.Key) {
		if id, _ := event.Payload["id"].(string); !slices.Contains(replies, id) {
			continue
		}
		if event.Type != "message.deleted" && event.Type != "comment.deleted" {
			continue
		}
		if quotes(event.Payload) {
			t.Fatalf("the %s of reply %s carries reply_body %q and reply_block %v, its parent's text: a delete event writes no copy of another post's text",
				event.Type, event.Payload["id"], event.Payload["reply_body"], event.Payload["reply_block"])
		}
	}
}

// Every event of an edited post reads with the time its author last replaced the body, as the
// post itself does, its own edit events among them: the event of its creation keeps the body as
// first written, and the mark is what tells a reader of the log that the body was replaced
// since. An event of a post nobody edited reads with no edit time.
func TestEveryEventOfAnEditedPostCarriesTheTimeOfItsLastEdit(t *testing.T) {
	t.Parallel()
	handler := newTestHandler(t)
	issue := createInteractionIssue(t, handler, "TEST", "An edited post's events", "A spec")
	comment := createThreadComment(t, handler, issue.Key, map[string]any{"body": "the comment as first written"}, "alice")
	message := createIssueMessage(t, handler, issue.Key, map[string]any{"body": "the message as first written"}, "alice")
	standing := createIssueMessage(t, handler, issue.Key, map[string]any{"body": "the message nobody edits"}, "alice")

	commentEdited := editThreadComment(t, handler, comment.ID, "the comment as corrected", "alice")
	editIssueMessage(t, handler, message.ID, "the message as corrected once", "alice")
	messageEdited := editIssueMessage(t, handler, message.ID, "the message as corrected twice", "alice").EditedAt
	if messageEdited == nil {
		t.Fatal("the second edit of the message answered no edited_at")
	}

	want := map[string]struct {
		at      *time.Time
		created string
	}{
		comment.ID:  {at: &commentEdited, created: "the comment as first written"},
		message.ID:  {at: messageEdited, created: "the message as first written"},
		standing.ID: {at: nil, created: "the message nobody edits"},
	}
	read := map[string]int{}
	for _, event := range decodedIssueEvents(t, handler, issue.Key) {
		id, _ := event.Payload["id"].(string)
		post, found := want[id]
		if !found {
			continue
		}
		read[id]++
		at, _ := event.Payload["edited_at"].(string)
		if post.at == nil {
			if at != "" {
				t.Fatalf("the %s payload of a post nobody edited carries edited_at %q", event.Type, at)
			}
		} else if parsed, err := time.Parse(time.RFC3339Nano, at); err != nil || !parsed.Equal(*post.at) {
			t.Fatalf("the %s payload of %s edited_at = %v, want the post's last edit at %s", event.Type, id, event.Payload["edited_at"], post.at)
		}
		// The read marks the event and leaves the body it recorded.
		if strings.HasSuffix(event.Type, ".created") && event.Payload["body"] != post.created {
			t.Fatalf("the %s payload of %s body = %v, want the body as first written", event.Type, id, event.Payload["body"])
		}
	}
	// The comment's creation and its edit, the message's creation and its two edits, and the
	// standing message's creation.
	if read[comment.ID] != 2 || read[message.ID] != 3 || read[standing.ID] != 1 {
		t.Fatalf("the issue's events of the three posts = %v, want 2, 3 and 1", read)
	}
}
