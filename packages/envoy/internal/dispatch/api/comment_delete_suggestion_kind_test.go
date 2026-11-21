package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// The architect's ruling (b) of 2026-11-20 08:08Z on dispatch://INFRA-1523: a suggestion records
// the kind of its mark when it is made, in the row's suggestion column (storedSuggestion), and the
// delete of a suggestion whose mark a browser edit took out of the document reads that kind to
// tell an insert's quote, which is the comment's own words, from document text (deleteComment).
// A row made before the create stored the kind takes the rule the delete had before: its quote is
// emptied when it is its replace_with, byte for byte.

// suggestedWordsStem starts every word the insert suggestions below put in the document, as typed
// and as a browser retyped them, so a read that gives any of those words gives the stem.
const suggestedWordsStem = "zarquon"

// storedSuggestionKind is the kind the row of comment id holds in its suggestion column, and
// whether it holds one.
func storedSuggestionKind(t *testing.T, database *store.Store, id string) (string, bool) {
	t.Helper()
	var kind *string
	if err := database.Pool.QueryRow(context.Background(), `
		select suggestion->>'kind' from comments where id = $1
	`, id).Scan(&kind); err != nil {
		t.Fatalf("read the stored suggestion kind of %s: %v", id, err)
	}
	if kind == nil {
		return "", false
	}
	return *kind, true
}

// retypeSuggestedWords stands in for a browser editor that retypes part of the words a suggestion
// mark wraps: from becomes to in the text under markID, which keeps the mark.
func retypeSuggestedWords(t *testing.T, documentService *docs.Service, artifactID, markID, from, to string) {
	t.Helper()
	peer := connectBrowserPeer(t, documentService, artifactID)
	t.Cleanup(peer.close)
	peer.edit(t, func(tree *pmdoc.Node) error {
		retyped := false
		pmdoc.Walk(tree, func(node *pmdoc.Node) bool {
			for _, mark := range node.Marks {
				if mark.Type == string(docs.MarkSuggestion) && mark.Attrs["id"] == markID && strings.Contains(node.Text, from) {
					node.Text = strings.Replace(node.Text, from, to, 1)
					retyped = true
					return false
				}
			}
			return true
		})
		if !retyped {
			return fmt.Errorf("no text under the suggestion mark %q holds %q", markID, from)
		}
		return nil
	})
	peer.barrier(t)
}

// retypeSuggestedWordsAndRefresh has a browser edit change "suggested" to "revised" under the
// insert mark m-insert, and a server edit elsewhere refresh the document's anchors, and answers
// the quote the anchor then stores: "zarquonrevised ", no longer the suggestion's replace_with.
func retypeSuggestedWordsAndRefresh(t *testing.T, documentService *docs.Service, handler http.Handler, artifactID string) string {
	t.Helper()
	retypeSuggestedWords(t, documentService, artifactID, "m-insert", "suggested", "revised")
	if response := editDocument(t, handler, artifactID, map[string]any{
		"ops": []map[string]any{{"op": "replace", "find": "fox", "with": "dog"}},
	}); response.Code != http.StatusOK {
		t.Fatalf("the server edit that refreshes the anchors: status=%d body=%s", response.Code, response.Body.String())
	}
	return suggestedWordsStem + "revised "
}

// takeStoredKindOff takes the stored kind off the suggestion column of comment id, as a row made
// before the create stored one holds none.
func takeStoredKindOff(t *testing.T, database *store.Store, id string) {
	t.Helper()
	if _, err := database.Pool.Exec(context.Background(), `
		update comments set suggestion = suggestion - 'kind' where id = $1
	`, id); err != nil {
		t.Fatalf("take the stored kind off the row: %v", err)
	}
	if kind, stored := storedSuggestionKind(t, database, id); stored {
		t.Fatalf("the row still stores the kind %q", kind)
	}
}

// suggestionToDelete is a suggestion the rows below delete, as a fixture leaves it - an insert
// whose mark a browser edit took out of the document (orphanedInsertSuggestion), or a suggestion
// a reject decided (rejectedSuggestion): quote is its anchor's stored quote.
type suggestionToDelete struct {
	handler  http.Handler
	database *store.Store
	issueKey string
	comment  model.Comment
	quote    string
}

