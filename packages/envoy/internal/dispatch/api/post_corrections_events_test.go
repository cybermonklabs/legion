package api

// What the four events of a correction carry, and what a read of an event of a deleted post
// gives: the mark, no part of the body, and the frozen preview of another post untouched.

import (
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"
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
	// what the outbox reads to compute its destinations.
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

// requireNoBody fails t when the payload gives any part of the post's body.
func requireNoBody(t *testing.T, eventType string, payload map[string]any) {
	t.Helper()
	if body, present := payload["body"]; present && body != "" {
		t.Fatalf("the %s payload body = %v, want no body at all", eventType, body)
	}
	if truncated, present := payload["body_truncated"]; present && truncated != false {
		t.Fatalf("the %s payload says body_truncated = %v, which names a body it must not carry", eventType, truncated)
	}
	if suggestion := payload["suggestion"]; suggestion != nil {
		t.Fatalf("the %s payload carries the suggestion %v, which is the comment's text too", eventType, suggestion)
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

// Every event of a deleted comment reads with the mark and no part of its body, however many
// events it has. The one copy of the text that stays is the reply's own frozen preview of the
// block it answers, which is part of the event of the reply and not of the deleted comment.
func TestEveryEventOfADeletedCommentGivesNoBodyAndKeepsTheReplysFrozenPreview(t *testing.T) {
	handler := newTestHandler(t)
	issue := createInteractionIssue(t, handler, "TEST", "A deleted comment's events", "A spec")
	const phrase = "the withdrawn claim no reader may meet again"
	comment := createThreadComment(t, handler, issue.Key, map[string]any{
		"body": phrase, "anchor": map[string]any{"artifact": "spec", "quote": "A spec"},
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

	deleteThreadComment(t, handler, comment.ID, "alice")

	frozen := 0
	for _, event := range decodedIssueEvents(t, handler, issue.Key) {
		if event.Payload["id"] == comment.ID {
			requireNoBody(t, event.Type, event.Payload)
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
func TestEveryEventOfADeletedMessageGivesNoBodyAndKeepsTheReplysFrozenPreview(t *testing.T) {
	handler := newTestHandler(t)
	issue := createInteractionIssue(t, handler, "TEST", "A deleted message's events", "A spec")
	const phrase = "the withdrawn message no reader may meet again"
	message := createIssueMessage(t, handler, issue.Key, map[string]any{"body": phrase}, "alice")
	editIssueMessage(t, handler, message.ID, phrase+", corrected once", "alice")
	reply := createIssueMessage(t, handler, issue.Key, map[string]any{
		"body": "the reply that quotes it", "in_reply_to": message.ID,
	}, "bob")

	deleteIssueMessage(t, handler, message.ID, "alice")

	frozen := 0
	for _, event := range decodedIssueEvents(t, handler, issue.Key) {
		if event.Payload["id"] == message.ID {
			requireNoBody(t, event.Type, event.Payload)
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
