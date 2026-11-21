package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/reearth/ygo/crdt"

	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// The architect's ruling of 2026-11-20 16:33Z on dispatch://INFRA-1523: R3's one exception is a
// preview of a post frozen inside another post's event payload. Another comment's or ask's anchor
// quote is no payload: every read of that comment or ask gives it live. An anchor can quote words
// a pending insert suggestion put in the document, since an anchor's mark may overlap a
// suggestion's, and the insert's reject takes those words out. So the delete of a suggestion whose
// insert no accept made document text clears the quote of any anchor that holds those words, open
// or closed: an anchor whose mark lay wholly on them is emptied and stands orphaned, and one whose
// mark also wrapped document text keeps that text alone. The anchors are named by their marks, as
// the removal finds them on the inserted words (docs.TakenAnchorMarks), so an anchor whose mark an
// unsettled browser edit took out elsewhere keeps its quote. An anchor is a derived field, which an
// anchor refresh rewrites whenever the document changes, so this is no edit of another author's
// post. An anchor's events written before the delete keep the quote they froze; the
// comment.anchor_refreshed or ask.anchor_refreshed the delete writes is a correction event, and
// carries none of the words.

// insertedWords is the text the insert suggestion below put in the document, which the anchors
// beside it quote.
const insertedWords = "zarquoninserted"

// anchorsOnAnInsert is a document holding an insert suggestion's words and anchors on them, as
// newAnchorsOnAnInsert leaves it.
type anchorsOnAnInsert struct {
	documentService *docs.Service
	handler         http.Handler
	database        *store.Store
	// server is the server handler serves, whose fields only a test sets
	// (server.deleteLeavesSettlement).
	server     *server
	issueKey   string
	artifactID string
	// insert is alice's insert suggestion, on a browser's insert mark around insertedWords.
	insert model.Comment
	// onWords and resolved are bob's comments anchored on insertedWords, the second resolved, and
	// askID and closedAskID a session's asks anchored on them, the second answered: each mark lies
	// wholly on the inserted words.
	onWords, resolved  model.Comment
	askID, closedAskID string
	// straddling and settled are bob's comments anchored on insertedWords and the document text
	// after them, the second resolved: each mark wraps "brown" besides the inserted words.
	straddling, settled model.Comment
	// outside is bob's comment on "fox", outside the words: the control. browserEdited is bob's
	// comment on "jumps", whose mark a browser edit no settlement refreshed takes out
	// (takeOutMarkInBrowser).
	outside, browserEdited model.Comment
}

// newAnchorsOnAnInsert makes the document of anchorsOnAnInsert, and fails t unless every anchor
// quotes what it was made on, standing, before anything is decided or deleted.
func newAnchorsOnAnInsert(t *testing.T) anchorsOnAnInsert {
	t.Helper()
	documentService, handler, database, served := browserDocumentServer(t)
	issue := createInteractionIssue(t, handler, "TEST", "Anchors on an insert's words", "The "+insertedWords+" brown fox jumps")
	writeBrowserMark(t, database, documentService, issue.PrimaryArtifactID, docs.MarkSpec{
		Kind:  docs.MarkSuggestion,
		ID:    "m-insert",
		Attrs: pmdoc.Attrs{"id": "m-insert", "by": "user:alice", "kind": "insert"},
	}, insertedWords+" ")
	comment := func(body, quote string) model.Comment {
		t.Helper()
		return createThreadComment(t, handler, issue.Key, map[string]any{
			"body": body, "anchor": map[string]any{"artifact": "spec", "quote": quote},
		}, "bob")
	}
	ask := func(question string) string {
		t.Helper()
		asked := sessionRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/asks", map[string]any{
			"question": question, "anchor": map[string]any{"artifact": "spec", "quote": insertedWords},
			"actor": sessionActor(),
		})
		if asked.Code != http.StatusCreated {
			t.Fatalf("open an ask on the inserted words: status=%d body=%s", asked.Code, asked.Body.String())
		}
		return decodeBody[model.Ask](t, asked).ID
	}
	f := anchorsOnAnInsert{
		documentService: documentService, handler: handler, database: database, server: served,
		issueKey: issue.Key, artifactID: issue.PrimaryArtifactID,
		insert: createThreadComment(t, handler, issue.Key, map[string]any{
			"body":       "Add this word.",
			"anchor":     map[string]any{"artifact": "spec", "mark_id": "m-insert"},
			"suggestion": map[string]string{"replace_with": insertedWords + " "},
		}, "alice"),
		onWords:       comment("Why this word?", insertedWords),
		resolved:      comment("Settled on this word.", insertedWords),
		askID:         ask("Is this word right?"),
		closedAskID:   ask("Was this word right?"),
		straddling:    comment("Why this phrase?", insertedWords+" brown"),
		settled:       comment("Settled on this phrase.", insertedWords+" brown"),
		outside:       comment("Is it a fox?", "fox"),
		browserEdited: comment("Does it jump?", "jumps"),
	}
	for _, id := range []string{f.resolved.ID, f.settled.ID} {
		if response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/comments/"+id+"/resolve", map[string]any{}, "bob"); response.Code != http.StatusOK {
			t.Fatalf("resolve comment %s: status=%d body=%s", id, response.Code, response.Body.String())
		}
	}
	if response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/asks/"+f.closedAskID+"/answer", map[string]string{"text": "Yes"}, "alice"); response.Code != http.StatusOK {
		t.Fatalf("answer the ask on the inserted words: status=%d body=%s", response.Code, response.Body.String())
	}
	// The positive control: every anchor quotes the text it was made on, standing, so each check
	// of it below can fail.
	for _, want := range []struct {
		comment model.Comment
		quote   string
	}{
		{f.onWords, insertedWords}, {f.resolved, insertedWords}, {f.straddling, insertedWords + " brown"},
		{f.settled, insertedWords + " brown"}, {f.outside, "fox"}, {f.browserEdited, "jumps"},
	} {
		if read := readThreadComment(t, handler, want.comment.ID); read.Anchor == nil || read.Anchor.Quote != want.quote || read.Anchor.Orphaned {
			t.Fatalf("comment %q = anchor %#v, want it quoting %q, standing", want.comment.Body, read.Anchor, want.quote)
		}
	}
	for _, id := range []string{f.askID, f.closedAskID} {
		if anchor := storedAskAnchor(t, database, id); anchor.Quote != insertedWords || anchor.Orphaned {
			t.Fatalf("ask %s = anchor %#v, want it quoting %q, standing", id, anchor, insertedWords)
		}
	}
	return f
}