// orphanedInsertSuggestion makes an insert suggestion on a browser's insert mark around
// "zarquonsuggested ", and then a browser edit that takes the words and the mark out of the
// document. retyped has another browser edit change "suggested" to "revised" under the mark
// first, and a server edit elsewhere refresh the document's anchors, so the anchor's stored quote
// is "zarquonrevised " and no longer the suggestion's replace_with. legacy takes the stored kind
// off the row, as a row made before the create stored one holds none.
func orphanedInsertSuggestion(t *testing.T, retyped, legacy bool) suggestionToDelete {
	t.Helper()
	const typed = suggestedWordsStem + "suggested "
	documentService, handler, database := browserDocumentService(t)
	issue := createInteractionIssue(t, handler, "TEST", "Delete an orphaned insert", "The "+typed+"brown fox")
	writeBrowserMark(t, database, documentService, issue.PrimaryArtifactID, docs.MarkSpec{
		Kind:  docs.MarkSuggestion,
		ID:    "m-insert",
		Attrs: pmdoc.Attrs{"id": "m-insert", "by": "user:alice", "kind": "insert"},
	}, typed)
	comment := createThreadComment(t, handler, issue.Key, map[string]any{
		"body":       "Add this word.",
		"anchor":     map[string]any{"artifact": "spec", "mark_id": "m-insert"},
		"suggestion": map[string]string{"replace_with": typed},
	}, "alice")
	quote := typed
	if retyped {
		quote = retypeSuggestedWordsAndRefresh(t, documentService, handler, issue.PrimaryArtifactID)
	}
	// The positive control: the anchor quotes the words on the standing mark, and a retyped
	// quote is no longer the suggestion's replace_with.
	if read := readThreadComment(t, handler, comment.ID); read.Anchor == nil || read.Anchor.Quote != quote || read.Anchor.Orphaned ||
		read.Suggestion == nil || (read.Suggestion.ReplaceWith == read.Anchor.Quote) == retyped {
		t.Fatalf("the comment before the orphaning = anchor %#v, suggestion %#v; want the anchor quoting %q on the standing mark",
			read.Anchor, read.Suggestion, quote)
	}
	if legacy {
		takeStoredKindOff(t, database, comment.ID)
	}
	writeBrowserDocument(t, documentService, issue.PrimaryArtifactID, "The brown dog\n")
	return suggestionToDelete{handler: handler, database: database, issueKey: issue.Key, comment: comment, quote: quote}
}

// deleteAndFindTheWords deletes the suggestion, answers the comment's read after the delete, and
// names each read that gives any of the suggestion's words: the delete's answer, the comment's
// read, each event the delete stored, and the event log's read of each event of the comment.
func (f suggestionToDelete) deleteAndFindTheWords(t *testing.T) (model.Comment, []string) {
	t.Helper()
	var giving []string
	gives := func(what, text string) {
		if strings.Contains(text, suggestedWordsStem) {
			giving = append(giving, what+": "+text)
		}
	}
	beforeDelete := lastEventID(t, f.database)
	answer := dispatchRequest(t, f.handler, http.MethodDelete, "/api/v1/comments/"+f.comment.ID, nil, "alice")
	if answer.Code != http.StatusOK {
		t.Fatalf("delete the suggestion: status=%d body=%s", answer.Code, answer.Body.String())
	}
	gives("the delete's answer", answer.Body.String())
	read := dispatchRequest(t, f.handler, http.MethodGet, "/api/v1/comments/"+f.comment.ID, nil, "alice")
	if read.Code != http.StatusOK {
		t.Fatalf("read the comment: status=%d body=%s", read.Code, read.Body.String())
	}
	gives("the read after the delete", read.Body.String())
	deletes := 0
	for _, event := range storedCommentEvents(t, f.database, f.comment.ID, beforeDelete) {
		gives("the stored "+event.eventType+" the delete wrote", event.raw)
		if event.eventType == "comment.deleted" {
			deletes++
		}
	}
	if deletes != 1 {
		t.Fatalf("the delete stored %d comment.deleted events, want one", deletes)
	}
	for _, event := range decodedIssueEvents(t, f.handler, f.issueKey) {
		if event.Payload["id"] != f.comment.ID {
			continue
		}
		encoded, err := json.Marshal(event.Payload)
		if err != nil {
			t.Fatalf("encode the event log's %s: %v", event.Type, err)
		}
		gives("the event log's "+event.Type, string(encoded))
	}
	return decodeBody[struct {
		Comment model.Comment `json:"comment"`
	}](t, read).Comment, giving
}

