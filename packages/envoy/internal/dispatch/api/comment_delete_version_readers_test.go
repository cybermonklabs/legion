package api

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/text"
)

// The architect's ruling of 2026-11-20 22:18Z on dispatch://INFRA-1523, census class V (b): the
// stored readers of an old version, and the stored answers cut from one, give no word of a pending
// insert suggestion its author deleted. A pinned block reference (V.6) and the candidates of a
// pinned short block id (V.11) read the version's block text masked by its insert tree, as every
// read of the version's text is (docs.MaskedVersionBlockTexts); a kept related-item check whose
// document hit was cut while the insert stood is not replayed (V.22, answerEndedByDelete); and an
// answer run given a document holding the words is withdrawn by the comment's delete (V.13,
// goingItemText, recordGoingPosts).

// pinnedIDPrefix is the prefix the two paragraphs of a pinnedInsert's spec share: their ids are it
// with "ef" and with "ff".
const pinnedIDPrefix = "ab12cd34"

// pinnedInsert is a spec of two paragraphs whose ids share pinnedIDPrefix, the first holding
// insertedWords under alice's pending insert suggestion, and the version named while the insert
// stood pending, whose insert tree holds the suggestion's mark and whose block index recorded the
// first paragraph's text with the words.
type pinnedInsert struct {
	handler http.Handler
	specRef string
	version int
	insert  model.Comment
}

// newPinnedInsert makes a pinnedInsert. A browser gives the spec's paragraphs their ids, as a
// browser keeps the ids its blocks carry, and marks the words as an insert suggestion, which alice
// then suggests; the version named after holds both.
func newPinnedInsert(t *testing.T) pinnedInsert {
	t.Helper()
	documentService, handler, database := browserDocumentService(t)
	issue := createInteractionIssue(t, handler, "TEST", "Pinned reads of an insert's words",
		"The "+insertedWords+" brown fox jumps\n\nAnother paragraph.")
	peer := connectBrowserPeer(t, documentService, issue.PrimaryArtifactID)
	t.Cleanup(peer.close)
	peer.edit(t, func(tree *pmdoc.Node) error {
		if len(tree.Children) != 2 {
			return fmt.Errorf("the spec holds %d blocks, want its two paragraphs", len(tree.Children))
		}
		for index, suffix := range []string{"ef", "ff"} {
			paragraph := tree.Children[index]
			if paragraph.Attrs == nil {
				paragraph.Attrs = pmdoc.Attrs{}
			}
			paragraph.Attrs[pmdoc.BlockIDAttr] = pinnedIDPrefix + suffix
		}
		return nil
	})
	peer.barrier(t)
	writeBrowserMark(t, database, documentService, issue.PrimaryArtifactID, docs.MarkSpec{
		Kind:  docs.MarkSuggestion,
		ID:    "m-insert",
		Attrs: pmdoc.Attrs{"id": "m-insert", "by": "user:alice", "kind": "insert"},
	}, insertedWords+" ")
	insert := createThreadComment(t, handler, issue.Key, map[string]any{
		"body":       "Add this word.",
		"anchor":     map[string]any{"artifact": "spec", "mark_id": "m-insert"},
		"suggestion": map[string]string{"replace_with": insertedWords + " "},
	}, "alice")
	return pinnedInsert{
		handler: handler,
		specRef: "dispatch://" + issue.Key + "/spec",
		version: nameVersion(t, handler, issue.PrimaryArtifactID, "With the insert pending"),
		insert:  insert,
	}
}

// pinned is the reference to block of the spec at the version named while the insert stood.
func (f pinnedInsert) pinned(block string) string {
	return f.specRef + "#b-" + block + "@v" + strconv.Itoa(f.version)
}

