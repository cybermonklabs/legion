package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
)

// R3 of dispatch://INFRA-1523: a delete leaves no part of the post's body readable, and an insert
// suggestion's anchor quote is its own suggested words (the architect's ruling of 2026-11-20
// 00:47Z). TestDeletingAPendingInsertSuggestionClearsTheQuoteOfItsWords holds that for a pending
// insert whose mark still stands. A browser edit can take the inserted words out of the live
// document first, which orphans the mark: the delete's reject then finds no mark and answers no
// kind, and the row's anchor keeps the quote it was written with. That quote is still the
// suggestion's words, so the delete must empty it all the same, in the comment's read, in its
// stored comment.deleted, and in the event log's read of every event of the comment (test round at
// 869d340e).
func TestDeletingAnOrphanedInsertSuggestionLeavesNoneOfItsWords(t *testing.T) {
	const words = "zarquonorphaninsert"
	documentService, handler, database := browserDocumentService(t)
	issue := createInteractionIssue(t, handler, "TEST", "Delete an orphaned insert", "The "+words+" brown fox")
	writeBrowserMark(t, database, documentService, issue.PrimaryArtifactID, docs.MarkSpec{
		Kind:  docs.MarkSuggestion,
		ID:    "m-insert",
		Attrs: pmdoc.Attrs{"id": "m-insert", "by": "user:alice", "kind": "insert"},
	}, words+" ")
	comment := createThreadComment(t, handler, issue.Key, map[string]any{
		"body":       "Add this word.",
		"anchor":     map[string]any{"artifact": "spec", "mark_id": "m-insert"},
		"suggestion": map[string]string{"replace_with": words + " "},
	}, "alice")
	readRaw := func() string {
		t.Helper()
		response := dispatchRequest(t, handler, http.MethodGet, "/api/v1/comments/"+comment.ID, nil, "alice")
		if response.Code != http.StatusOK {
			t.Fatalf("read comment: status=%d body=%s", response.Code, response.Body.String())
		}
		return response.Body.String()
	}
	// The positive control: before the orphaning and the delete, the read quotes the words.
	if before := readRaw(); !strings.Contains(before, words) {
		t.Fatalf("the comment before the delete reads %s, want its anchor quoting %q", before, words)
	}
	// A browser edit takes the inserted words out of the live document, orphaning the mark.
	writeBrowserDocument(t, documentService, issue.PrimaryArtifactID, "The brown fox\n")

	beforeDelete := lastEventID(t, database)
	deleteThreadComment(t, handler, comment.ID, "alice")

	if after := readRaw(); strings.Contains(after, words) {
		t.Errorf("the read after the delete gives the orphaned insert suggestion's words %q: %s", words, after)
	}
	for _, event := range storedCommentEvents(t, database, comment.ID, beforeDelete) {
		if strings.Contains(event.raw, words) {
			t.Errorf("the stored %s the delete wrote gives the words %q: %s", event.eventType, words, event.raw)
		}
	}
	for _, event := range decodedIssueEvents(t, handler, issue.Key) {
		if event.Payload["id"] != comment.ID {
			continue
		}
		encoded, err := json.Marshal(event.Payload)
		if err != nil {
			t.Fatalf("encode the event log's %s: %v", event.Type, err)
		}
		if strings.Contains(string(encoded), words) {
			t.Errorf("the event log's %s gives the words %q: %s", event.Type, words, encoded)
		}
	}
}