// Row 1 of the ruling. A browser edit changed some of an insert's words under its mark, an anchor
// refresh moved the anchor's quote to the words as retyped, and a later browser edit took the
// words and the mark out of the document. The quote is no longer the suggestion's replace_with, so
// no fact of the row's text tells it from document text; the kind the create stored says it is an
// insert's, and the delete empties it.
func TestDeletingAnOrphanedInsertSuggestionABrowserRetypedLeavesNoneOfItsWords(t *testing.T) {
	fixture := orphanedInsertSuggestion(t, true, false)

	read, giving := fixture.deleteAndFindTheWords(t)

	if read.Anchor == nil || read.Anchor.Quote != "" || read.Anchor.MarkID != "m-insert" || !read.Anchor.Orphaned {
		t.Errorf("the read after the delete gives the anchor %#v, want mark m-insert, orphaned, with an empty quote", read.Anchor)
	}
	for _, what := range giving {
		t.Errorf("%s gives the deleted insert suggestion's words", what)
	}
}

// Row 2 of the ruling. A replace suggestion's quote is document text, which stays in the document
// whatever the delete does, so the delete of one whose mark a browser edit took out keeps it. Its
// replace_with is the very text it quotes: that is the one replace whose quote the rule for a row
// with no stored kind empties, so only the stored kind keeps it.
func TestDeletingAnOrphanedReplaceSuggestionKeepsItsQuote(t *testing.T) {
	documentService, handler, database := browserDocumentService(t)
	issue := createInteractionIssue(t, handler, "TEST", "Delete an orphaned replace", "The quick brown fox")
	comment := createThreadComment(t, handler, issue.Key, map[string]any{
		"body":       "Keep this word.",
		"anchor":     map[string]any{"artifact": "spec", "quote": "quick"},
		"suggestion": map[string]string{"replace_with": "quick"},
	}, "alice")
	// A browser edit takes the mark out of the document and leaves its text.
	writeBrowserDocument(t, documentService, issue.PrimaryArtifactID, "The quick brown dog\n")
	if _, err := documentService.VerifyMark(context.Background(), issue.PrimaryArtifactID, docs.MarkSuggestion, comment.Anchor.MarkID); !errors.Is(err, docs.ErrAnchorMissing) {
		t.Fatalf("the suggestion's mark after the browser edit = %v, want it gone", err)
	}

	beforeDelete := lastEventID(t, database)
	answer := deleteThreadComment(t, handler, comment.ID, "alice")

	if answer.Anchor == nil || answer.Anchor.Quote != "quick" {
		t.Errorf("the delete's answer gives the anchor %#v, want the quote %q", answer.Anchor, "quick")
	}
	if read := readThreadComment(t, handler, comment.ID); read.Anchor == nil || read.Anchor.Quote != "quick" || !read.Anchor.Orphaned {
		t.Errorf("the read after the delete gives the anchor %#v, want the quote %q, orphaned", read.Anchor, "quick")
	}
	for _, event := range storedCommentEvents(t, database, comment.ID, beforeDelete) {
		if event.eventType != "comment.deleted" {
			continue
		}
		if quote, ok := payloadAnchorQuote(event.payload); !ok || quote != "quick" {
			t.Errorf("the stored comment.deleted anchor quote = %q (carried %t), want %q", quote, ok, "quick")
		}
	}
}