// V.6: a block reference pinned to the version named while the insert stood gives the block's text
// at that version, which the block index recorded with the words and never rewrites. Once the
// insert's author deletes it, the pin's text is the block's in that version's tree read without
// the words, as the version's own read gives it (docs.MaskedVersionText).
func TestAPinnedBlockReferenceGivesNoWordOfAPendingInsertDeletedSince(t *testing.T) {
	f := newPinnedInsert(t)
	reference := f.pinned(text.EncodeBlockID(pinnedIDPrefix + "ef"))
	read := func() (string, model.GraphReferences) {
		t.Helper()
		response := graphRequest(t, f.handler, url.Values{"to": {reference}})
		if response.Code != http.StatusOK {
			t.Fatalf("GET the references to %s: status=%d body=%s", reference, response.Code, response.Body.String())
		}
		return response.Body.String(), decodeBody[model.GraphReferences](t, response)
	}
	// The positive control: while the insert stands pending, the pin gives its words.
	if _, before := read(); before.Node.Pin == nil || !strings.Contains(before.Node.Pin.Text, insertedWords) {
		t.Fatalf("the pinned reference before the delete = %#v, want its pin giving %q, so its check after the delete can fail", before.Node, insertedWords)
	}

	deleteThreadComment(t, f.handler, f.insert.ID, "alice")
	body, after := read()
	if strings.Contains(body, insertedWords) {
		t.Errorf("the pinned reference after the insert's delete gives its words %q: %s", insertedWords, body)
	}
	if after.Node.Pin == nil || after.Node.Pin.Text == "" {
		t.Errorf("the pinned reference after the delete = %#v, want a pin with the block's text at version %d", after.Node, f.version)
	}
}

// V.11: a short block id pinned to the version, which both paragraphs' ids start with, is 409
// ID_AMBIGUOUS listing both blocks with the first line of each one's text at that version. Once the
// insert's author deletes it, the first paragraph's line is its text read without the words.
func TestAPinnedShortBlockIDsCandidatesGiveNoWordOfAPendingInsertDeletedSince(t *testing.T) {
	f := newPinnedInsert(t)
	reference := f.pinned(pinnedIDPrefix)
	read := func() (string, string) {
		t.Helper()
		response := graphRequest(t, f.handler, url.Values{"to": {reference}})
		if response.Code != http.StatusConflict {
			t.Fatalf("GET the references to %s: status=%d body=%s, want 409", reference, response.Code, response.Body.String())
		}
		answer := decodeBody[ambiguousIDAnswer](t, response)
		if answer.Code != contracts.IDAmbiguous || len(answer.Candidates) != 2 ||
			answer.Candidates[0].ID != pinnedIDPrefix+"ef" || answer.Candidates[1].ID != pinnedIDPrefix+"ff" {
			t.Fatalf("the refusal = %+v, want %s listing both paragraphs", answer, contracts.IDAmbiguous)
		}
		lines := make([]string, len(answer.Candidates))
		for index, candidate := range answer.Candidates {
			lines[index] = candidate.FirstLine
		}
		return response.Body.String(), strings.Join(lines, "\n")
	}
	// The positive control: while the insert stands pending, the first paragraph's line gives its
	// words.
	if _, lines := read(); !strings.Contains(lines, insertedWords) {
		t.Fatalf("the candidates' first lines before the delete are %q, want one giving %q, so their check after the delete can fail", lines, insertedWords)
	}

	deleteThreadComment(t, f.handler, f.insert.ID, "alice")
	if body, _ := read(); strings.Contains(body, insertedWords) {
		t.Errorf("the candidates after the insert's delete give its words %q: %s", insertedWords, body)
	}
}

