package api

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// readPostHistory is one history read as an authenticated person makes it.
func readPostHistory(t *testing.T, handler http.Handler, target string) postHistoryRead {
	t.Helper()
	response := dispatchRequest(t, handler, http.MethodGet, target, nil, "alice")
	if response.Code != http.StatusOK {
		t.Fatalf("read %s: status=%d body=%s", target, response.Code, response.Body.String())
	}
	return decodeBody[postHistoryRead](t, response)
}

// A post nobody has edited has an empty history, not an error: the read is the record of what
// replaced a body, and nothing has replaced one.
func TestPostHistoryOfAPostNobodyEditedIsEmptyAndNotAnError(t *testing.T) {
	handler := newTestHandler(t)
	issue := createInteractionIssue(t, handler, "TEST", "Untouched posts", "before")
	comment := createThreadComment(t, handler, issue.Key, map[string]any{"body": "stands as written"}, "alice")
	created := sessionRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/messages", map[string]any{
		"actor": sessionActor(), "body": "also stands as written",
	})
	if created.Code != http.StatusCreated {
		t.Fatalf("create message: status=%d body=%s", created.Code, created.Body.String())
	}
	message := decodeBody[model.Message](t, created)

	for name, target := range map[string]string{
		"comment": "/api/v1/comments/" + comment.ID + "/history",
		"message": "/api/v1/messages/" + message.ID + "/history",
	} {
		t.Run(name, func(t *testing.T) {
			history := readPostHistory(t, handler, target)
			if len(history.Entries) != 0 || history.DeletedAt != nil || history.DeletedBy != nil {
				t.Fatalf("history of an untouched %s = %#v, want no entries and no deleted mark", name, history)
			}
		})
	}
}

// Two edits leave two rows, newest first, each with the body it replaced, the actor that
// replaced it and the time; ?before= reads the page after a row, and anything that is not an
// RFC 3339 time is refused.
func TestCommentHistoryListsEachReplacedBodyNewestFirst(t *testing.T) {
	handler := newTestHandler(t)
	issue := createInteractionIssue(t, handler, "TEST", "Edited twice", "before")
	comment := createThreadComment(t, handler, issue.Key, map[string]any{
		"body": "first body", "actor": sessionActor(),
	}, "")
	for _, body := range []string{"second body", "third body"} {
		edited := sessionRequest(t, handler, http.MethodPatch, "/api/v1/comments/"+comment.ID, map[string]any{
			"body": body, "actor": sessionActor(),
		})
		if edited.Code != http.StatusOK {
			t.Fatalf("edit to %q: status=%d body=%s", body, edited.Code, edited.Body.String())
		}
	}

	history := readPostHistory(t, handler, "/api/v1/comments/"+comment.ID+"/history")
	if len(history.Entries) != 2 {
		t.Fatalf("history = %#v, want the two bodies the two edits replaced", history.Entries)
	}
	if history.Entries[0].Body != "second body" || history.Entries[1].Body != "first body" {
		t.Fatalf("history bodies = %q and %q, want \"second body\" then \"first body\": newest first",
			history.Entries[0].Body, history.Entries[1].Body)
	}
	author := model.Actor{Kind: "session", ID: "session-0123456789abcdef"}
	for index, entry := range history.Entries {
		if !entry.EditedBy.SameAs(author) {
			t.Fatalf("entry %d was replaced by %#v, want the editing session %#v", index, entry.EditedBy, author)
		}
		if entry.EditedAt.IsZero() || entry.EditedAt.After(time.Now().UTC().Add(time.Second)) {
			t.Fatalf("entry %d edited_at = %s, want the time of the edit", index, entry.EditedAt)
		}
	}
	if !history.Entries[0].EditedAt.After(history.Entries[1].EditedAt) {
		t.Fatalf("edited_at %s is not after %s: the list is not newest first",
			history.Entries[0].EditedAt, history.Entries[1].EditedAt)
	}

	// The page after the first row is the rows written before it.
	page := readPostHistory(t, handler, "/api/v1/comments/"+comment.ID+"/history?before="+
		history.Entries[0].EditedAt.Format(time.RFC3339Nano))
	if len(page.Entries) != 1 || page.Entries[0].Body != "first body" {
		t.Fatalf("?before= the newest row answered %#v, want the one row under it", page.Entries)
	}
	empty := readPostHistory(t, handler, "/api/v1/comments/"+comment.ID+"/history?before="+
		history.Entries[1].EditedAt.Format(time.RFC3339Nano))
	if len(empty.Entries) != 0 {
		t.Fatalf("?before= the oldest row answered %#v, want nothing under it", empty.Entries)
	}
	refused := dispatchRequest(t, handler, http.MethodGet,
		"/api/v1/comments/"+comment.ID+"/history?before=yesterday", nil, "alice")
	if refused.Code != http.StatusBadRequest {
		t.Fatalf("?before=yesterday: status=%d body=%s, want 400", refused.Code, refused.Body.String())
	}
}