// storedAskAnchor is the anchor the asks table stores for ask id.
func storedAskAnchor(t *testing.T, database *store.Store, id string) model.Anchor {
	t.Helper()
	var encoded []byte
	if err := database.Pool.QueryRow(context.Background(), `select anchor from asks where id = $1`, id).Scan(&encoded); err != nil {
		t.Fatalf("read the anchor of ask %s: %v", id, err)
	}
	var anchor model.Anchor
	if err := json.Unmarshal(encoded, &anchor); err != nil {
		t.Fatalf("decode the anchor of ask %s: %v", id, err)
	}
	return anchor
}

// takeOutMarkInBrowser stands in for a browser editor that takes the mark of the comment on
// "jumps" off its text, which stays: an edit no settlement has refreshed the document's anchors
// after, so the row still reads as anchored until the next refresh finds its mark gone.
func (f anchorsOnAnInsert) takeOutMarkInBrowser(t *testing.T) {
	t.Helper()
	peer := connectBrowserPeer(t, f.documentService, f.artifactID)
	t.Cleanup(peer.close)
	peer.transact(t, func(txn *crdt.Transaction, fragment *crdt.YXmlFragment) error {
		return pmdoc.Unmark(txn, fragment, string(docs.MarkComment), f.browserEdited.Anchor.MarkID)
	})
	peer.barrier(t)
	if read := readThreadComment(t, f.handler, f.browserEdited.ID); read.Anchor == nil || read.Anchor.Quote != "jumps" || read.Anchor.Orphaned {
		t.Fatalf("the comment on jumps after the browser edit = anchor %#v, want it still read as quoting %q, standing", read.Anchor, "jumps")
	}
}

