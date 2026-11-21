package api

import (
	"net/http"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/textversions"
)

// The keyword table's rows of a document version holding a pending insert's words, once the
// insert's comment is deleted (R3 of dispatch://INFRA-1523, and the architect's ruling of
// 2026-11-21 12:20Z): the words were the comment's own and never document text, and a vector
// cannot be masked at a read, so no keyword row of a past version of the issue matches them once
// the delete commits, whether the fill wrote the row before the delete (the delete rewrites it,
// textversions.MaskDeletedInsert) or after it (the fill masks it, insertVersions). An accepted
// insert's words are document text and stay.

// keywordInsert is an issue whose primary document a browser gave insertedWords, which no
// version of it held before, marked as alice's pending insert suggestion, and the version named
// while the insert stood pending, which holds them.
type keywordInsert struct {
	handler              http.Handler
	database             *store.Store
	issueKey, artifactID string
	insert               model.Comment
	pending              int
}

// newKeywordInsert makes keywordInsert's issue, and fails t unless version 1 of its document holds
// no word of the insert and the pending version holds them and stores the insert's tree.
func newKeywordInsert(t *testing.T) keywordInsert {
	t.Helper()
	documentService, handler, database, _ := browserDocumentServer(t)
	issue := createInteractionIssue(t, handler, "TEST", "An insert's words", "The brown fox jumps")
	writeBrowserDocument(t, documentService, issue.PrimaryArtifactID, "The "+insertedWords+" brown fox jumps\n")
	writeBrowserMark(t, database, documentService, issue.PrimaryArtifactID, docs.MarkSpec{
		Kind:  docs.MarkSuggestion,
		ID:    "m-insert",
		Attrs: pmdoc.Attrs{"id": "m-insert", "by": "user:alice", "kind": "insert"},
	}, insertedWords+" ")
	f := keywordInsert{
		handler: handler, database: database, issueKey: issue.Key, artifactID: issue.PrimaryArtifactID,
		insert: createThreadComment(t, handler, issue.Key, map[string]any{
			"body":       "Add this word.",
			"anchor":     map[string]any{"artifact": "spec", "mark_id": "m-insert"},
			"suggestion": map[string]string{"replace_with": insertedWords + " "},
		}, "alice"),
	}
	f.pending = nameVersion(t, handler, f.artifactID, "With the insert pending")
	var first, pending string
	if err := database.Pool.QueryRow(t.Context(), `
		select (select markdown from artifact_versions where artifact_id = $1::uuid and number = 1),
		       (select markdown from artifact_versions where artifact_id = $1::uuid and number = $2)
	`, f.artifactID, f.pending).Scan(&first, &pending); err != nil {
		t.Fatalf("read the document's versions: %v", err)
	}
	if strings.Contains(first, insertedWords) || !strings.Contains(pending, insertedWords) {
		t.Fatalf("version 1 reads %q and the pending version %d %q; want the insert's words %q in the second alone", first, f.pending, pending, insertedWords)
	}
	if tree := storedInsertTree(t, database, f.artifactID, f.pending); tree == nil || !strings.Contains(*tree, `"m-insert"`) {
		t.Fatalf("the pending version %d stores insert tree %v, want one naming the mark m-insert", f.pending, tree)
	}
	return f
}

// fill runs the keyword fill up to the log's horizon (textversions.FillToHorizon).
func (f keywordInsert) fill(t *testing.T) {
	t.Helper()
	if err := textversions.FillToHorizon(t.Context(), f.database, events.NewHorizon(f.database.Pool)); err != nil {
		t.Fatalf("fill the keyword table: %v", err)
	}
}