// V.22: a related-item check sent under a key while an insert suggestion stood in a project
// document lists that document with a snippet cut from its version holding the words. Once the
// insert's author deletes it, a repeat of that key is told the answer is not kept, as for a write
// that landed with no copy of its answer, and gives none of the words. So is a repeat of a check
// kept before the insert existed, though its snippet never held them: nothing kept says when an
// answer was stored, so the arm reads no time (answerEndedByDelete), and the architect ruled for
// that over-reach - a key keeps its answer 24 hours, so an answer ended that need not have been
// costs its caller one read again.
func TestAKeptRelatedCheckFromWhileOrBeforeAnInsertStoodIsNotReplayedAfterItsDelete(t *testing.T) {
	documentService, handler, database := browserDocumentService(t)
	createTestProject(t, handler, "CORE")
	notes := createProjectDocument(t, handler, "CORE", "Design notes", "The brown fox jumps\n")
	refreshTermStats(t, database, "CORE")
	check := func(key string) *httptest.ResponseRecorder {
		t.Helper()
		return keyedRequest(t, handler, http.MethodPost, "/api/v1/projects/CORE/related",
			map[string]any{"title": "brown fox jumps", "keywords_only": true}, "alice", key)
	}
	listsNotes := func(response *httptest.ResponseRecorder) bool {
		body := response.Body.String()
		return response.Code == http.StatusOK && strings.Contains(body, `"kind":"document"`) && strings.Contains(body, notes.ID)
	}
	before := check("before-the-insert")
	if !listsNotes(before) || strings.Contains(before.Body.String(), insertedWords) {
		t.Fatalf("the check before the insert: status=%d body=%s, want 200 listing the document %s without %q",
			before.Code, before.Body.String(), notes.ID, insertedWords)
	}

	// A browser inserts the words as alice's insert suggestion, which she suggests, and the version
	// named after holds them under the suggestion's mark.
	writeBrowserDocument(t, documentService, notes.ID, "The "+insertedWords+" brown fox jumps\n")
	insert := suggestProjectInsert(t, handler, database, documentService, notes, "m-insert")
	nameVersion(t, handler, notes.ID, "With the insert pending")
	// The positive control: the check while the insert stands gives its words in the document's
	// snippet, so its repeat after the delete can fail.
	during := check("while-the-insert-stood")
	if !listsNotes(during) || !strings.Contains(during.Body.String(), insertedWords) {
		t.Fatalf("the check while the insert stood: status=%d body=%s, want 200 listing the document %s with %q",
			during.Code, during.Body.String(), notes.ID, insertedWords)
	}

	deleteThreadComment(t, handler, insert.ID, "alice")
	again := check("while-the-insert-stood")
	if code, _ := refusalOf(t, again); again.Code != during.Code || !replayed(again) || code != contracts.IdempotencyAnswerNotKept {
		t.Errorf("the repeat of the check kept while the insert stood: status=%d replayed=%t body=%s, want %d %s, replayed",
			again.Code, replayed(again), again.Body.String(), during.Code, contracts.IdempotencyAnswerNotKept)
	}
	if strings.Contains(again.Body.String(), insertedWords) {
		t.Errorf("the repeat after the delete gives the insert's words %q: %s", insertedWords, again.Body.String())
	}
	kept := check("before-the-insert")
	if code, _ := refusalOf(t, kept); kept.Code != before.Code || !replayed(kept) || code != contracts.IdempotencyAnswerNotKept {
		t.Errorf("the repeat of the check kept before the insert: status=%d replayed=%t body=%s, want %d %s, replayed, as the ruled over-reach ends it",
			kept.Code, replayed(kept), kept.Body.String(), before.Code, contracts.IdempotencyAnswerNotKept)
	}
}

