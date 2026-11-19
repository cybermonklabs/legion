package api

import (
	"context"
	"net/http"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// The ask and message writers, and the comment writer's project.
// object_nodes_issue_test.go holds the issue and document writers and
// defines the helpers these read through.

// eventNaming answers the id of the newest event of type whose payload names the object id, at
// the top level or under the object it carries.
func eventNaming(t *testing.T, database *store.Store, eventType, id string) int64 {
	t.Helper()
	var event int64
	if err := database.Pool.QueryRow(context.Background(), `
		select id from events
		where type = $1 and $2 in (payload->>'id', payload->'message'->>'id', payload->'ask'->>'id', payload->'comment'->>'id')
		order by id desc limit 1
	`, eventType, id).Scan(&event); err != nil {
		t.Fatalf("read the %s event naming %s: %v", eventType, id, err)
	}
	return event
}

// TestEveryAskCreateStoresItsNode holds every writer of an ask to writing its node before its row,
// in the project of the issue or the document it is asked on, caused by its ask.opened: a
// question on an issue (createAskFor), a question on a project document, an approval request
// (openApprovalAsk) and the ask a settled document's new ask block opens (createAskBlock).
func TestEveryAskCreateStoresItsNode(t *testing.T) {
	handler, database := newTestHandlerWithStore(t)
	requireNodeBeforeRow(t, database, "asks", graphstore.KindAsk, "id")
	issue := createInteractionIssue(t, handler, "ASKN", "Asks name their project", "The spec.\n")
	runbook := createProjectDocument(t, handler, "ASKN", "Runbook.md", "# Runbook\n\nStep one.\n")

	onIssue := createIssueAsk(t, handler, issue.Key, "Does the issue's ask name its project?")
	requireObjectNode(t, database, graphstore.KindAsk, onIssue, graphstore.SupertagAsk, "ASKN", nil,
		eventNaming(t, database, "ask.opened", onIssue))

	asked := sessionRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+runbook.ID+"/asks", map[string]any{
		"actor": sessionActor(), "question": "Does the document's ask name its project?",
	})
	if asked.Code != http.StatusCreated {
		t.Fatalf("ask on the runbook: status=%d body=%s", asked.Code, asked.Body.String())
	}
	onDocument := decodeBody[model.Ask](t, asked).ID
	requireObjectNode(t, database, graphstore.KindAsk, onDocument, graphstore.SupertagAsk, "ASKN", nil,
		eventNaming(t, database, "ask.opened", onDocument))

	requestApproval(t, handler, issue.PrimaryArtifactID)
	var approval string
	if err := database.Pool.QueryRow(context.Background(),
		`select id::text from asks where kind = 'approval' and issue_key = $1`, issue.Key).Scan(&approval); err != nil {
		t.Fatalf("read the approval request: %v", err)
	}
	requireObjectNode(t, database, graphstore.KindAsk, approval, graphstore.SupertagAsk, "ASKN", nil,
		eventNaming(t, database, "ask.opened", approval))

	uploaded := sessionRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/artifacts", map[string]any{
		"actor": sessionActor(), "name": "spec.md",
		"content": "Before\n\n:::ask{#node-ask urgency=\"high\" multiple=\"false\"}\nShip the decision?\n\n- Ship: Now.\n- Hold: Later.\n:::\n",
	})
	if uploaded.Code != http.StatusCreated {
		t.Fatalf("upload a spec holding an ask block: status=%d body=%s", uploaded.Code, uploaded.Body.String())
	}
	settled := indexedDecisionBlocks(t, handler, issue.Key, issue.PrimaryArtifactID, map[string]string{"node-ask": "Ship the decision?"})["node-ask"]
	requireObjectNode(t, database, graphstore.KindAsk, settled, graphstore.SupertagAsk, "ASKN", nil,
		eventNaming(t, database, "ask.opened", settled))
}

