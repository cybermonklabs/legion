package api

// post_deleted_ask_thread_reads_test.go holds R2 and R3 of dispatch://INFRA-1523 on the reads of
// an ask's thread the census found no test of after a reply's delete: an issue's ask list with its
// threads (row 1.13), the one ask (1.14) and the Inbox (1.15).
//
// The row numbers in subtest names and comments are the rows of the issue's surface census,
// dispatch://INFRA-1523/artifact/surface-census-md.

import (
	"net/http"
	"testing"
)

// Every read of an ask's thread, made while alice's reply in it stands and again after she deletes
// it. She edited the reply, so every copy of it carries the edit's time (R2), and bob quotes its
// first block, so the thread carries a reply's quote of it. Before the delete each read gives the
// reply's words; after it each gives none of them, and says on the reply and on bob's quote of it
// that the reply is deleted.
func TestEveryReadOfAnAsksThreadSaysADeletedReplyIsDeletedAndGivesNoneOfIt(t *testing.T) {
	t.Parallel()
	handler := newTestHandler(t)
	issue := createInteractionIssue(t, handler, "TEST", "A thread with a withdrawn reply", "before")
	askID := openTurnAsk(t, handler, issue.Key, "Which auditor signs off?")
	const phrase = "zarquonwithdrawnanswer"
	reply := createThreadComment(t, handler, issue.Key, map[string]any{
		"body": phrase + " as first written.\n\nThe second paragraph.", "ask_id": askID,
	}, "alice")
	editThreadComment(t, handler, reply.ID, phrase+" as corrected.\n\nThe second paragraph.", "alice")
	createThreadComment(t, handler, issue.Key, map[string]any{
		"body": "On the first paragraph.", "reply_to": reply.ID, "reply_to_block": "b1",
	}, "bob")

	// Each read carries the thread's replies: scanComment's mark on alice's reply, and
	// refs.ReplyBlocks' mark on bob's quote of it; the delete's clear (clearDeletedPostBody) took
	// the body and the excerpt the quote serves.
	post := postCopies{kind: "comment", id: reply.ID, by: "alice", posts: 1, quotes: 1, edited: true}
	rows := []struct{ read, path string }{
		// listIssueAsks with ?thread=1: loadAskThreads, loadInboxAskReplyChains, scanComment.
		{"1.13 GET /api/v1/issues/{key}/asks?thread=1", "/api/v1/issues/" + issue.Key + "/asks?thread=1"},
		// getAsk: loadReplyChain, scanComment.
		{"1.14 GET /api/v1/asks/{id}", "/api/v1/asks/" + askID},
		// listInbox: loadAskThreads, as 1.13.
		{"1.15 GET /api/v1/inbox", "/api/v1/inbox"},
	}
	for _, row := range rows {
		response := dispatchRequest(t, handler, http.MethodGet, row.path, nil, "alice")
		if response.Code != http.StatusOK {
			t.Fatalf("%s before the delete: status=%d body=%s", row.read, response.Code, response.Body.String())
		}
		requireStandingPostRead(t, row.read, response.Body.Bytes(), phrase, post)
	}

	deleteThreadComment(t, handler, reply.ID, "alice")

	for _, row := range rows {
		t.Run(row.read, func(t *testing.T) {
			response := dispatchRequest(t, handler, http.MethodGet, row.path, nil, "alice")
			if response.Code != http.StatusOK {
				t.Fatalf("%s after the delete: status=%d body=%s", row.read, response.Code, response.Body.String())
			}
			requireDeletedPostRead(t, row.read, response.Body.Bytes(), phrase, post)
		})
	}
}