// V.22's window: the delete's deleted_at is when its transaction began (api/comment_delete.go,
// ledger.Now), before it waited for the owner's lock, so a related-item check that claimed its key
// after that and read the document before the delete committed holds the insert's words under a
// key newer than deleted_at, and stores its answer after it. The delete has no pause between its
// transaction's start and its commit but that lock wait, so the row leaves the state the race
// leaves rather than running it, as TestADeleteEndsAKeptAnswerWhateverTimeItBegan does for the
// other arms: the check is kept while the insert stands, the comment is deleted, and its
// deleted_at is moved between the comment's created_at and the key's. A repeat of the key is told
// the answer is not kept, and gives none of the words (answerEndedByDelete).
func TestAKeptRelatedCheckClaimedAfterTheInsertsDeleteBeganIsNotReplayed(t *testing.T) {
	documentService, handler, database := browserDocumentService(t)
	createTestProject(t, handler, "CORE")
	notes := createProjectDocument(t, handler, "CORE", "Design notes", "The "+insertedWords+" brown fox jumps\n")
	insert := suggestProjectInsert(t, handler, database, documentService, notes, "m-insert")
	nameVersion(t, handler, notes.ID, "With the insert pending")
	refreshTermStats(t, database, "CORE")
	const key = "claimed-after-the-delete-began"
	check := func() *httptest.ResponseRecorder {
		t.Helper()
		return keyedRequest(t, handler, http.MethodPost, "/api/v1/projects/CORE/related",
			map[string]any{"title": "brown fox jumps", "keywords_only": true}, "alice", key)
	}
	// The positive control: the check, read before the delete commits, lists the document with
	// the insert's words in its snippet, so its repeat can fail.
	during := check()
	if body := during.Body.String(); during.Code != http.StatusOK || !strings.Contains(body, `"kind":"document"`) ||
		!strings.Contains(body, notes.ID) || !strings.Contains(body, insertedWords) {
		t.Fatalf("the check while the insert stood: status=%d body=%s, want 200 listing the document %s with %q",
			during.Code, body, notes.ID, insertedWords)
	}

	deleteThreadComment(t, handler, insert.ID, "alice")
	ctx := context.Background()
	moved, err := database.Pool.Exec(ctx, `
		update comments c set deleted_at = c.created_at + (k.created_at - c.created_at) / 2
		from request_keys k where c.id = $1 and k.idempotency_key = $2
	`, insert.ID, key)
	if err != nil || moved.RowsAffected() != 1 {
		t.Fatalf("move the deleted_at of comment %s between its creation and the key's: %v (%d rows)", insert.ID, err, moved.RowsAffected())
	}
	// The control: the delete now reads as one that began after the comment was made and before
	// the key was claimed, which is the race this row stands for.
	var raced bool
	if err := database.Pool.QueryRow(ctx, `
		select c.created_at < c.deleted_at and c.deleted_at < k.created_at
		from comments c, request_keys k where c.id = $1 and k.idempotency_key = $2
	`, insert.ID, key).Scan(&raced); err != nil || !raced {
		t.Fatalf("the deleted_at of comment %s is between its created_at and the key's: %t (%v), want true", insert.ID, raced, err)
	}

	again := check()
	if code, _ := refusalOf(t, again); again.Code != during.Code || !replayed(again) || code != contracts.IdempotencyAnswerNotKept {
		t.Errorf("the repeat of the check claimed after the delete began: status=%d replayed=%t body=%s, want %d %s, replayed",
			again.Code, replayed(again), again.Body.String(), during.Code, contracts.IdempotencyAnswerNotKept)
	}
	if strings.Contains(again.Body.String(), insertedWords) {
		t.Errorf("the repeat of the check claimed after the delete began gives the insert's words %q: %s", insertedWords, again.Body.String())
	}
}

// V.22's window has no start at the key's claim: a related-item check whose key was claimed before
// the insert's comment was made, and whose answer was stored after, can hold the insert's words -
// the check read the document once the comment was made, or a takeover ran it again under the
// key's first claim (request_keys.created_at). The row leaves that state rather than running the
// race, as the row above does: the check is kept while the insert stands, and its key's created_at
// is moved to before the comment's. Once the insert's author deletes it, a repeat of the key is
// told the answer is not kept, and gives none of the words (answerEndedByDelete).
func TestAKeptRelatedCheckClaimedBeforeTheInsertWasMadeIsNotReplayedAfterItsDelete(t *testing.T) {
	documentService, handler, database := browserDocumentService(t)
	createTestProject(t, handler, "CORE")
	notes := createProjectDocument(t, handler, "CORE", "Design notes", "The "+insertedWords+" brown fox jumps\n")
	insert := suggestProjectInsert(t, handler, database, documentService, notes, "m-insert")
	nameVersion(t, handler, notes.ID, "With the insert pending")
	refreshTermStats(t, database, "CORE")
	const key = "claimed-before-the-insert-was-made"
	check := func() *httptest.ResponseRecorder {
		t.Helper()
		return keyedRequest(t, handler, http.MethodPost, "/api/v1/projects/CORE/related",
			map[string]any{"title": "brown fox jumps", "keywords_only": true}, "alice", key)
	}
	// The positive control: the check lists the document with the insert's words in its snippet,
	// so its repeat can fail.
	during := check()
	if body := during.Body.String(); during.Code != http.StatusOK || !strings.Contains(body, `"kind":"document"`) ||
		!strings.Contains(body, notes.ID) || !strings.Contains(body, insertedWords) {
		t.Fatalf("the check while the insert stood: status=%d body=%s, want 200 listing the document %s with %q",
			during.Code, body, notes.ID, insertedWords)
	}

	ctx := context.Background()
	moved, err := database.Pool.Exec(ctx, `
		update request_keys k set created_at = c.created_at - interval '1 second'
		from comments c where c.id = $1 and k.idempotency_key = $2
	`, insert.ID, key)
	if err != nil || moved.RowsAffected() != 1 {
		t.Fatalf("move the created_at of key %s before comment %s's: %v (%d rows)", key, insert.ID, err, moved.RowsAffected())
	}
	// The control: the key now reads as claimed before the comment was made, which is the state
	// this row stands for.
	var claimedBefore bool
	if err := database.Pool.QueryRow(ctx, `
		select k.created_at < c.created_at
		from comments c, request_keys k where c.id = $1 and k.idempotency_key = $2
	`, insert.ID, key).Scan(&claimedBefore); err != nil || !claimedBefore {
		t.Fatalf("the created_at of key %s is before comment %s's: %t (%v), want true", key, insert.ID, claimedBefore, err)
	}

	deleteThreadComment(t, handler, insert.ID, "alice")
	again := check()
	if code, _ := refusalOf(t, again); again.Code != during.Code || !replayed(again) || code != contracts.IdempotencyAnswerNotKept {
		t.Errorf("the repeat of the check claimed before the insert was made: status=%d replayed=%t body=%s, want %d %s, replayed",
			again.Code, replayed(again), again.Body.String(), during.Code, contracts.IdempotencyAnswerNotKept)
	}
	if strings.Contains(again.Body.String(), insertedWords) {
		t.Errorf("the repeat of the check claimed before the insert was made gives the insert's words %q: %s", insertedWords, again.Body.String())
	}
}