// requireNoneOfTheWords fails t unless, after the insert's delete that appended every event after
// beforeDelete, no read the ruling names gives any of the inserted words, and each anchor stands as
// the ruling says. Every anchor whose mark lay wholly on the words - open and resolved comments,
// open and answered asks - reads an empty quote, orphaned, on its mark; each comment whose mark
// also wrapped "brown", open and resolved, reads "brown", standing; their newest anchor refreshes,
// every event the delete stored and the event log's read of each of them give none of the words.
// The comment on "fox" keeps its quote, standing, the one on "jumps", whose mark the browser edit
// took out, reads orphaned with its quote kept, and the creation of the comment on the words, an
// event of another post written before the delete, keeps the quote it froze.
func (f anchorsOnAnInsert) requireNoneOfTheWords(t *testing.T, beforeDelete int64) {
	t.Helper()
	requireNoWords := func(what, text string) {
		t.Helper()
		if strings.Contains(text, insertedWords) {
			t.Errorf("%s gives the deleted insert suggestion's words %q: %s", what, insertedWords, text)
		}
	}
	for _, want := range []struct {
		comment  model.Comment
		quote    string
		orphaned bool
	}{
		{f.onWords, "", true}, {f.resolved, "", true}, {f.straddling, "brown", false}, {f.settled, "brown", false},
		{f.outside, "fox", false}, {f.browserEdited, "jumps", true},
	} {
		read := readThreadComment(t, f.handler, want.comment.ID)
		if read.Anchor == nil || read.Anchor.Quote != want.quote || read.Anchor.Orphaned != want.orphaned || read.Anchor.MarkID != want.comment.Anchor.MarkID {
			t.Errorf("comment %q reads the anchor %#v, want its mark %q quoting %q, orphaned %t",
				want.comment.Body, read.Anchor, want.comment.Anchor.MarkID, want.quote, want.orphaned)
		}
	}
	for _, id := range []string{f.askID, f.closedAskID} {
		if anchor := storedAskAnchor(t, f.database, id); anchor.Quote != "" || !anchor.Orphaned {
			t.Errorf("ask %s stores the anchor %#v, want it orphaned with an empty quote", id, anchor)
		}
	}
	for what, target := range map[string]string{
		"the read of the open comment on the words":       "/api/v1/comments/" + f.onWords.ID,
		"the read of the resolved comment on the words":   "/api/v1/comments/" + f.resolved.ID,
		"the read of the open comment over the words":     "/api/v1/comments/" + f.straddling.ID,
		"the read of the resolved comment over the words": "/api/v1/comments/" + f.settled.ID,
		"the read of the open ask on the words":           "/api/v1/asks/" + f.askID,
		"the read of the answered ask on the words":       "/api/v1/asks/" + f.closedAskID,
	} {
		response := dispatchRequest(t, f.handler, http.MethodGet, target, nil, "alice")
		if response.Code != http.StatusOK {
			t.Fatalf("%s: status=%d body=%s", what, response.Code, response.Body.String())
		}
		requireNoWords(what, response.Body.String())
	}
	// The rows the delete itself writes, which no refresh reads or which its reject's refresh
	// leaves to it, each announced: those on a mark taken out whole, and the resolved one on a cut
	// mark. The open comment on a cut mark is a refresh's to write.
	for _, row := range []struct {
		id, refreshType, quote string
		orphaned               bool
	}{
		{f.onWords.ID, "comment.anchor_refreshed", "", true},
		{f.resolved.ID, "comment.anchor_refreshed", "", true},
		{f.askID, "ask.anchor_refreshed", "", true},
		{f.closedAskID, "ask.anchor_refreshed", "", true},
		{f.settled.ID, "comment.anchor_refreshed", "brown", false},
	} {
		var newest *storedCommentEvent
		for _, event := range storedCommentEvents(t, f.database, row.id, beforeDelete) {
			if event.eventType == row.refreshType {
				newest = &event
			}
		}
		if newest == nil {
			t.Errorf("the delete stored no %s of %s, so no event announces its anchor's new quote", row.refreshType, row.id)
			continue
		}
		anchor, _ := newest.payload["anchor"].(map[string]any)
		if anchor == nil || anchor["quote"] != row.quote || anchor["orphaned"] != row.orphaned {
			t.Errorf("the newest stored %s of %s = %s, want its anchor quoting %q, orphaned %t", row.refreshType, row.id, newest.raw, row.quote, row.orphaned)
		}
	}
	rows, err := f.database.Pool.Query(context.Background(), `select type, payload::text from events where id > $1 order by id`, beforeDelete)
	if err != nil {
		t.Fatalf("read the events the delete stored: %v", err)
	}
	stored := 0
	for rows.Next() {
		var eventType, raw string
		if err := rows.Scan(&eventType, &raw); err != nil {
			rows.Close()
			t.Fatalf("scan an event the delete stored: %v", err)
		}
		stored++
		requireNoWords("the stored "+eventType+" the delete wrote", raw)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		t.Fatalf("read the events the delete stored: %v", err)
	}
	logged := dispatchRequest(t, f.handler, http.MethodGet, "/api/v1/issues/"+f.issueKey+"/events", nil, "alice")
	if logged.Code != http.StatusOK {
		t.Fatalf("read the issue's events: status=%d body=%s", logged.Code, logged.Body.String())
	}
	logRead := 0
	for _, event := range decodeBody[[]model.Event](t, logged) {
		if event.ID <= beforeDelete {
			continue
		}
		logRead++
		encoded, err := json.Marshal(event.Payload)
		if err != nil {
			t.Fatalf("encode the event log's %s: %v", event.Type, err)
		}
		requireNoWords("the event log's "+event.Type+" the delete wrote", string(encoded))
	}
	if logRead != stored || stored == 0 {
		t.Errorf("the event log reads %d events the delete wrote, and the events table stores %d: want the same events, at least one", logRead, stored)
	}
	froze := false
	for _, event := range storedCommentEvents(t, f.database, f.onWords.ID, 0) {
		if event.eventType == "comment.created" {
			quote, _ := payloadAnchorQuote(event.payload)
			froze = quote == insertedWords
		}
	}
	if !froze {
		t.Errorf("the stored comment.created of the comment on the inserted words no longer quotes %q, the words it froze", insertedWords)
	}
}

