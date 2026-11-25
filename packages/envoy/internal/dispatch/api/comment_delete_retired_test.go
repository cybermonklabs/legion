package api

import (
	"context"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
)

// A retired document takes no new text, so a pending suggestion in it can never apply there and
// stays in its frozen text (docs.ErrRetiredDocumentKeepsText). The author's delete of the comment
// holding that suggestion stands: the comment reads deleted with no suggestion, and the retired
// document keeps its versions, its live history and its markdown, with no reject and no version
// written (comment_delete.go).
//
// An insert's quote is the comment's own words, so the delete empties it as the stored kind says;
// a replace's quote is document text and stays; and an insert of a row that stores no kind, whose
// quote is its replace_with, is emptied by that fallback. No anchor is recorded orphaned: the mark
// stays in the frozen text.
func TestTheAuthorDeletesACommentHoldingAPendingSuggestionOnARetiredDocument(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name   string
		insert bool
		legacy bool
		quote  string
	}{
		{name: "a pending insert suggestion", insert: true, quote: ""},
		{name: "a pending insert suggestion of a row with no stored kind", insert: true, legacy: true, quote: ""},
		{name: "a pending replace suggestion", quote: "quick"},
	} {
		t.Run(test.name, func(t *testing.T) {
			documentService, handler, database := browserDocumentService(t)
			issue := createInteractionIssue(t, handler, "TEST", "Delete "+test.name+" on a retired document", "A spec.\n")
			notes := uploadIssueDocument(t, handler, issue.Key, "notes.md", "The quick brown fox\n")
			input := map[string]any{
				"body": "Say this instead.", "anchor": map[string]any{"artifact": notes.ID, "quote": "quick"},
				"suggestion": map[string]string{"replace_with": "slow"},
			}
			if test.insert {
				writeBrowserMark(t, database, documentService, notes.ID, docs.MarkSpec{
					Kind:  docs.MarkSuggestion,
					ID:    "m-insert",
					Attrs: pmdoc.Attrs{"id": "m-insert", "by": "user:alice", "kind": "insert"},
				}, "quick ")
				input["anchor"] = map[string]any{"artifact": notes.ID, "mark_id": "m-insert"}
				input["suggestion"] = map[string]string{"replace_with": "quick "}
			}
			comment := createThreadComment(t, handler, issue.Key, input, "alice")
			if test.legacy {
				takeStoredKindOff(t, database, comment.ID)
			}
			if comment.Anchor == nil || comment.Suggestion == nil || comment.Anchor.ArtifactID != notes.ID {
				t.Fatalf("the comment was created as %#v, want a suggestion anchored in notes.md", comment)
			}
			retireTestDocument(t, database, notes.ID)
			written := readDocumentWrites(t, database, notes.ID)
			markdown, err := documentService.Text(context.Background(), notes.ID)
			if err != nil {
				t.Fatalf("read the retired notes.md before the delete: %v", err)
			}

			deleted := deleteThreadComment(t, handler, comment.ID, "alice")
			if deleted.Body != "" || deleted.DeletedAt == nil || deleted.Suggestion != nil {
				t.Fatalf("the delete answered %#v, want an empty body with the mark and no suggestion", deleted)
			}
			stored := readThreadComment(t, handler, comment.ID)
			if stored.Body != "" || stored.DeletedAt == nil || stored.Suggestion != nil {
				t.Fatalf("the comment reads back as %#v, want an empty body with the mark and no suggestion", stored)
			}
			if stored.Anchor == nil || stored.Anchor.ArtifactID != notes.ID || stored.Anchor.MarkID != comment.Anchor.MarkID ||
				stored.Anchor.Quote != test.quote || stored.Anchor.Orphaned {
				t.Errorf("the deleted comment's anchor = %#v, want the mark %q in notes.md, the quote %q and no orphaning",
					stored.Anchor, comment.Anchor.MarkID, test.quote)
			}

			unwritten(t, database, notes.ID, "the delete of "+test.name, written)
			if after, err := documentService.Text(context.Background(), notes.ID); err != nil || after != markdown {
				t.Errorf("the retired notes.md after the delete = %q (%v), want the %q it held", after, err, markdown)
			}
		})
	}
}