// The row beside V.22 and V.13 for a comment on a pending insert's words: bob's comment anchored on
// the words alice's insert suggestion put in the document lies in the same text run as the insert,
// so the run carries both marks. The rule that finds an insert in a stored tree
// (docs.InsertMarkPath, docs.InsertMarkIDsPath) reads the type, the kind and the id of one mark at
// a time, so bob's mark is no insert's: his comment's delete ends no kept answer, and withdraws no
// answer run (the row after V.13), while the insert's own delete then does both, as V.22 and V.13
// hold.
func TestAKeptRelatedCheckIsNotEndedByTheDeleteOfACommentOnAPendingInsertsWords(t *testing.T) {
	documentService, handler, database := browserDocumentService(t)
	createTestProject(t, handler, "CORE")
	notes := createProjectDocument(t, handler, "CORE", "Design notes", "The "+insertedWords+" brown fox jumps\n")
	insert := suggestProjectInsert(t, handler, database, documentService, notes, "m-insert")
	created := dispatchRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+notes.ID+"/comments", map[string]any{
		"body": "Why this word?", "anchor": map[string]any{"artifact": notes.Slug, "quote": insertedWords},
	}, "bob")
	if created.Code != http.StatusCreated {
		t.Fatalf("comment on the insert's words: status=%d body=%s", created.Code, created.Body.String())
	}
	onWords := decodeBody[model.Comment](t, created)
	nameVersion(t, handler, notes.ID, "With the insert pending")
	refreshTermStats(t, database, "CORE")
	check := func() *httptest.ResponseRecorder {
		t.Helper()
		return keyedRequest(t, handler, http.MethodPost, "/api/v1/projects/CORE/related",
			map[string]any{"title": "brown fox jumps", "keywords_only": true}, "alice", "while-the-insert-stood")
	}
	// The positive control: the check lists the document with the insert's words in its snippet,
	// and not bob's comment, so only the rule can end it at his comment's delete.
	during := check()
	if body := during.Body.String(); during.Code != http.StatusOK || !strings.Contains(body, `"kind":"document"`) ||
		!strings.Contains(body, notes.ID) || !strings.Contains(body, insertedWords) || strings.Contains(body, onWords.ID) {
		t.Fatalf("the check while the insert stood: status=%d body=%s, want 200 listing the document %s with %q and not the comment %s",
			during.Code, body, notes.ID, insertedWords, onWords.ID)
	}

	deleteThreadComment(t, handler, onWords.ID, "bob")
	if kept := check(); kept.Code != during.Code || !replayed(kept) || kept.Body.String() != during.Body.String() {
		t.Errorf("the repeat after the delete of bob's comment on the insert's words: status=%d replayed=%t body=%s, want it replayed as kept: %d %s",
			kept.Code, replayed(kept), kept.Body.String(), during.Code, during.Body.String())
	}
	deleteThreadComment(t, handler, insert.ID, "alice")
	again := check()
	if code, _ := refusalOf(t, again); again.Code != during.Code || !replayed(again) || code != contracts.IdempotencyAnswerNotKept {
		t.Errorf("the repeat after the insert's delete: status=%d replayed=%t body=%s, want %d %s, replayed",
			again.Code, replayed(again), again.Body.String(), during.Code, contracts.IdempotencyAnswerNotKept)
	}
}