// rows counts the issue's keyword rows that name version number of its primary document, every
// row of the issue for number 0, and of those the ones whose vector matches the insert's words.
func (f keywordInsert) rows(t *testing.T, number int) (named, matching int) {
	t.Helper()
	if err := f.database.Pool.QueryRow(t.Context(), `
		select count(*), count(*) filter (where search @@ plainto_tsquery('english', $3))
		  from search_text_versions
		 where kind = 'issue' and id = $1 and ($2 = 0 or spec_version = $2)
	`, f.issueKey, number, insertedWords).Scan(&named, &matching); err != nil {
		t.Fatalf("count the issue's keyword rows: %v", err)
	}
	return named, matching
}

// TestTheDeleteOfAPendingInsertTakesItsWordsOutOfTheKeywordRows fills the keyword table while the
// insert stands pending, so the row of the version named then matches its words, and then
// deletes its comment: once the delete commits, no keyword row of the issue matches them, the row
// of that version included, which the delete rewrote.
func TestTheDeleteOfAPendingInsertTakesItsWordsOutOfTheKeywordRows(t *testing.T) {
	f := newKeywordInsert(t)
	f.fill(t)
	if named, matching := f.rows(t, f.pending); named == 0 || matching == 0 {
		t.Fatalf("before the delete the issue has %d keyword rows of version %d, %d matching %q; want one matching, so the check after the delete can fail",
			named, f.pending, matching, insertedWords)
	}

	deleteThreadComment(t, f.handler, f.insert.ID, "alice")
	if named, matching := f.rows(t, 0); named == 0 || matching != 0 {
		t.Errorf("after the delete, %d of the issue's %d keyword rows match the deleted insert's words %q; want none", matching, named, insertedWords)
	}
	f.fill(t)
	if named, matching := f.rows(t, 0); matching != 0 {
		t.Errorf("after the delete and the fill of its events, %d of the issue's %d keyword rows match the deleted insert's words %q; want none", matching, named, insertedWords)
	}
}

// TestAKeywordRowTheFillWritesAfterAPendingInsertsDeleteHoldsNoWordOfIt deletes the comment of a
// pending insert before the fill has read the version named while it stood, as a fill that lags
// the log does: the row the fill writes of that version after the delete matches none of the
// insert's words, nor does any other row of the issue.
func TestAKeywordRowTheFillWritesAfterAPendingInsertsDeleteHoldsNoWordOfIt(t *testing.T) {
	f := newKeywordInsert(t)
	if named, _ := f.rows(t, 0); named != 0 {
		t.Fatalf("the issue has %d keyword rows before any fill, want none", named)
	}
	deleteThreadComment(t, f.handler, f.insert.ID, "alice")
	f.fill(t)
	if named, _ := f.rows(t, f.pending); named == 0 {
		t.Fatalf("the fill wrote no keyword row of version %d, the one holding the insert's words, so the check of its words cannot fail", f.pending)
	}
	if named, matching := f.rows(t, 0); matching != 0 {
		t.Errorf("%d of the issue's %d keyword rows, written after the delete, match the deleted insert's words %q; want none", matching, named, insertedWords)
	}
}

// TestAnAcceptedInsertsWordsStayInTheKeywordRowsAfterItsDelete accepts the insert, which makes
// its words document text, and then deletes its comment: the row of the version named while it
// stood pending still matches them, as every read of that version still gives them.
func TestAnAcceptedInsertsWordsStayInTheKeywordRowsAfterItsDelete(t *testing.T) {
	f := newKeywordInsert(t)
	f.fill(t)
	if accepted := dispatchRequest(t, f.handler, http.MethodPost, "/api/v1/comments/"+f.insert.ID+"/accept", map[string]any{}, "alice"); accepted.Code != http.StatusOK {
		t.Fatalf("accept the insert: status=%d body=%s", accepted.Code, accepted.Body.String())
	}
	deleteThreadComment(t, f.handler, f.insert.ID, "alice")
	f.fill(t)
	if named, matching := f.rows(t, f.pending); named == 0 || matching == 0 {
		t.Errorf("after the accepted insert's delete, %d of the issue's %d keyword rows of version %d match its words %q; want the row matching them still",
			matching, named, f.pending, insertedWords)
	}
}