// Row 3 of the ruling. A row made before the create stored the kind holds none, and the delete of
// its orphaned suggestion takes the rule it had before: the quote goes when it is the suggestion's
// replace_with, byte for byte, and stays otherwise, as the words of an insert a browser retyped
// stay, which no fact of such a row tells from document text.
func TestDeletingAnOrphanedSuggestionOfARowWithNoStoredKindComparesItsQuote(t *testing.T) {
	for _, test := range []struct {
		name    string
		retyped bool
	}{
		{name: "a quote that is the replace_with"},
		{name: "a quote a browser retyped", retyped: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := orphanedInsertSuggestion(t, test.retyped, true)

			read, giving := fixture.deleteAndFindTheWords(t)

			want := ""
			if test.retyped {
				want = fixture.quote
			}
			if read.Anchor == nil || read.Anchor.Quote != want {
				t.Fatalf("the read after the delete gives the anchor %#v, want the quote %q", read.Anchor, want)
			}
			if !test.retyped {
				for _, what := range giving {
					t.Errorf("%s gives the deleted insert suggestion's words", what)
				}
			}
		})
	}
}

// The create stores the kind of the suggestion's mark - a browser mark's own, and replace for the
// mark a quote anchor writes - and no answer carries it: every read decodes the column into
// model.Suggestion, which has no kind, and so does the comment.created event the create wrote.
func TestASuggestionStoresTheKindOfItsMarkAndNoAnswerCarriesIt(t *testing.T) {
	for _, test := range []struct {
		name, kind string
		mark       bool
	}{
		{name: "an insert mark", kind: "insert", mark: true},
		{name: "a delete mark", kind: "delete", mark: true},
		{name: "a quote", kind: "replace"},
	} {
		t.Run(test.name, func(t *testing.T) {
			documentService, handler, database := browserDocumentService(t)
			issue := createInteractionIssue(t, handler, "TEST", "Store "+test.name, "The quick brown fox")
			anchor := map[string]any{"artifact": "spec", "quote": "quick"}
			if test.mark {
				writeBrowserMark(t, database, documentService, issue.PrimaryArtifactID, docs.MarkSpec{
					Kind:  docs.MarkSuggestion,
					ID:    "m-browser",
					Attrs: pmdoc.Attrs{"id": "m-browser", "by": "user:alice", "kind": test.kind},
				}, "quick")
				anchor = map[string]any{"artifact": "spec", "mark_id": "m-browser"}
			}
			comment := createThreadComment(t, handler, issue.Key, map[string]any{
				"body": "Say this instead.", "anchor": anchor, "suggestion": map[string]string{"replace_with": "slow"},
			}, "alice")

			if kind, stored := storedSuggestionKind(t, database, comment.ID); !stored || kind != test.kind {
				t.Fatalf("the row stores the kind %q (stored %t), want %q", kind, stored, test.kind)
			}
			response := dispatchRequest(t, handler, http.MethodGet, "/api/v1/comments/"+comment.ID, nil, "alice")
			if response.Code != http.StatusOK {
				t.Fatalf("read comment: status=%d body=%s", response.Code, response.Body.String())
			}
			// The read wraps the comment in "comment" (commentRead), beside its replies.
			read := decodeBody[struct {
				Comment struct {
					Suggestion map[string]any `json:"suggestion"`
				} `json:"comment"`
			}](t, response)
			suggestion := read.Comment.Suggestion
			if suggestion == nil || suggestion["replace_with"] != "slow" {
				t.Fatalf("the read gives the suggestion %#v, want replace_with slow: %s", suggestion, response.Body.String())
			}
			if kind, carried := suggestion["kind"]; carried {
				t.Errorf("the read's suggestion carries the kind %v: %#v", kind, suggestion)
			}
			list := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+issue.Key+"/comments", nil, "alice")
			if list.Code != http.StatusOK || !strings.Contains(list.Body.String(), comment.ID) {
				t.Fatalf("list comments: status=%d body=%s", list.Code, list.Body.String())
			}
			if stored := `"kind":"` + test.kind + `"`; strings.Contains(list.Body.String(), stored) {
				t.Errorf("the comment list carries %s: %s", stored, list.Body.String())
			}
			created := 0
			for _, event := range storedCommentEvents(t, database, comment.ID, 0) {
				if event.eventType != "comment.created" {
					continue
				}
				created++
				suggestion, _ := event.payload["suggestion"].(map[string]any)
				if suggestion == nil {
					t.Fatalf("the stored comment.created carries no suggestion: %s", event.raw)
				}
				if kind, carried := suggestion["kind"]; carried {
					t.Errorf("the stored comment.created suggestion carries the kind %v: %s", kind, event.raw)
				}
			}
			if created != 1 {
				t.Fatalf("the create stored %d comment.created events, want one", created)
			}
		})
	}
}