// goingOverAnInsert is the rig of the answer-run rows below: an issue of project GWI whose spec
// holds insertedWords under alice's pending insert suggestion, and that suggestion's comment.
type goingOverAnInsert struct {
	rig        goingRoutesRig
	answerer   *fakeAnswerer
	issueKey   string
	artifactID string
	insert     model.Comment
}

// newGoingOverAnInsert makes a goingOverAnInsert: a browser marks the spec's words as an insert
// suggestion, which alice then suggests.
func newGoingOverAnInsert(t *testing.T) goingOverAnInsert {
	t.Helper()
	answerer := &fakeAnswerer{}
	rig := newGoingRoutesRig(t, answerer)
	documentService, served := rig.server.deps.Docs.(*docs.Service)
	if !served {
		t.Fatalf("the rig's document service is %T, want *docs.Service", rig.server.deps.Docs)
	}
	issue := createInteractionIssue(t, rig.handler, "GWI", "Where the answer reads", "The "+insertedWords+" brown fox jumps")
	writeBrowserMark(t, rig.database, documentService, issue.PrimaryArtifactID, docs.MarkSpec{
		Kind:  docs.MarkSuggestion,
		ID:    "m-insert",
		Attrs: pmdoc.Attrs{"id": "m-insert", "by": "user:alice", "kind": "insert"},
	}, insertedWords+" ")
	return goingOverAnInsert{
		rig: rig, answerer: answerer, issueKey: issue.Key, artifactID: issue.PrimaryArtifactID,
		insert: createThreadComment(t, rig.handler, issue.Key, map[string]any{
			"body":       "Add this word.",
			"anchor":     map[string]any{"artifact": "spec", "mark_id": "m-insert"},
			"suggestion": map[string]string{"replace_with": insertedWords + " "},
		}, "alice"),
	}
}

// V.13: an answer run given the spec while alice's insert suggestion stood in it reads the words
// from the spec's version, whose insert tree holds the suggestion's mark, and quotes them. The
// run is recorded with the suggestion's comment, whose own words they are (goingItemText,
// recordGoingPosts), so the comment's delete withdraws the run as the delete of any post it read
// does: the stream gives the withdrawal alone, under the id of the run's ending.
func TestGoingStreamOverADocumentHoldingAPendingInsertIsWithdrawnByItsDelete(t *testing.T) {
	f := newGoingOverAnInsert(t)
	nameVersion(t, f.rig.handler, f.artifactID, "With the insert pending")
	run := askQuotingPost(t, f.rig, f.answerer, "GWI", "document", f.artifactID, insertedWords)

	// The positive control: while the insert stands, the run streams its frames, the words among
	// them.
	want := quotingStream("document", f.artifactID, insertedWords)
	if before := f.rig.readStream(t, run.ID, ""); before != want {
		t.Fatalf("before the delete the stream wrote\n%q\nwant\n%q", before, want)
	}

	deleteThreadComment(t, f.rig.handler, f.insert.ID, "alice")
	after := f.rig.readStream(t, run.ID, "")
	if after != withdrawnGoingStream(3) {
		t.Errorf("after the insert's delete the stream wrote\n%q\nwant the withdrawal alone\n%q", after, withdrawnGoingStream(3))
	}
	if strings.Contains(after, insertedWords) {
		t.Errorf("after the delete the stream carries the insert's words: %q", after)
	}
}

