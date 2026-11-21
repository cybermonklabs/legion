package api

// post_deleted_message_reads_test.go holds R2 and R3 of dispatch://INFRA-1523 on the reads of an
// issue message the census found no test of after the message's delete: its history (row 1.5),
// its blocks (1.7), the issue's threads and the one thread (1.8, 1.9), the conversation by any of
// its ids (1.10), the session's conversations (1.11), and the reference graph's node and block of
// a message (the message half of 1.23).
//
// The row numbers in subtest names and comments are the rows of the issue's surface census,
// dispatch://INFRA-1523/artifact/surface-census-md.

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// Every read of one issue message, made while it stands and again after its author deletes it.
// The message is targeted at a session, so the session's conversations list it; its author edited
// it, so its history holds the body the edit replaced and every copy of it carries the edit's time
// (R2); bob quotes its first block in one reply and answers it plainly in another, so a thread
// carries a reply's quote of it and the reference graph an edge ending at it. Before the delete
// each read gives the message's words; after it each gives none of them, and says the message is
// deleted wherever its answer carries the message.
func TestEveryReadOfADeletedIssueMessageSaysItIsDeletedAndGivesNoneOfIt(t *testing.T) {
	f := newAnswersFixture(t)
	const phrase = "zarquonwithdrawnmessage"
	message := f.message(t, map[string]any{
		"body":   phrase + " as first written.\n\nThe second paragraph.",
		"target": "session:" + answersSession, "delivery": "btw",
	}, "alice")
	editIssueMessage(t, f.handler, message, phrase+" as corrected.\n\nThe second paragraph.", "alice")
	createIssueMessage(t, f.handler, f.issue, map[string]any{
		"body": "On the first paragraph.", "in_reply_to": message, "in_reply_to_block": "b1",
	}, "bob")
	plain := createIssueMessage(t, f.handler, f.issue, map[string]any{"body": "A plain answer.", "in_reply_to": message}, "bob")
	reference := "dispatch://" + f.issue + "/message/" + message

	// thread is a read of the thread: the message itself and bob's quote of its first block.
	thread := postCopies{kind: "message", id: message, by: "alice", posts: 1, quotes: 1, edited: true}
	// node is a read of the reference graph: the message's own node.
	node := postCopies{kind: "message", id: message, by: "alice", posts: 1, edited: true}
	rows := []struct {
		read, path string
		post       postCopies
		// gone is the check of the read after the delete where its answer is no copy of the
		// message at all; nil is requireDeletedPostRead's.
		gone func(t *testing.T, response *httptest.ResponseRecorder)
	}{
		// scanMessage's mark on the message, and refs.ReplyBlocks' mark on bob's quote; the
		// delete's clear (clearDeletedPostBody) took the body and the excerpt the quote serves.
		{read: "1.8 GET /api/v1/issues/{key}/messages", path: "/api/v1/issues/" + f.issue + "/messages", post: thread},
		// scanMessage's and refs.ReplyBlocks' marks, as 1.8.
		{read: "1.9 GET /api/v1/issues/{key}/messages/{id}", path: "/api/v1/issues/" + f.issue + "/messages/" + message, post: thread},
		// scanMessage's and refs.ReplyBlocks' marks, as 1.8: readableMessageThread reads the
		// thread through readMessage.
		{read: "1.10 GET /api/v1/messages/{id}", path: "/api/v1/messages/" + message, post: thread},
		// scanMessage's and refs.ReplyBlocks' marks, as 1.8: listAgentMessages scans each root
		// with scanMessage.
		{read: "1.11 GET /api/v1/agents/{session_id}/messages", path: "/api/v1/agents/" + answersSession + "/messages", post: thread},
		// The delete's clear leaves the body no block, and the reference graph marks the message's
		// own node (refs.loadMessageNodes), which the answer's item is.
		{read: "1.7 GET /api/v1/messages/{id}/blocks", path: "/api/v1/messages/" + message + "/blocks", post: node},
		// The reference graph's node: the edge from bob's plain answer ends at the message's node,
		// which carries the mark, and its excerpt is cut from the body the delete cleared.
		{
			read: "1.23 GET /api/v1/references?from=, an edge ending at the message",
			path: "/api/v1/references?" + url.Values{"from": {"dispatch://" + f.issue + "/message/" + plain.ID}}.Encode(),
			post: node,
		},
		// The reference graph's block node: a block read of a deleted post is refused with the
		// deletion named (itemBlockNotFound, postIsDeleted), where it gave the block's text.
		{
			read: "1.23 GET /api/v1/references?to=, a block of the message",
			path: "/api/v1/references?" + url.Values{"to": {reference + "#b-b1"}}.Encode(),
			post: postCopies{kind: "message", id: message, by: "alice"},
			gone: func(t *testing.T, response *httptest.ResponseRecorder) {
				t.Helper()
				if body := response.Body.String(); response.Code != http.StatusNotFound ||
					!strings.Contains(body, `"code":"NOT_FOUND"`) || !strings.Contains(body, ": this message is deleted") {
					t.Fatalf("the block read after the delete: status=%d body=%s, want 404 NOT_FOUND saying the message is deleted", response.Code, body)
				}
			},
		},
		// The history read's own statement (postHistory, post_history.go) gives a deleted post's
		// kept bodies as none, under the mark.
		{
			read: "1.5 GET /api/v1/messages/{id}/history",
			path: "/api/v1/messages/" + message + "/history",
			post: postCopies{kind: "message", id: message, by: "alice"},
			gone: func(t *testing.T, response *httptest.ResponseRecorder) {
				t.Helper()
				if response.Code != http.StatusOK {
					t.Fatalf("the history after the delete: status=%d body=%s", response.Code, response.Body.String())
				}
				history := decodeBody[postHistoryRead](t, response)
				if history.DeletedAt == nil || history.DeletedBy == nil || history.DeletedBy.ID != "alice" {
					t.Fatalf("the history's mark after the delete = %v by %v, want alice's delete", history.DeletedAt, history.DeletedBy)
				}
				if len(history.Entries) == 0 {
					t.Fatalf("the history after the delete holds no row, want the edit's and the delete's, each with no body")
				}
				for index, entry := range history.Entries {
					if entry.Body != "" {
						t.Fatalf("history row %d after the delete gives %q, want no body", index, entry.Body)
					}
				}
			},
		},
	}
	for _, row := range rows {
		response := dispatchRequest(t, f.handler, http.MethodGet, row.path, nil, "alice")
		if response.Code != http.StatusOK {
			t.Fatalf("%s before the delete: status=%d body=%s", row.read, response.Code, response.Body.String())
		}
		requireStandingPostRead(t, row.read, response.Body.Bytes(), phrase, row.post)
	}

	deleteIssueMessage(t, f.handler, message, "alice")

	for _, row := range rows {
		t.Run(row.read, func(t *testing.T) {
			response := dispatchRequest(t, f.handler, http.MethodGet, row.path, nil, "alice")
			if row.gone != nil {
				if strings.Contains(response.Body.String(), phrase) {
					t.Fatalf("%s after the delete gives the deleted text %q: %s", row.read, phrase, response.Body.String())
				}
				row.gone(t, response)
				return
			}
			if response.Code != http.StatusOK {
				t.Fatalf("%s after the delete: status=%d body=%s", row.read, response.Code, response.Body.String())
			}
			requireDeletedPostRead(t, row.read, response.Body.Bytes(), phrase, row.post)
		})
	}
}