// A history is read by whoever reads the post: any authenticated caller, the author or not. An
// agent that did not write the comment reads what replaced its bodies, as a person does.
func TestCommentHistoryIsReadByASessionThatIsNotItsAuthor(t *testing.T) {
	handler := newTestHandler(t)
	issue := createInteractionIssue(t, handler, "TEST", "Another session reads", "before")
	comment := createThreadComment(t, handler, issue.Key, map[string]any{
		"body": "the author's first body", "actor": sessionActor(),
	}, "")
	edited := sessionRequest(t, handler, http.MethodPatch, "/api/v1/comments/"+comment.ID, map[string]any{
		"body": "the author's second body", "actor": sessionActor(),
	})
	if edited.Code != http.StatusOK {
		t.Fatalf("the author edits: status=%d body=%s", edited.Code, edited.Body.String())
	}

	// A bearer caller that is not the author: no actor is named, because a read records none.
	read := sessionRequest(t, handler, http.MethodGet, "/api/v1/comments/"+comment.ID+"/history", nil)
	if read.Code != http.StatusOK {
		t.Fatalf("another session reads the history: status=%d body=%s", read.Code, read.Body.String())
	}
	history := decodeBody[postHistoryRead](t, read)
	if len(history.Entries) != 1 || history.Entries[0].Body != "the author's first body" {
		t.Fatalf("history read by another session = %#v, want the one replaced body", history.Entries)
	}
	if other := readPostHistory(t, handler, "/api/v1/comments/"+comment.ID+"/history"); len(other.Entries) != 1 {
		t.Fatalf("history read by another person = %#v, want the one replaced body", other.Entries)
	}
}

// A delete gives no part of the body on any read of the post, so the history it kept is served
// as the mark and no text at all.
func TestDeletedPostHistoryGivesTheMarkAndNoBody(t *testing.T) {
	handler := newTestHandler(t)
	issue := createInteractionIssue(t, handler, "TEST", "Deleted history", "before")
	comment := createThreadComment(t, handler, issue.Key, map[string]any{"body": "the first body"}, "alice")
	edited := dispatchRequest(t, handler, http.MethodPatch, "/api/v1/comments/"+comment.ID,
		map[string]any{"body": "the second body"}, "alice")
	if edited.Code != http.StatusOK {
		t.Fatalf("edit: status=%d body=%s", edited.Code, edited.Body.String())
	}
	deleted := dispatchRequest(t, handler, http.MethodDelete, "/api/v1/comments/"+comment.ID, nil, "alice")
	if deleted.Code != http.StatusOK {
		t.Fatalf("delete: status=%d body=%s", deleted.Code, deleted.Body.String())
	}

	history := readPostHistory(t, handler, "/api/v1/comments/"+comment.ID+"/history")
	if history.DeletedAt == nil || history.DeletedBy == nil || history.DeletedBy.ID != "alice" {
		t.Fatalf("deleted history mark = %#v / %#v, want the time and the deleting actor", history.DeletedAt, history.DeletedBy)
	}
	if len(history.Entries) != 2 {
		t.Fatalf("deleted history = %#v, want the edit's row and the delete's row", history.Entries)
	}
	for index, entry := range history.Entries {
		if entry.Body != "" {
			t.Fatalf("deleted history entry %d carries %q, want no body at all", index, entry.Body)
		}
	}
	if body := deleted.Body.String(); strings.Contains(body, "the second body") || strings.Contains(body, "the first body") {
		t.Fatalf("the delete answered %s, want no part of the body", body)
	}
}
