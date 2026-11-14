// question_reference_create_test.go holds the `#id` edge of a source this transaction creates:
// a route writing a new ask, comment, reply, issue message, approval request or settled ask
// block has only queued that row's insert (INFRA-636), so the document its `#id`s resolve
// against cannot be read back from the row and is handed to the reader instead
// (refs.PendingDocument). Each test writes `#q1` against a settled question block of the issue's
// spec and reads the one edge it must store, or the event that must name the target it moved.
package api

import (
	"context"
	"net/http"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// issueRef is the issue createInteractionIssue answers with.
type issueRef = struct {
	Key               string `json:"key"`
	PrimaryArtifactID string `json:"primary_artifact_id"`
}

// settledQuestionFixture is an issue whose spec holds one settled question block, `#q1`, which
// is what a `#q1` written anywhere in that issue's scope names (refs.QuestionMap). It answers
// the handler, the store, the issue and the id of the ask settlement made for the block. The
// block is written through the document service rather than seeded, so a settlement running
// beside the test cannot replace the version that declares it.
func settledQuestionFixture(t *testing.T, title string) (http.Handler, *store.Store, issueRef, string) {
	t.Helper()
	handler, database := blockAskHandler(t)
	issue := createInteractionIssue(t, handler, "TEST", title, "Context\n")
	addAskBlock(t, handler, issue.PrimaryArtifactID, "q1", "Which way?")
	return handler, database, issue, blockAskID(t, handler, issue.Key, "q1")
}

// addAskBlock writes one ask block at the end of the document and waits for settlement to index
// it, which is what gives the block its ask.
func addAskBlock(t *testing.T, handler http.Handler, artifactID, blockID, question string) {
	t.Helper()
	written := sessionRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+artifactID+"/edits", map[string]any{
		"actor": sessionActor(),
		"ops":   []map[string]string{{"op": "insert", "after": "end", "markdown": ":::ask{#" + blockID + "}\n" + question + "\n:::\n"}},
	})
	if written.Code != http.StatusOK {
		t.Fatalf("write the ask block %q: status=%d body=%s", blockID, written.Code, written.Body.String())
	}
	awaitIndexedAskBlock(t, handler, artifactID, blockID, question)
}

// questionEdges is how many edges one source stores to the settled question's ask.
func questionEdges(t *testing.T, database *store.Store, fromKind, fromID, askID string) int {
	t.Helper()
	var edges int
	if err := database.Pool.QueryRow(context.Background(), `
		select count(*) from refs
		where from_kind = $1 and from_id = $2 and to_kind = 'ask' and to_id = $3
	`, fromKind, fromID, askID).Scan(&edges); err != nil {
		t.Fatalf("count the `#q1` edges of %s %s: %v", fromKind, fromID, err)
	}
	return edges
}

// A `#q1` in the question of an ask this route creates names the question block of the issue's
// spec, the document the ask belongs to once its queued row is written (refs.ScopeOf), and is
// stored as that ask's edge.
func TestANewAsksQuestionReferenceResolvesAgainstItsIssuesSpec(t *testing.T) {
	handler, database, issue, question := settledQuestionFixture(t, "A new ask citing a question")

	created := sessionRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/asks", map[string]any{
		"question": "Does #q1 settle this?", "actor": sessionActor(),
	})
	if created.Code != http.StatusCreated {
		t.Fatalf("create the ask: status=%d body=%s", created.Code, created.Body.String())
	}
	ask := decodeBody[struct {
		ID string `json:"id"`
	}](t, created)
	if edges := questionEdges(t, database, "ask", ask.ID, question); edges != 1 {
		t.Fatalf("the new ask's `#q1` edges to %s = %d, want 1", question, edges)
	}
}

// The same for a comment this route creates on the issue: its `#q1` resolves against the issue's
// spec, the document ScopeOf answers for a comment that names none itself.
func TestANewCommentsQuestionReferenceResolvesAgainstItsIssuesSpec(t *testing.T) {
	handler, database, issue, question := settledQuestionFixture(t, "A new comment citing a question")

	created := sessionRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/comments", map[string]any{
		"body": "Answered by #q1.", "actor": sessionActor(),
	})
	if created.Code != http.StatusCreated {
		t.Fatalf("create the comment: status=%d body=%s", created.Code, created.Body.String())
	}
	comment := decodeBody[struct {
		ID string `json:"id"`
	}](t, created)
	if edges := questionEdges(t, database, "comment", comment.ID, question); edges != 1 {
		t.Fatalf("the new comment's `#q1` edges to %s = %d, want 1", question, edges)
	}
}

// A reply names no document of its own: its scope is its parent's, which ScopeOf walks the
// thread for, and the reply's queued row cannot answer that walk.
func TestANewReplysQuestionReferenceResolvesAgainstItsThreadsDocument(t *testing.T) {
	handler, database, issue, question := settledQuestionFixture(t, "A new reply citing a question")

	root := sessionRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/comments", map[string]any{
		"body": "The thread root.", "actor": sessionActor(),
	})
	if root.Code != http.StatusCreated {
		t.Fatalf("create the thread root: status=%d body=%s", root.Code, root.Body.String())
	}
	rootComment := decodeBody[struct {
		ID string `json:"id"`
	}](t, root)
	created := sessionRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/comments", map[string]any{
		"body": "Answered by #q1.", "reply_to": rootComment.ID, "actor": sessionActor(),
	})
	if created.Code != http.StatusCreated {
		t.Fatalf("create the reply: status=%d body=%s", created.Code, created.Body.String())
	}
	reply := decodeBody[struct {
		ID string `json:"id"`
	}](t, created)
	if edges := questionEdges(t, database, "comment", reply.ID, question); edges != 1 {
		t.Fatalf("the new reply's `#q1` edges to %s = %d, want 1", question, edges)
	}
}