// The architect's ruling of 2026-11-20 16:33Z on dispatch://INFRA-1523: only an accepted insert's
// words became document text. A pending or a rejected insert's words were only ever the post's
// own, so the delete empties a suggestion's quote unless an accept decided it, by the kind the
// create stored, which the accept and the reject keep (commentAction), and on a row with no stored
// kind by its quote against its replace_with. A rejected insert's words left the document with
// its reject, so the delete rejects nothing again.

// rejectedSuggestion makes a suggestion of kind, "insert" or "replace", and rejects it. An insert
// is made on a browser's insert mark around "zarquonsuggested ", whose words and mark the reject
// takes out of the document; retyped has a browser edit change "suggested" to "revised" under the
// mark first, and a server edit elsewhere refresh the document's anchors, so the anchor's stored
// quote is "zarquonrevised " and no longer the suggestion's replace_with. A replace quotes
// "quick", which is its replace_with too, and its reject leaves that text in the document. legacy
// takes the stored kind off the row after the reject, as a row made before the create stored one
// holds none.
func rejectedSuggestion(t *testing.T, kind string, retyped, legacy bool) suggestionToDelete {
	t.Helper()
	const typed = suggestedWordsStem + "suggested "
	documentService, handler, database := browserDocumentService(t)
	spec, quote := "The quick brown fox", "quick"
	input := map[string]any{
		"body":       "Say this instead.",
		"anchor":     map[string]any{"artifact": "spec", "quote": "quick"},
		"suggestion": map[string]string{"replace_with": "quick"},
	}
	if kind == "insert" {
		spec, quote = "The "+typed+"brown fox", typed
		input["anchor"] = map[string]any{"artifact": "spec", "mark_id": "m-insert"}
		input["suggestion"] = map[string]string{"replace_with": typed}
	}
	issue := createInteractionIssue(t, handler, "TEST", "Delete a rejected "+kind, spec)
	if kind == "insert" {
		writeBrowserMark(t, database, documentService, issue.PrimaryArtifactID, docs.MarkSpec{
			Kind:  docs.MarkSuggestion,
			ID:    "m-insert",
			Attrs: pmdoc.Attrs{"id": "m-insert", "by": "user:alice", "kind": "insert"},
		}, typed)
	}
	comment := createThreadComment(t, handler, issue.Key, input, "alice")
	if retyped {
		quote = retypeSuggestedWordsAndRefresh(t, documentService, handler, issue.PrimaryArtifactID)
	}

	rejected := dispatchRequest(t, handler, http.MethodPost, "/api/v1/comments/"+comment.ID+"/reject", map[string]any{}, "alice")
	if rejected.Code != http.StatusOK || !strings.Contains(rejected.Body.String(), `"accepted":false`) {
		t.Fatalf("reject the suggestion: status=%d body=%s", rejected.Code, rejected.Body.String())
	}
	// The reject keeps the kind the create stored.
	if stored, held := storedSuggestionKind(t, database, comment.ID); !held || stored != kind {
		t.Fatalf("the row stores the kind %q after the reject (stored %t), want %q", stored, held, kind)
	}
	// An insert's reject took its words out of the document, and a replace's left its quote there.
	if markdown, err := documentService.Text(context.Background(), issue.PrimaryArtifactID); err != nil ||
		strings.Contains(markdown, suggestedWordsStem) || !strings.Contains(markdown, "brown") {
		t.Fatalf("the document after the reject = %q (%v), want it holding none of %q", markdown, err, suggestedWordsStem)
	}
	// The positive control: after the reject and before the delete, the read still quotes the
	// words, and a retyped quote is no longer the suggestion's replace_with.
	if read := readThreadComment(t, handler, comment.ID); read.Anchor == nil || read.Anchor.Quote != quote ||
		read.Suggestion == nil || read.Suggestion.Accepted == nil || *read.Suggestion.Accepted ||
		(read.Suggestion.ReplaceWith == read.Anchor.Quote) == retyped {
		t.Fatalf("the comment after the reject = anchor %#v, suggestion %#v; want the rejected suggestion quoting %q",
			read.Anchor, read.Suggestion, quote)
	}
	if legacy {
		takeStoredKindOff(t, database, comment.ID)
	}
	return suggestionToDelete{handler: handler, database: database, issueKey: issue.Key, comment: comment, quote: quote}
}