// Anchors on words comment A's pending insert suggestion put in the document. A's author deletes A,
// whose reject takes the words out (docs.RejectDeletedCommentSuggestion) and answers the marks it
// took text from: the delete empties the quote of every anchor whose mark lay wholly on the words,
// open or closed, which stands orphaned, and gives a closed one whose mark it cut that mark's
// remaining text, as the reject's refresh gives an open one (docs.RewriteTakenAnchors). The comment
// on "jumps", whose mark a browser edit took out before the delete, is orphaned by that refresh
// with its quote kept: its mark was never on the words.
func TestDeletingAPendingInsertEmptiesTheQuoteOfEveryOtherAnchorOnItsWords(t *testing.T) {
	f := newAnchorsOnAnInsert(t)
	f.takeOutMarkInBrowser(t)
	beforeDelete := lastEventID(t, f.database)

	deleteThreadComment(t, f.handler, f.insert.ID, "alice")

	f.requireNoneOfTheWords(t, beforeDelete)
}

// The same anchors, where a reject took A's words out before its author deletes A. The reject
// orphans the open anchors that lay wholly on the words, keeping their quote, and cuts the open one
// over them back to "brown", as any refresh does; the words are A's own while A stands, and the
// closed anchors keep them. The reject records on A's row the marks it took text from, where no
// answer gives them, and the delete reads them and rewrites every anchor on them against the
// document as it stands (docs.RewriteTakenAnchors).
func TestDeletingARejectedInsertEmptiesTheQuoteOfEveryOtherAnchorItsRejectTookTextFrom(t *testing.T) {
	f := newAnchorsOnAnInsert(t)
	rejected := dispatchRequest(t, f.handler, http.MethodPost, "/api/v1/comments/"+f.insert.ID+"/reject", map[string]any{}, "alice")
	if rejected.Code != http.StatusOK {
		t.Fatalf("reject the insert: status=%d body=%s", rejected.Code, rejected.Body.String())
	}
	if strings.Contains(rejected.Body.String(), `"taken"`) {
		t.Errorf("the reject's answer gives the marks it took text from: %s", rejected.Body.String())
	}
	var recorded bool
	if err := f.database.Pool.QueryRow(context.Background(), `
		select suggestion->'taken' is not null from comments where id = $1
	`, f.insert.ID).Scan(&recorded); err != nil {
		t.Fatalf("read the marks the reject recorded: %v", err)
	}
	if !recorded {
		t.Fatalf("the reject recorded no marks it took text from on the insert's row")
	}
	// What the reject left, which the delete is then left to rewrite: the words stand in the
	// quotes of the open anchors it orphaned and of the closed ones.
	for _, want := range []struct {
		comment  model.Comment
		quote    string
		orphaned bool
	}{
		{f.onWords, insertedWords, true}, {f.resolved, insertedWords, false},
		{f.straddling, "brown", false}, {f.settled, insertedWords + " brown", false},
	} {
		if read := readThreadComment(t, f.handler, want.comment.ID); read.Anchor == nil || read.Anchor.Quote != want.quote || read.Anchor.Orphaned != want.orphaned {
			t.Fatalf("comment %q after the reject = anchor %#v, want it quoting %q, orphaned %t", want.comment.Body, read.Anchor, want.quote, want.orphaned)
		}
	}
	for id, orphaned := range map[string]bool{f.askID: true, f.closedAskID: false} {
		if anchor := storedAskAnchor(t, f.database, id); anchor.Quote != insertedWords || anchor.Orphaned != orphaned {
			t.Fatalf("ask %s after the reject = anchor %#v, want it quoting %q, orphaned %t", id, anchor, insertedWords, orphaned)
		}
	}
	f.takeOutMarkInBrowser(t)
	beforeDelete := lastEventID(t, f.database)

	deleteThreadComment(t, f.handler, f.insert.ID, "alice")

	f.requireNoneOfTheWords(t, beforeDelete)
}