// An issue message belongs to its issue's spec, and its row's insert is queued with the rest of
// the write (createStoredMessage), so its `#q1` is resolved from what that row will hold.
func TestANewIssueMessagesQuestionReferenceResolvesAgainstItsIssuesSpec(t *testing.T) {
	handler, database, issue, question := settledQuestionFixture(t, "A new message citing a question")

	message := createIssueMessage(t, handler, issue.Key, map[string]any{"body": "Answered by #q1."}, "alice")
	if edges := questionEdges(t, database, "message", message.ID, question); edges != 1 {
		t.Fatalf("the new message's `#q1` edges to %s = %d, want 1", question, edges)
	}
}

// An approval request is an ask this route opens with its row's insert queued too, and the
// summary the requester wrote is part of its question: a `#q1` in that summary resolves against
// the issue's spec, the document the request belongs to.
func TestANewApprovalRequestsQuestionReferenceResolvesAgainstItsIssuesSpec(t *testing.T) {
	handler, database, issue, question := settledQuestionFixture(t, "An approval request citing a question")

	requested := sessionRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+issue.PrimaryArtifactID+"/approval-requests", map[string]any{
		"actor": sessionActor(), "summary": "It settles #q1.",
	})
	if requested.Code != http.StatusCreated {
		t.Fatalf("request approval: status=%d body=%s", requested.Code, requested.Body.String())
	}
	request := decodeBody[struct {
		Ask struct {
			ID string `json:"id"`
		} `json:"ask"`
	}](t, requested)
	if edges := questionEdges(t, database, "ask", request.Ask.ID, question); edges != 1 {
		t.Fatalf("the approval request's `#q1` edges to %s = %d, want 1", question, edges)
	}
}

// Settlement opens the ask of a new question block with that ask's insert queued, and the
// document the block is on - the one that declares every question a `#q1` in it can name - is
// the ask's own scope. A second question block citing the first stores that edge.
func TestANewAskBlocksQuestionReferenceResolvesAgainstItsOwnDocument(t *testing.T) {
	handler, database, issue, question := settledQuestionFixture(t, "An ask block citing a question")

	addAskBlock(t, handler, issue.PrimaryArtifactID, "q2", "Does #q1 settle this?")
	citing := blockAskID(t, handler, issue.Key, "q2")
	if edges := questionEdges(t, database, "ask", citing, question); edges != 1 {
		t.Fatalf("the new ask block's `#q1` edges to %s = %d, want 1", question, edges)
	}
}

// Settlement opens the ask of every new question block of one edit in one transaction, each
// insert queued and no read of the asks table answering for it. A `#q2` written in the question
// of a block that same edit declares still names the ask of that block, because the question
// map is the document's as the transaction leaves it (refs.QuestionMap).
func TestQuestionReferencesToABlockOfTheSameEditResolve(t *testing.T) {
	handler, database := blockAskHandler(t)
	issue := createInteractionIssue(t, handler, "TEST", "Two question blocks in one edit", "Context\n")
	written := sessionRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+issue.PrimaryArtifactID+"/edits", map[string]any{
		"actor": sessionActor(),
		"ops": []map[string]string{{"op": "insert", "after": "end", "markdown": "" +
			":::ask{#q2}\nWhich way?\n:::\n\nSettled by #q2.\n\n:::ask{#q3}\nDoes #q2 settle this?\n:::\n"}},
	})
	if written.Code != http.StatusOK {
		t.Fatalf("write the two ask blocks: status=%d body=%s", written.Code, written.Body.String())
	}
	awaitIndexedAskBlock(t, handler, issue.PrimaryArtifactID, "q3", "Does #q2 settle this?")
	question := blockAskID(t, handler, issue.Key, "q2")
	citing := blockAskID(t, handler, issue.Key, "q3")
	if edges := questionEdges(t, database, "ask", citing, question); edges != 1 {
		t.Fatalf("the second new ask block's `#q2` edges to %s = %d, want 1", question, edges)
	}
}

// The edge is only half of what the write owes: the ask.opened of the citing block names the
// ask it moved, so a reader refreshes that ask's cards and its panel by name rather than
// falling back. The target's row is one this very transaction queued, and a read of the asks
// table answers for none of it (refs.Pending).
func TestANewAskBlocksEventNamesTheAskOfTheSameEdit(t *testing.T) {
	handler, _ := blockAskHandler(t)
	issue := createInteractionIssue(t, handler, "TEST", "An event naming a question of the same edit", "Context\n")
	written := sessionRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+issue.PrimaryArtifactID+"/edits", map[string]any{
		"actor": sessionActor(),
		"ops": []map[string]string{{"op": "insert", "after": "end", "markdown": "" +
			":::ask{#q2}\nWhich way?\n:::\n\nSettled by #q2.\n\n:::ask{#q3}\nDoes #q2 settle this?\n:::\n"}},
	})
	if written.Code != http.StatusOK {
		t.Fatalf("write the two ask blocks: status=%d body=%s", written.Code, written.Body.String())
	}
	awaitIndexedAskBlock(t, handler, issue.PrimaryArtifactID, "q3", "Does #q2 settle this?")
	question := blockAskID(t, handler, issue.Key, "q2")
	citing := blockAskID(t, handler, issue.Key, "q3")
	requireReferencesChanged(t, handler, issue.Key, "ask.opened", citing, []model.ChangedReference{
		{Kind: "ask", ID: question, IssueKey: &issue.Key},
	})
}