// Row 1 of the ruling of 16:33Z. A browser edit changed some of an insert's words under its mark,
// an anchor refresh moved the anchor's quote to the words as retyped, and a reject took the words
// and the mark out of the document. The quote is no longer the suggestion's replace_with, so only
// the kind the create stored, which the reject kept, says it is the comment's own words, and the
// delete empties it: neither the delete's answer, the comment's read, the comment.deleted it
// stores nor any event of the comment the event log reads, its creation's and its reject's
// included, gives any of them.
func TestDeletingARejectedInsertSuggestionLeavesNoneOfItsWords(t *testing.T) {
	fixture := rejectedSuggestion(t, "insert", true, false)

	read, giving := fixture.deleteAndFindTheWords(t)

	if read.Anchor == nil || read.Anchor.Quote != "" || read.Anchor.MarkID != "m-insert" {
		t.Errorf("the read after the delete gives the anchor %#v, want mark m-insert with an empty quote", read.Anchor)
	}
	for _, what := range giving {
		t.Errorf("%s gives the deleted insert suggestion's words", what)
	}
}

// Row 2 of the ruling of 16:33Z. A replace suggestion's quote is document text, which its reject
// left in the document, so the delete of a rejected replace keeps it. Its replace_with is the very
// text it quotes, the one quote the rule for a row with no stored kind empties, so only the kind
// the reject kept keeps it.
func TestDeletingARejectedReplaceSuggestionKeepsItsQuote(t *testing.T) {
	fixture := rejectedSuggestion(t, "replace", false, false)
	beforeDelete := lastEventID(t, fixture.database)

	answer := deleteThreadComment(t, fixture.handler, fixture.comment.ID, "alice")

	if answer.Anchor == nil || answer.Anchor.Quote != "quick" {
		t.Errorf("the delete's answer gives the anchor %#v, want the quote %q", answer.Anchor, "quick")
	}
	if read := readThreadComment(t, fixture.handler, fixture.comment.ID); read.Anchor == nil || read.Anchor.Quote != "quick" {
		t.Errorf("the read after the delete gives the anchor %#v, want the quote %q", read.Anchor, "quick")
	}
	deletes := 0
	for _, event := range storedCommentEvents(t, fixture.database, fixture.comment.ID, beforeDelete) {
		if event.eventType != "comment.deleted" {
			continue
		}
		deletes++
		if quote, ok := payloadAnchorQuote(event.payload); !ok || quote != "quick" {
			t.Errorf("the stored comment.deleted anchor quote = %q (carried %t), want %q", quote, ok, "quick")
		}
	}
	if deletes != 1 {
		t.Errorf("the delete stored %d comment.deleted events, want one", deletes)
	}
}

// Row 3 of the ruling of 16:33Z. A row made before the create stored the kind holds none, and the
// delete of its rejected suggestion takes the rule the delete of an orphaned one takes: the quote
// goes when it is the suggestion's replace_with, byte for byte, as the words of an insert no
// browser retyped are.
func TestDeletingARejectedSuggestionOfARowWithNoStoredKindComparesItsQuote(t *testing.T) {
	fixture := rejectedSuggestion(t, "insert", false, true)

	read, giving := fixture.deleteAndFindTheWords(t)

	if read.Anchor == nil || read.Anchor.Quote != "" {
		t.Fatalf("the read after the delete gives the anchor %#v, want an empty quote", read.Anchor)
	}
	for _, what := range giving {
		t.Errorf("%s gives the deleted insert suggestion's words", what)
	}
}

