// post_corrections_actor_test.go is who the four correction routes read as the caller. The
// author of a post is a session or a person, and a person acts through two credentials: their
// browser's cookie, which the author test of each route holds, and a personal token of their
// own, which this file holds.
package api

import (
	"net/http"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// A person's own personal token, holding the person scope and naming no session, corrects the
// posts that person wrote: their comment and their issue message, each edited and each
// deleted. The token proves no session of its own, so without correctionActor
// (comment_actions.go) bearerSessionActor asks its body to name one - 400 ACTOR_KIND - and a
// body naming one makes the write that session's, which the author is not - 403 NOT_AUTHOR.
// Somebody else's post is refused to that same token, because it carries its owner's
// authorship and nobody else's.
func TestAPersonsOwnTokenCorrectsTheirOwnPostAndNobodyElses(t *testing.T) {
	handler := newTestHandler(t)
	issue := createInteractionIssue(t, handler, "TOK", "A person's token corrects", "before")
	token := mintPersonalToken(t, handler, "alice", actingTokenName, []string{model.ScopePerson})

	comment := createThreadComment(t, handler, issue.Key, map[string]any{"body": "alice's first comment body"}, "alice")
	message := createIssueMessage(t, handler, issue.Key, map[string]any{"body": "alice's first message body"}, "alice")
	strangerComment := createThreadComment(t, handler, issue.Key, map[string]any{"body": "bob's comment body"}, "bob")
	strangerMessage := createIssueMessage(t, handler, issue.Key, map[string]any{"body": "bob's message body"}, "bob")

	editedComment := agentRequest(t, handler, http.MethodPatch, "/api/v1/comments/"+comment.ID,
		map[string]any{"body": "alice's second comment body"}, token.Token)
	if editedComment.Code != http.StatusOK {
		t.Fatalf("alice's token edits her own comment: status=%d body=%s, want 200",
			editedComment.Code, editedComment.Body.String())
	}
	if updated := decodeBody[model.Comment](t, editedComment); updated.Body != "alice's second comment body" || updated.EditedAt == nil {
		t.Fatalf("the edited comment = %#v, want her new body and edited_at", updated)
	}

	editedMessage := agentRequest(t, handler, http.MethodPatch, "/api/v1/messages/"+message.ID,
		map[string]any{"body": "alice's second message body"}, token.Token)
	if editedMessage.Code != http.StatusOK {
		t.Fatalf("alice's token edits her own message: status=%d body=%s, want 200",
			editedMessage.Code, editedMessage.Body.String())
	}
	if updated := decodeBody[model.Message](t, editedMessage); updated.Body != "alice's second message body" || updated.EditedAt == nil {
		t.Fatalf("the edited message = %#v, want her new body and edited_at", updated)
	}

	for _, refusal := range []struct {
		name   string
		method string
		path   string
		body   map[string]any
	}{
		{"edit bob's comment", http.MethodPatch, "/api/v1/comments/" + strangerComment.ID, map[string]any{"body": "her edit of his comment"}},
		{"delete bob's comment", http.MethodDelete, "/api/v1/comments/" + strangerComment.ID, map[string]any{}},
		{"edit bob's message", http.MethodPatch, "/api/v1/messages/" + strangerMessage.ID, map[string]any{"body": "her edit of his message"}},
		{"delete bob's message", http.MethodDelete, "/api/v1/messages/" + strangerMessage.ID, map[string]any{}},
	} {
		response := agentRequest(t, handler, refusal.method, refusal.path, refusal.body, token.Token)
		if response.Code != http.StatusForbidden || !strings.Contains(response.Body.String(), `"code":"NOT_AUTHOR"`) {
			t.Fatalf("alice's token %s: status=%d body=%s, want 403 NOT_AUTHOR",
				refusal.name, response.Code, response.Body.String())
		}
	}

	// A body that names a session is the caller saying it writes as that session, which the
	// person arm never overrides: the same token, naming one on alice's own comment, is the
	// session correcting her post and is refused as any other session is.
	named := agentRequest(t, handler, http.MethodPatch, "/api/v1/comments/"+comment.ID, map[string]any{
		"body": "her agent's edit, as itself", "actor": map[string]any{"kind": "session", "id": actingSession},
	}, token.Token)
	if named.Code != http.StatusForbidden || !strings.Contains(named.Body.String(), `"code":"NOT_AUTHOR"`) {
		t.Fatalf("alice's token edits her comment as a session it names: status=%d body=%s, want 403 NOT_AUTHOR",
			named.Code, named.Body.String())
	}

	deletedComment := agentRequest(t, handler, http.MethodDelete, "/api/v1/comments/"+comment.ID, map[string]any{}, token.Token)
	if deletedComment.Code != http.StatusOK {
		t.Fatalf("alice's token deletes her own comment: status=%d body=%s, want 200",
			deletedComment.Code, deletedComment.Body.String())
	}
	withdrawnComment := decodeBody[model.Comment](t, deletedComment)
	if withdrawnComment.Body != "" || withdrawnComment.DeletedAt == nil {
		t.Fatalf("the deleted comment = %#v, want the mark and no body", withdrawnComment)
	}
	requireDeletedByAlicesToken(t, "comment", withdrawnComment.DeletedBy, token)

	deletedMessage := agentRequest(t, handler, http.MethodDelete, "/api/v1/messages/"+message.ID, map[string]any{}, token.Token)
	if deletedMessage.Code != http.StatusOK {
		t.Fatalf("alice's token deletes her own message: status=%d body=%s, want 200",
			deletedMessage.Code, deletedMessage.Body.String())
	}
	withdrawnMessage := decodeBody[model.Message](t, deletedMessage)
	if withdrawnMessage.Body != "" || withdrawnMessage.DeletedAt == nil {
		t.Fatalf("the deleted message = %#v, want the mark and no body", withdrawnMessage)
	}
	requireDeletedByAlicesToken(t, "message", withdrawnMessage.DeletedBy, token)
}

// requireDeletedByAlicesToken fails t unless the delete was recorded as alice herself with the
// token that made it named beside her, which is what actingPerson records of a person's own
// credential and what tells this path apart from a session's own correction.
func requireDeletedByAlicesToken(t *testing.T, kind string, deletedBy *model.Actor, token agentTokenResponse) {
	t.Helper()
	if deletedBy == nil || deletedBy.Kind != "user" || deletedBy.ID != "alice" {
		t.Fatalf("the deleted %s was deleted by %#v, want alice herself", kind, deletedBy)
	}
	requireActingToken(t, "the deleted "+kind+"'s deleted_by", deletedBy.Act, "", token, model.ScopePerson)
}