// V.13's issue hit: an issue's text holds its primary document's latest version (goingItemText's
// issue arm, issue_embedding_spec), so an answer run given the issue while alice's insert
// suggestion stood in its spec reads the insert's words, and the run is recorded with the
// suggestion's comment from the insert tree of that same version. The run searches a word of the
// issue's title, which neither the spec nor the comment holds, so it is given the issue alone and
// no hit whose own arm records the comment: only the issue's arm can, and the comment's delete then
// withdraws the run.
func TestGoingStreamOverAnIssueWhoseSpecHoldsAPendingInsertIsWithdrawnByItsDelete(t *testing.T) {
	f := newGoingOverAnInsert(t)
	nameVersion(t, f.rig.handler, f.artifactID, "With the insert pending")
	run := askQuotingPostFound(t, f.rig, f.answerer, "GWI", "answer", "issue", f.issueKey, insertedWords)
	// The control: the run was given no hit of the spec document or of a comment, whose own arms
	// would record the run whatever the issue's arm reads.
	_, hits, _ := f.answerer.read()
	for _, hit := range hits {
		if hit.Kind == "document" || hit.Kind == "comment" {
			t.Fatalf("the run was given %+v beside the issue, which records it with the comment by an arm of its own: hits %v", hit, hits)
		}
	}
	// The positive control: while the insert stands, the run streams its frames, the words among
	// them.
	want := quotingStream("issue", f.issueKey, insertedWords)
	if before := f.rig.readStream(t, run.ID, ""); before != want {
		t.Fatalf("before the delete the stream wrote\n%q\nwant\n%q", before, want)
	}

	deleteThreadComment(t, f.rig.handler, f.insert.ID, "alice")
	after := f.rig.readStream(t, run.ID, "")
	if after != withdrawnGoingStream(3) {
		t.Errorf("after the insert's delete the stream wrote\n%q\nwant the withdrawal alone\n%q", after, withdrawnGoingStream(3))
	}
	if strings.Contains(after, insertedWords) {
		t.Errorf("after the delete the stream carries the insert's words: %q", after)
	}
}

// The answer-run half of the row after V.22: a run given the spec while bob's comment and alice's
// insert stood in one text run of it is recorded with the insert's comment alone (goingItemText), so
// bob's delete leaves the run's stream as it was, and the insert's delete then withdraws it.
func TestGoingStreamIsNotWithdrawnByTheDeleteOfACommentOnAPendingInsertsWords(t *testing.T) {
	f := newGoingOverAnInsert(t)
	onWords := createThreadComment(t, f.rig.handler, f.issueKey, map[string]any{
		"body": "Why this word?", "anchor": map[string]any{"artifact": "spec", "quote": insertedWords},
	}, "bob")
	nameVersion(t, f.rig.handler, f.artifactID, "With the insert pending")
	run := askQuotingPost(t, f.rig, f.answerer, "GWI", "document", f.artifactID, insertedWords)

	// The positive control: the run streams its frames, and bob's comment is none of the hits it
	// was given, so only the rule can record the run with it.
	want := quotingStream("document", f.artifactID, insertedWords)
	if before := f.rig.readStream(t, run.ID, ""); before != want {
		t.Fatalf("before the deletes the stream wrote\n%q\nwant\n%q", before, want)
	}
	_, hits, _ := f.answerer.read()
	for _, hit := range hits {
		if hit.ID == onWords.ID {
			t.Fatalf("the run was given bob's comment as the hit %+v, so its delete withdraws the run whatever the rule reads", hit)
		}
	}

	deleteThreadComment(t, f.rig.handler, onWords.ID, "bob")
	if after := f.rig.readStream(t, run.ID, ""); after != want {
		t.Errorf("after the delete of bob's comment on the insert's words the stream wrote\n%q\nwant it as before\n%q", after, want)
	}
	deleteThreadComment(t, f.rig.handler, f.insert.ID, "alice")
	if after := f.rig.readStream(t, run.ID, ""); after != withdrawnGoingStream(3) {
		t.Errorf("after the insert's delete the stream wrote\n%q\nwant the withdrawal alone\n%q", after, withdrawnGoingStream(3))
	}
}