// An accept and a reject write the suggestion column back as the create wrote it, with the kind
// it stored, which the delete of a rejected suggestion reads (deleteComment), and no answer of a
// decided suggestion carries that kind, as no answer of a pending one does
// (TestASuggestionStoresTheKindOfItsMarkAndNoAnswerCarriesIt): neither the decision's own answer,
// the read, the comment list nor the stored suggestion.accepted or suggestion.rejected.
func TestADecidedSuggestionKeepsTheKindOfItsMarkAndNoAnswerCarriesIt(t *testing.T) {
	for _, action := range []string{"accept", "reject"} {
		t.Run(action, func(t *testing.T) {
			documentService, handler, database := browserDocumentService(t)
			issue := createInteractionIssue(t, handler, "TEST", "Decide a suggestion by "+action, "The quick brown fox")
			writeBrowserMark(t, database, documentService, issue.PrimaryArtifactID, docs.MarkSpec{
				Kind:  docs.MarkSuggestion,
				ID:    "m-browser",
				Attrs: pmdoc.Attrs{"id": "m-browser", "by": "user:alice", "kind": "insert"},
			}, "quick")
			comment := createThreadComment(t, handler, issue.Key, map[string]any{
				"body": "Say this instead.", "anchor": map[string]any{"artifact": "spec", "mark_id": "m-browser"},
				"suggestion": map[string]string{"replace_with": "quick"},
			}, "alice")

			decided := dispatchRequest(t, handler, http.MethodPost, "/api/v1/comments/"+comment.ID+"/"+action, map[string]any{}, "alice")
			if decided.Code != http.StatusOK {
				t.Fatalf("%s the suggestion: status=%d body=%s", action, decided.Code, decided.Body.String())
			}

			if kind, stored := storedSuggestionKind(t, database, comment.ID); !stored || kind != "insert" {
				t.Errorf("the row stores the kind %q after the %s (stored %t), want insert", kind, action, stored)
			}
			// The decision answers the comment itself (newShapedComment), and the read wraps it in
			// "comment" (commentRead), beside its replies.
			answered := decodeBody[struct {
				Suggestion map[string]any `json:"suggestion"`
			}](t, decided).Suggestion
			response := dispatchRequest(t, handler, http.MethodGet, "/api/v1/comments/"+comment.ID, nil, "alice")
			if response.Code != http.StatusOK {
				t.Fatalf("read comment: status=%d body=%s", response.Code, response.Body.String())
			}
			read := decodeBody[struct {
				Comment struct {
					Suggestion map[string]any `json:"suggestion"`
				} `json:"comment"`
			}](t, response).Comment.Suggestion
			for what, suggestion := range map[string]map[string]any{"the " + action + "'s answer": answered, "the read": read} {
				if suggestion == nil || suggestion["accepted"] != (action == "accept") {
					t.Errorf("%s gives the suggestion %#v, want it decided by the %s", what, suggestion, action)
				}
				if kind, carried := suggestion["kind"]; carried {
					t.Errorf("%s's suggestion carries the kind %v: %#v", what, kind, suggestion)
				}
			}
			list := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+issue.Key+"/comments", nil, "alice")
			if list.Code != http.StatusOK || !strings.Contains(list.Body.String(), comment.ID) {
				t.Fatalf("list comments: status=%d body=%s", list.Code, list.Body.String())
			}
			if strings.Contains(list.Body.String(), `"kind":"insert"`) {
				t.Errorf("the comment list carries the stored kind: %s", list.Body.String())
			}
			decisions := 0
			for _, event := range storedCommentEvents(t, database, comment.ID, 0) {
				if event.eventType != "suggestion.accepted" && event.eventType != "suggestion.rejected" {
					continue
				}
				decisions++
				suggestion, _ := event.payload["suggestion"].(map[string]any)
				if suggestion == nil {
					t.Fatalf("the stored %s carries no suggestion: %s", event.eventType, event.raw)
				}
				if kind, carried := suggestion["kind"]; carried {
					t.Errorf("the stored %s suggestion carries the kind %v: %s", event.eventType, kind, event.raw)
				}
			}
			if decisions != 1 {
				t.Fatalf("the %s stored %d decision events, want one", action, decisions)
			}
		})
	}
}
