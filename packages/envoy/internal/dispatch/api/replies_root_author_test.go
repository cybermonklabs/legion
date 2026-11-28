package api

import (
	"net/http"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// sessionMessage posts an issue message as the actor input names, under the shared token.
func sessionMessage(t *testing.T, handler http.Handler, issueKey string, input map[string]any) model.Message {
	t.Helper()
	response := sessionRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issueKey+"/messages", input)
	if response.Code != http.StatusCreated {
		t.Fatalf("session message on %s: status=%d body=%s", issueKey, response.Code, response.Body.String())
	}
	return decodeBody[model.Message](t, response)
}

// Each Reply row names the actor who wrote its thread's root, as the root's author is stored: the
// asker of an ask, the author of a comment thread's root comment and of a message thread's root
// message, and the Legion daemon's session for a message thread the daemon started. Bob wrote
// every root but the daemon's, Alice wrote in each thread after it, and a session wrote last, so
// the root's writer is neither the reader nor the newest reply's author. An Agent row, whose root
// is always the caller's own message, carries none.
func TestMyRepliesCarriesTheRootAuthor(t *testing.T) {
	t.Parallel()
	handler, _, _, _, _ := directConversationFrom(t, "alice")
	issue := createInteractionIssue(t, handler, "ACME", "Root authors", "A spec")
	// Bob signs in and the issue is his, so his open ask waits on him: an open ask that waits on
	// Alice is her Inbox row and never her Reply row (myRepliesOwner).
	getMyReplies(t, handler, "bob", "")
	assignIssue(t, handler, issue.Key, "bob")
	comments := "/api/v1/issues/" + issue.Key + "/comments"
	replier := sessionActor()

	asked := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/asks", map[string]any{
		"question": "Which region?",
	}, "bob")
	if asked.Code != http.StatusCreated {
		t.Fatalf("bob's ask: status=%d body=%s", asked.Code, asked.Body.String())
	}
	ask := decodeBody[model.Ask](t, asked)
	createThreadComment(t, handler, issue.Key, map[string]any{"body": "The one closest to the users?", "ask_id": ask.ID}, "alice")
	sessionComment(t, handler, comments, map[string]any{"body": "That is eu-west-1.", "ask_id": ask.ID})

	root := createThreadComment(t, handler, issue.Key, map[string]any{"body": "Ship it on Friday?"}, "bob")
	createThreadComment(t, handler, issue.Key, map[string]any{"body": "Friday works for me.", "reply_to": root.ID}, "alice")
	sessionComment(t, handler, comments, map[string]any{"body": "Friday it is.", "reply_to": root.ID})

	message := createIssueMessage(t, handler, issue.Key, map[string]any{"body": "Status?"}, "bob")
	checking := createIssueMessage(t, handler, issue.Key, map[string]any{"body": "Still checking.", "in_reply_to": message.ID}, "alice")
	sessionMessage(t, handler, issue.Key, map[string]any{"body": "Green.", "in_reply_to": checking.ID, "actor": replier})

	daemon := daemonSessionOf("ACME")
	lifecycle := sessionMessage(t, handler, issue.Key, map[string]any{
		"body": "Legion moved the issue to implement.", "actor": daemon,
	})
	why := createIssueMessage(t, handler, issue.Key, map[string]any{"body": "Why now?", "in_reply_to": lifecycle.ID}, "alice")
	sessionMessage(t, handler, issue.Key, map[string]any{"body": "The plan was approved.", "in_reply_to": why.ID, "actor": replier})

	page := getMyReplies(t, handler, "alice", "?issue="+issue.Key+"&state=unread")
	if page.Total != 4 || len(page.Rows) != 4 {
		t.Fatalf("page = %#v, want the ask, the comment thread and the two message threads", page)
	}
	bob := map[string]any{"kind": "user", "id": "bob"}
	for _, want := range []struct {
		name, thread, kind string
		author             map[string]any
	}{
		{"the ask's asker", ask.ID, "ask", bob},
		{"the root comment's author", root.ID, "comment", bob},
		{"the root message's author", message.ID, "message", bob},
		{"the Legion daemon's session", lifecycle.ID, "message", daemon},
	} {
		row := replyRow(t, page, want.thread)
		if row.Kind != want.kind {
			t.Errorf("%s: row kind = %q, want %q", want.name, row.Kind, want.kind)
		}
		if row.Reply.Author.Kind != replier["kind"] || row.Reply.Author.ID != replier["id"] {
			t.Errorf("%s: the newest reply's author = %#v, want the replying session", want.name, row.Reply.Author)
		}
		if row.RootAuthor == nil || row.RootAuthor.Kind != want.author["kind"] || row.RootAuthor.ID != want.author["id"] {
			t.Errorf("%s: root_author = %#v, want %s %s", want.name, row.RootAuthor, want.author["kind"], want.author["id"])
		}
	}

	btw, _ := agentThread(t, handler, "btw", "Deploy is tonight.", "Noted.")
	agents := getMyReplies(t, handler, "alice", "?kind=agent")
	if row := replyRow(t, agents, btw.ID); row.Kind != "agent" || row.RootAuthor != nil {
		t.Fatalf("agent row = %#v, want an agent row with no root_author", row)
	}
	for _, row := range agents.Rows {
		if row.RootAuthor != nil {
			t.Fatalf("agent row %s carries root_author %#v, want none", row.ThreadID, row.RootAuthor)
		}
	}
}