// TestEveryMessageCreateStoresItsNode holds every writer of a message to writing its node before
// its row, caused by the event that records it: a message on an issue, in that issue's project; a
// direct message to a session, between the person who sent it and that session; the session's
// reply in that thread, between the same two, read off the root it joins; and a broadcast's copy,
// between its sender and its recipient.
func TestEveryMessageCreateStoresItsNode(t *testing.T) {
	handler, database, root, reply, _ := directConversationFrom(t, "alice")
	requireNodeBeforeRow(t, database, "messages", graphstore.KindMessage, "id")
	direct := []string{graphstore.PartyPerson("alice"), "session:s1"}

	// The direct conversation's root was written before the trigger: its node is held to its
	// parties all the same.
	requireObjectNode(t, database, graphstore.KindMessage, root.ID, graphstore.SupertagMessage, "", direct,
		eventNaming(t, database, "message.created", root.ID))

	answered := reply("The session's answer.")
	if answered.Code != http.StatusCreated {
		t.Fatalf("the session's reply: status=%d body=%s", answered.Code, answered.Body.String())
	}
	sessionReply := decodeBody[model.Message](t, answered).ID
	requireObjectNode(t, database, graphstore.KindMessage, sessionReply, graphstore.SupertagMessage, "", direct,
		eventNaming(t, database, "message.answered", sessionReply))

	second := dispatchRequest(t, handler, http.MethodPost, "/api/v1/agents/s1/messages", map[string]any{
		"body": "A second direct message.", "delivery": "btw",
	}, "alice")
	if second.Code != http.StatusCreated {
		t.Fatalf("a second direct message: status=%d body=%s", second.Code, second.Body.String())
	}
	secondID := decodeBody[model.Message](t, second).ID
	requireObjectNode(t, database, graphstore.KindMessage, secondID, graphstore.SupertagMessage, "", direct,
		eventNaming(t, database, "message.created", secondID))

	broadcast := dispatchRequest(t, handler, http.MethodPost, "/api/v1/broadcasts", map[string]any{
		"body": "To everyone.", "delivery": "btw", "session_ids": []string{"s1"},
	}, "alice")
	if broadcast.Code != http.StatusCreated {
		t.Fatalf("a broadcast: status=%d body=%s", broadcast.Code, broadcast.Body.String())
	}
	var copyID string
	if err := database.Pool.QueryRow(context.Background(),
		`select id::text from messages where broadcast_id is not null`).Scan(&copyID); err != nil {
		t.Fatalf("read the broadcast's copy: %v", err)
	}
	requireObjectNode(t, database, graphstore.KindMessage, copyID, graphstore.SupertagMessage, "", direct,
		eventNaming(t, database, "message.created", copyID))

	if response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/projects", map[string]string{
		"key": "MSGN", "name": "Messages name their project",
	}, "alice"); response.Code != http.StatusCreated {
		t.Fatalf("create project: status=%d body=%s", response.Code, response.Body.String())
	}
	created := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues", map[string]string{
		"project": "MSGN", "title": "Messages on an issue", "spec": "The spec.\n",
	}, "alice")
	if created.Code != http.StatusCreated {
		t.Fatalf("create issue: status=%d body=%s", created.Code, created.Body.String())
	}
	issueKey := decodeBody[model.Issue](t, created).Key
	onIssue := createIssueMessage(t, handler, issueKey, map[string]any{"body": "A message on the issue."}, "alice")
	requireObjectNode(t, database, graphstore.KindMessage, onIssue.ID, graphstore.SupertagMessage, "MSGN", nil,
		eventNaming(t, database, "message.created", onIssue.ID))
}

// TestACommentNodeCarriesItsProject holds the one comment writer (commentStored, through
// graphstore.CommentFacts) to giving a comment's node the project of what it belongs to, through
// both of its callers: a comment on an issue and on a project document (createCommentFor), and a
// session's reply to a mention (replyComment).
func TestACommentNodeCarriesItsProject(t *testing.T) {
	rig := newStoreWriterRig(t)
	for name, path := range map[string]string{
		"an issue's comment":   "/api/v1/issues/" + rig.issue + "/comments",
		"a document's comment": "/api/v1/artifacts/" + rig.document + "/comments",
	} {
		created := dispatchRequest(t, rig.handler, http.MethodPost, path, map[string]any{"body": "A remark."}, "alice")
		if created.Code != http.StatusCreated {
			t.Fatalf("%s: status=%d body=%s", name, created.Code, created.Body.String())
		}
		id := decodeMentionedComment(t, created).ID
		requireCommentProject(t, rig.database, id, "LOCK")
	}

	answered := decodeMentionedComment(t, postMentionedComment(t, rig.handler, rig.issue, map[string]any{
		"body": "Does this ship, @s1?", "mentions": []map[string]any{{"target": "session:s1"}},
	}))
	settledCommentDeliveries(t, rig.database, answered.ID)
	replied := bearerRequest(t, rig.handler, http.MethodPost, "/api/v1/comments/"+answered.ID+"/reply", map[string]any{
		"actor": map[string]any{"kind": "session", "id": "s1"}, "attempt": 1, "body": "Yes, it ships.",
	})
	if replied.Code != http.StatusCreated {
		t.Fatalf("the session's reply: status=%d body=%s", replied.Code, replied.Body.String())
	}
	requireCommentProject(t, rig.database, decodeMentionedComment(t, replied).ID, "LOCK")
}

func requireCommentProject(t *testing.T, database *store.Store, id, project string) {
	t.Helper()
	node, held, err := graphstore.ReadNode(context.Background(), database.Pool, graphstore.KindComment, id)
	if err != nil || !held {
		t.Fatalf("the store holds no node of comment %s (held %t, %v)", id, held, err)
	}
	if node.Project != project || node.Parties != nil {
		t.Errorf("comment %s's node carries project %q and parties %v, want %q and none", id, node.Project, node.Parties, project)
	}
	value, stated, err := graphstore.CurrentValue(context.Background(), database.Pool, graphstore.KindComment, id, graphstore.AttributeProject)
	if err != nil || !stated || value != project {
		t.Errorf("comment %s states project %q (stated %t, %v), want %q", id, value, stated, err, project)
	}
}
