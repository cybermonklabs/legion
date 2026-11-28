package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"reflect"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// deleteComment deletes the comment as login, or as the session sessionActor() names when login
// is empty, and fails t on anything but 200.
func deleteThreadComment(t *testing.T, handler http.Handler, id, login string) model.Comment {
	t.Helper()
	var response = dispatchRequest(t, handler, http.MethodDelete, "/api/v1/comments/"+id, nil, login)
	if login == "" {
		response = sessionRequest(t, handler, http.MethodDelete, "/api/v1/comments/"+id, map[string]any{
			"actor": sessionActor(),
		})
	}
	if response.Code != http.StatusOK {
		t.Fatalf("delete comment %s: status=%d body=%s", id, response.Code, response.Body.String())
	}
	return decodeBody[model.Comment](t, response)
}

// commentBlockRow is one comment_blocks row as this file reads it: whether the block is still
// in the body, and the excerpt a reply read would serve as its text.
type commentBlockRow struct {
	current bool
	excerpt string
}

func commentBlockRows(t *testing.T, database *store.Store, commentID string) []commentBlockRow {
	t.Helper()
	rows, err := database.Pool.Query(context.Background(), `
		select position is not null, excerpt from comment_blocks
		where comment_id = $1 order by block_id
	`, commentID)
	if err != nil {
		t.Fatalf("read the blocks of %s: %v", commentID, err)
	}
	defer rows.Close()
	var blocks []commentBlockRow
	for rows.Next() {
		var block commentBlockRow
		if err := rows.Scan(&block.current, &block.excerpt); err != nil {
			t.Fatalf("scan a block of %s: %v", commentID, err)
		}
		blocks = append(blocks, block)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read the blocks of %s: %v", commentID, err)
	}
	return blocks
}

func commentSearchVector(t *testing.T, database *store.Store, commentID string) string {
	t.Helper()
	var vector string
	if err := database.Pool.QueryRow(context.Background(),
		`select search::text from comments where id = $1`, commentID).Scan(&vector); err != nil {
		t.Fatalf("read the search vector of %s: %v", commentID, err)
	}
	return vector
}

// postEmbeddingWindows counts the embedding windows of one post, whose ids all begin with the
// owner key `<kind>/<uuid>` (refs.ItemKind.WindowOwnerKey): the rows a meaning search matches a
// post by.
func postEmbeddingWindows(t *testing.T, database *store.Store, ownerKey string) int {
	t.Helper()
	var windows int
	if err := database.Pool.QueryRow(context.Background(), `
		select count(*) from embeddings where split_part(id, '#', 1) = $1
	`, ownerKey).Scan(&windows); err != nil {
		t.Fatalf("count the embedding windows of %s: %v", ownerKey, err)
	}
	return windows
}

// A delete clears the body and every copy of the body that does not follow the cleared column:
// the search vector, each block row with its stored excerpt, the document margin mark, and a
// suggestion's replacement text.
func TestDeleteCommentClearsEveryCopyOfItsBody(t *testing.T) {
	t.Parallel()
	handler, database := newTestHandlerWithStore(t)
	issue := createInteractionIssue(t, handler, "TEST", "Delete a comment", "before")
	const body = "The first paragraph of the withdrawn claim.\n\nThe second paragraph of it."
	comment := createThreadComment(t, handler, issue.Key, map[string]any{
		"body": body, "anchor": map[string]any{"artifact": "spec", "quote": "before"},
	}, "alice")
	if blocks := commentBlockRows(t, database, comment.ID); len(blocks) != 2 {
		t.Fatalf("the comment indexed %d blocks, want the body's two", len(blocks))
	}
	if vector := commentSearchVector(t, database, comment.ID); vector == "" {
		t.Fatalf("the comment's search vector is empty before the delete")
	}
	// A write of a post records its blocks and writes no window: the windows those blocks
	// stand in are the block-index tick's window arm's to write, one tick behind the write
	// (refs.WriteItemWindows, INFRA-988). So the windows the delete has to drop are written
	// here, as that arm writes them.
	windowOwedItems(t, database)
	if windows := postEmbeddingWindows(t, database, "comment/"+comment.ID); windows == 0 {
		t.Fatalf("the comment holds no embedding window before the delete, so dropping them proves nothing")
	}

	deleted := deleteThreadComment(t, handler, comment.ID, "alice")
	if deleted.Body != "" || deleted.DeletedAt == nil || deleted.DeletedBy == nil || deleted.DeletedBy.ID != "alice" {
		t.Fatalf("the delete answered %#v, want an empty body with the mark", deleted)
	}

	stored := readThreadComment(t, handler, comment.ID)
	if stored.Body != "" || stored.DeletedAt == nil || stored.DeletedBy == nil {
		t.Fatalf("the comment reads back as %#v, want an empty body with the mark", stored)
	}
	if vector := commentSearchVector(t, database, comment.ID); vector != "" {
		t.Fatalf("the search vector after the delete = %q, want the empty vector the cleared body makes", vector)
	}
	for index, block := range commentBlockRows(t, database, comment.ID) {
		if block.current {
			t.Fatalf("block %d is still in the body after the delete", index)
		}
		if block.excerpt != "" {
			t.Fatalf("block %d keeps the excerpt %q, which a reply read would serve as its text", index, block.excerpt)
		}
	}
	projection := loadMarkProjection(t, database, issue.PrimaryArtifactID, comment.ID)
	if projection["text"] != "" {
		t.Fatalf("the margin mark after the delete = %#v, want no text", projection)
	}
	if !strings.Contains(strings.Join(commentEventTypes(t, handler, issue.Key), ","), "comment.deleted") {
		t.Fatalf("the event log is missing comment.deleted")
	}
	// A deleted post is reachable by no meaning search: the delete drops every window of the
	// comment in its own transaction (clearDeletedPostBody, refs.TombstoneItem), and the
	// arm's pass after it writes none back, since the empty body splits into no block.
	windowOwedItems(t, database)
	if windows := postEmbeddingWindows(t, database, "comment/"+comment.ID); windows != 0 {
		t.Fatalf("the deleted comment keeps %d embedding windows, want none: a meaning search would reach it", windows)
	}
}

// A suggestion's replacement text is the comment's text too, so the delete clears it with the
// body.
func TestDeleteCommentClearsItsSuggestion(t *testing.T) {
	t.Parallel()
	handler := newTestHandler(t)
	issue := createInteractionIssue(t, handler, "TEST", "Delete a suggestion", "before")
	comment := createThreadComment(t, handler, issue.Key, map[string]any{
		"body": "Say this instead.", "suggestion": map[string]string{"replace_with": "the replacement text"},
	}, "alice")
	if comment.Suggestion == nil {
		t.Fatalf("the comment was created with no suggestion: %#v", comment)
	}

	if deleted := deleteThreadComment(t, handler, comment.ID, "alice"); deleted.Suggestion != nil {
		t.Fatalf("the delete answered the suggestion %#v, want none", deleted.Suggestion)
	}
	stored := readThreadComment(t, handler, comment.ID)
	if stored.Suggestion != nil {
		t.Fatalf("the comment keeps the suggestion %#v after the delete", stored.Suggestion)
	}
}

// Each reply keeps its own text and its own place: the deleted root stays where it was in the
// thread, with the reply under it, reading as it always did.
func TestDeleteCommentKeepsEachReplysTextAndPlace(t *testing.T) {
	t.Parallel()
	handler := newTestHandler(t)
	issue := createInteractionIssue(t, handler, "TEST", "Delete a root", "before")
	root := createThreadComment(t, handler, issue.Key, map[string]any{"body": "the withdrawn root"}, "alice")
	reply := createThreadComment(t, handler, issue.Key, map[string]any{
		"body": "the reply that stands", "reply_to": root.ID,
	}, "bob")

	deleteThreadComment(t, handler, root.ID, "alice")

	response := dispatchRequest(t, handler, http.MethodGet, "/api/v1/comments/"+root.ID, nil, "alice")
	if response.Code != http.StatusOK {
		t.Fatalf("read the deleted thread: status=%d body=%s", response.Code, response.Body.String())
	}
	read := decodeBody[commentRead](t, response)
	if read.Comment.Body != "" || read.Comment.DeletedAt == nil {
		t.Fatalf("the deleted root reads as %#v, want an empty body with the mark", read.Comment)
	}
	if len(read.Replies) != 1 || read.Replies[0].ID != reply.ID || read.Replies[0].Body != "the reply that stands" {
		t.Fatalf("the thread's replies = %#v, want the one reply with its own text", read.Replies)
	}
	if read.Replies[0].DeletedAt != nil {
		t.Fatalf("the reply carries the deleted mark %#v, which belongs to the root alone", read.Replies[0].DeletedAt)
	}

	listed := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+issue.Key+"/comments", nil, "alice")
	if listed.Code != http.StatusOK {
		t.Fatalf("list the issue's comments: status=%d body=%s", listed.Code, listed.Body.String())
	}
	comments := decodeBody[[]model.Comment](t, listed)
	if len(comments) != 2 || comments[0].ID != root.ID || comments[1].ID != reply.ID {
		t.Fatalf("the issue's comments = %#v, want the deleted root in its place with the reply after it", comments)
	}
}

// A delivery still pending would send the text after the delete, so the delete fails it with
// the one error that says why, and appends the comment.delivery receipt that settle owes.
func TestDeleteCommentFailsEachPendingMentionAttempt(t *testing.T) {
	t.Parallel()
	handler, database := newTestHandlerWithStore(t)
	issue := createInteractionIssue(t, handler, "TEST", "Delete with a pending send", "before")
	comment := createThreadComment(t, handler, issue.Key, map[string]any{"body": "mentioning a session"}, "alice")
	if _, err := database.Pool.Exec(context.Background(), `
		insert into comment_deliveries (comment_id, target, attempt, delivery, session_id, state)
		values ($1, 'session:s1', 1, 'steer', 's1', 'pending')
	`, comment.ID); err != nil {
		t.Fatalf("seed a pending attempt: %v", err)
	}

	deleteThreadComment(t, handler, comment.ID, "alice")

	var state, failure string
	if err := database.Pool.QueryRow(context.Background(), `
		select state, coalesce(error, '') from comment_deliveries
		where comment_id = $1 and target = 'session:s1' and attempt = 1
	`, comment.ID).Scan(&state, &failure); err != nil {
		t.Fatalf("read the attempt after the delete: %v", err)
	}
	if state != "failed" || failure != deletedPostDeliveryError {
		t.Fatalf("the attempt reads %q / %q, want failed with %q", state, failure, deletedPostDeliveryError)
	}

	receipts := issueEventsOfType(t, handler, issue.Key, "comment.delivery")
	if len(receipts) != 1 {
		t.Fatalf("the issue holds %d comment.delivery receipts, want the one the settle owes", len(receipts))
	}
	var payload struct {
		CommentID string `json:"comment_id"`
		Target    string `json:"target"`
		State     string `json:"state"`
		Error     string `json:"error"`
	}
	if err := json.Unmarshal(receipts[0].Payload, &payload); err != nil {
		t.Fatalf("decode the comment.delivery receipt: %v", err)
	}
	if payload.CommentID != comment.ID || payload.Target != "session:s1" ||
		payload.State != "failed" || payload.Error != deletedPostDeliveryError {
		t.Fatalf("the receipt says %#v, want the failed attempt with its error", payload)
	}
}

// The refusals are in one order on this write: the author first, then the post already deleted,
// since a delete is final. A session deletes its own comment, and no other session's.
func TestDeleteCommentIsAuthorOnlyAndFinal(t *testing.T) {
	t.Parallel()
	handler := newTestHandler(t)
	issue := createInteractionIssue(t, handler, "TEST", "Who may delete", "before")
	agents := createThreadComment(t, handler, issue.Key, map[string]any{
		"body": "the session's own comment", "actor": sessionActor(),
	}, "")

	person := dispatchRequest(t, handler, http.MethodDelete, "/api/v1/comments/"+agents.ID, nil, "alice")
	if person.Code != http.StatusForbidden || !strings.Contains(person.Body.String(), `"code":"NOT_AUTHOR"`) {
		t.Fatalf("a person deletes a session's comment: status=%d body=%s, want 403 NOT_AUTHOR", person.Code, person.Body.String())
	}
	other := sessionRequest(t, handler, http.MethodDelete, "/api/v1/comments/"+agents.ID, map[string]any{
		"actor": map[string]any{"kind": "session", "id": "session-fedcba9876543210"},
	})
	if other.Code != http.StatusForbidden || !strings.Contains(other.Body.String(), `"code":"NOT_AUTHOR"`) {
		t.Fatalf("another session deletes it: status=%d body=%s, want 403 NOT_AUTHOR", other.Code, other.Body.String())
	}
	if stored := readThreadComment(t, handler, agents.ID); stored.Body != "the session's own comment" {
		t.Fatalf("the comment after the refused deletes = %q, want it untouched", stored.Body)
	}

	deleteThreadComment(t, handler, agents.ID, "")
	again := sessionRequest(t, handler, http.MethodDelete, "/api/v1/comments/"+agents.ID, map[string]any{
		"actor": sessionActor(),
	})
	if again.Code != http.StatusConflict || !strings.Contains(again.Body.String(), `"code":"POST_DELETED"`) {
		t.Fatalf("a second delete: status=%d body=%s, want 409 POST_DELETED", again.Code, again.Body.String())
	}
}

// A pinned block reference serves the text the block's revision recorded (refs.ItemBlockAt), and
// the delete's re-index writes a null text at the new revision alone, so every earlier revision
// of the block still held the withdrawn text. The delete clears them, and a pin to any revision
// of a deleted comment's block then reads as one the comment does not hold. The same pinned read
// of a comment that stands is the control: it still gives its text.
func TestAPinnedBlockReferenceToADeletedCommentGivesNoText(t *testing.T) {
	t.Parallel()
	handler := newTestHandler(t)
	issue := createInteractionIssue(t, handler, "TEST", "Pinned blocks of a deleted comment", "before")
	const withdrawn = "The second paragraph of the withdrawn claim."
	const standing = "The second paragraph of the claim that stands."
	withdrawnComment := createThreadComment(t, handler, issue.Key, map[string]any{"body": "Alpha.\n\n" + withdrawn}, "alice")
	standingComment := createThreadComment(t, handler, issue.Key, map[string]any{"body": "Gamma.\n\n" + standing}, "alice")
	withdrawnRef := "dispatch://" + issue.Key + "/comment/" + withdrawnComment.ID + "#b-b2"
	standingRef := "dispatch://" + issue.Key + "/comment/" + standingComment.ID + "#b-b2"
	requirePinnedText := func(step, ref, text string) {
		t.Helper()
		read := readGraph(t, handler, url.Values{"to": {ref + "@v1"}})
		if read.Node.Pin == nil || read.Node.Pin.Text != text {
			t.Fatalf("%s: the pin reads %#v, want the text %q", step, read.Node.Pin, text)
		}
	}
	requirePinnedText("before the delete", withdrawnRef, withdrawn)
	requirePinnedText("the comment that stands, before the delete", standingRef, standing)

	deleteThreadComment(t, handler, withdrawnComment.ID, "alice")

	response := graphRequest(t, handler, url.Values{"to": {withdrawnRef + "@v1"}})
	if response.Code != http.StatusNotFound ||
		!strings.Contains(response.Body.String(), `no block \"b2\" in comment `+withdrawnComment.ID+" at revision 1") {
		t.Fatalf("the pinned read of a deleted comment's block: status=%d body=%s, want 404 for a revision that holds no block",
			response.Code, response.Body.String())
	}
	requirePinnedText("the comment that stands, after the delete", standingRef, standing)
}

// R3: every read of a deleted post says that it is deleted. A block of a deleted comment is
// gone with the body, so neither block read can give text; what each must not do is answer that
// the block is missing and leave the deletion unsaid. Both reads of the withdrawn comment's
// block - the unpinned one and a pin to the revision that recorded it - name the deletion in
// the words every refusal of a deleted post uses, and give no part of the body. The same two
// reads of a comment that stands are the control: unchanged, text and all.
func TestABlockReferenceToADeletedCommentNamesTheDeletion(t *testing.T) {
	t.Parallel()
	handler := newTestHandler(t)
	issue := createInteractionIssue(t, handler, "TEST", "Blocks of a deleted comment", "before")
	const withdrawn = "The second paragraph of the withdrawn claim."
	const standing = "The second paragraph of the claim that stands."
	withdrawnComment := createThreadComment(t, handler, issue.Key, map[string]any{"body": "Alpha.\n\n" + withdrawn}, "alice")
	standingComment := createThreadComment(t, handler, issue.Key, map[string]any{"body": "Gamma.\n\n" + standing}, "alice")
	withdrawnRef := "dispatch://" + issue.Key + "/comment/" + withdrawnComment.ID + "#b-b2"
	standingRef := "dispatch://" + issue.Key + "/comment/" + standingComment.ID + "#b-b2"

	deleteThreadComment(t, handler, withdrawnComment.ID, "alice")

	deletion := `: this comment is deleted`
	for _, read := range []struct{ name, ref, want string }{
		{
			name: "the unpinned read of a deleted comment's block",
			ref:  withdrawnRef,
			want: `no block \"b2\" in comment ` + withdrawnComment.ID + deletion,
		},
		{
			name: "a pin to the revision that recorded a deleted comment's block",
			ref:  withdrawnRef + "@v1",
			want: `no block \"b2\" in comment ` + withdrawnComment.ID + " at revision 1" + deletion,
		},
	} {
		response := graphRequest(t, handler, url.Values{"to": {read.ref}})
		if response.Code != http.StatusNotFound || !strings.Contains(response.Body.String(), `"code":"NOT_FOUND"`) ||
			!strings.Contains(response.Body.String(), read.want) {
			t.Fatalf("%s: status=%d body=%s, want 404 NOT_FOUND saying %q", read.name, response.Code, response.Body.String(), read.want)
		}
	}
	// A pin past the deleted comment's current revision is refused by the revision it names
	// rather than by the block, and it says the comment is deleted all the same. The refusal is
	// built from revision numbers and the delete mark alone, so no change of this server could
	// put the withdrawn text in it: the two clauses below are what this read is held to.
	pastRevision := graphRequest(t, handler, url.Values{"to": {withdrawnRef + "@v99"}})
	if body := pastRevision.Body.String(); pastRevision.Code != http.StatusNotFound ||
		!strings.Contains(body, "no revision 99 of comment "+withdrawnComment.ID) ||
		!strings.Contains(body, deletion) {
		t.Fatalf("a pin past a deleted comment's current revision: status=%d body=%s, want 404 naming the revision and saying %q", pastRevision.Code, body, deletion)
	}

	if node := readGraph(t, handler, url.Values{"to": {standingRef}}).Node; node.Text != standing || node.Pin != nil {
		t.Fatalf("the unpinned read of a standing comment's block = %#v, want the text %q and no pin", node, standing)
	}
	pinned := readGraph(t, handler, url.Values{"to": {standingRef + "@v1"}}).Node
	if pinned.Pin == nil || pinned.Pin.Text != standing || pinned.Pin.State != "unchanged" {
		t.Fatalf("the pinned read of a standing comment's block = %#v, want its text unchanged", pinned.Pin)
	}
}

// A pending suggestion is proof in the document as much as a row: its mark says the suggestion
// stands, and an insert suggestion's text is in the document's own body. The delete rejects it,
// so the mark goes and an insert's text goes with it; nothing else could, since an accept and a
// reject of a deleted post answer 409 POST_DELETED and a resolve unmarks nothing. Each check
// reads the document rather than the comment row.
//
// A closed issue takes that delete whole: its document takes the reject as it takes the
// comment's own anchor writes (docs.RejectDeletedCommentSuggestion), so the withdrawn text
// leaves the document there too, where an accept and a reject of the suggestion are refused.
func TestDeleteCommentRejectsItsPendingSuggestion(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name, quote, replaceWith, want string
		insert, closed                 bool
	}{
		{name: "a replace suggestion", quote: "quick", replaceWith: "slow", want: "The quick brown fox\n"},
		{name: "an insert suggestion", quote: "quick ", want: "The brown fox\n", insert: true},
		{name: "an insert suggestion on a closed issue", quote: "quick ", want: "The brown fox\n", insert: true, closed: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			documentService, handler, database := browserDocumentService(t)
			issue := createInteractionIssue(t, handler, "TEST", "Delete a pending suggestion", "The quick brown fox")
			anchor := map[string]any{"artifact": "spec", "quote": test.quote}
			if test.insert {
				writeBrowserMark(t, database, documentService, issue.PrimaryArtifactID, docs.MarkSpec{
					Kind:  docs.MarkSuggestion,
					ID:    "m-insert",
					Attrs: pmdoc.Attrs{"id": "m-insert", "by": "user:alice", "kind": "insert"},
				}, test.quote)
				anchor = map[string]any{"artifact": "spec", "mark_id": "m-insert"}
			}
			comment := createThreadComment(t, handler, issue.Key, map[string]any{
				"body": "Say this instead.", "anchor": anchor,
				"suggestion": map[string]string{"replace_with": test.replaceWith},
			}, "alice")
			if comment.Anchor == nil || comment.Suggestion == nil {
				t.Fatalf("the comment was created as %#v, want an anchored suggestion", comment)
			}
			if _, err := documentService.VerifyMark(context.Background(), issue.PrimaryArtifactID, docs.MarkSuggestion, comment.Anchor.MarkID); err != nil {
				t.Fatalf("the suggestion's mark before the delete: %v, want it in the document", err)
			}
			// The suggestion stands when the issue is closed: a closed issue takes no suggestion
			// of its own, and this one was made while the issue was open.
			if test.closed {
				closeIssue(t, handler, issue.Key)
			}

			deleted := deleteThreadComment(t, handler, comment.ID, "alice")
			if deleted.Body != "" || deleted.DeletedAt == nil || deleted.Suggestion != nil {
				t.Fatalf("the delete answered %#v, want an empty body with the mark and no suggestion", deleted)
			}
			if stored := readThreadComment(t, handler, comment.ID); stored.Body != "" || stored.DeletedAt == nil {
				t.Fatalf("the comment reads back as %#v, want an empty body with the mark", stored)
			}

			// The mark is read as a closed issue's document is read for a comment: SuggestionKind
			// under docs.WithCommentMarks, which that document's room takes (allowInject).
			// VerifyMark is the open issue's read: its own owner-verified path is a comment
			// mark's alone (closedIssueCommentAnchor), so a suggestion's mark read that way on a
			// closed issue is refused.
			var markErr error
			if test.closed {
				_, markErr = documentService.SuggestionKind(docs.WithCommentMarks(context.Background()),
					issue.PrimaryArtifactID, comment.Anchor.MarkID)
			} else {
				_, markErr = documentService.VerifyMark(context.Background(),
					issue.PrimaryArtifactID, docs.MarkSuggestion, comment.Anchor.MarkID)
			}
			if !errors.Is(markErr, docs.ErrAnchorMissing) {
				t.Fatalf("the suggestion's mark after the delete = %v, want it gone from the document", markErr)
			}
			markdown, err := documentService.Text(context.Background(), issue.PrimaryArtifactID)
			if err != nil || markdown != test.want {
				t.Fatalf("the document after the delete = %q (%v), want %q", markdown, err, test.want)
			}
		})
	}
}

// storedCommentEvent is one event the events table stores about a comment: its type, its payload
// as stored, and that payload decoded.
type storedCommentEvent struct {
	eventType string
	raw       string
	payload   map[string]any
}

// storedCommentEvents is every event stored about comment commentID after the event afterID,
// oldest first, read from the events table rather than through a read that masks it.
func storedCommentEvents(t *testing.T, database *store.Store, commentID string, afterID int64) []storedCommentEvent {
	t.Helper()
	rows, err := database.Pool.Query(context.Background(), `
		select type, payload::text from events where payload->>'id' = $1 and id > $2 order by id
	`, commentID, afterID)
	if err != nil {
		t.Fatalf("read the stored events of %s: %v", commentID, err)
	}
	defer rows.Close()
	var stored []storedCommentEvent
	for rows.Next() {
		var event storedCommentEvent
		if err := rows.Scan(&event.eventType, &event.raw); err != nil {
			t.Fatalf("scan a stored event of %s: %v", commentID, err)
		}
		if err := json.Unmarshal([]byte(event.raw), &event.payload); err != nil {
			t.Fatalf("decode the stored %s of %s: %v", event.eventType, commentID, err)
		}
		stored = append(stored, event)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read the stored events of %s: %v", commentID, err)
	}
	return stored
}

// payloadAnchorQuote is the quote of the anchor an event payload carries, and whether it carries
// one.
func payloadAnchorQuote(payload map[string]any) (string, bool) {
	anchor, ok := payload["anchor"].(map[string]any)
	if !ok {
		return "", false
	}
	quote, ok := anchor["quote"].(string)
	return quote, ok
}

// R3 of dispatch://INFRA-1523: an insert suggestion's mark wraps the text it inserted, so a
// comment anchored on that mark quotes its own suggested words (resolveAnchor). The delete
// rejects the suggestion, which takes those words out of the document, and empties the quote with
// the suggestion (clearDeletedPostBody). So the delete's answer, the comment's read, the
// comment.deleted event as stored, every event the delete stores, and every event of the comment
// the event log reads give the empty quote and none of the words, while the anchor keeps its
// document, its block and its mark id. A replace suggestion's quote and a plain comment's quote
// are document text, which stays in the document, and each keeps its quote everywhere: those
// are the controls.
//
// Where the comment sits does not move with the quote. The anchor keeps its block, and the read
// keeps its anchor_block, through a document edit after the delete, whose anchor refresh a
// standing comment's comment.anchor_refreshed proves ran. A deleted suggestion's anchor reads
// orphaned whether its quote was cleared or not: the reject takes the suggestion's mark out of
// the document - an insert's with the text under it (docs/reject.go:23-56), a replace's by
// unmarking it - and a refresh finds an anchor by its mark alone, never by its quote
// (docs.refreshAnchors), so the delete's own margin write already finds no mark. A plain
// comment's mark stays in the document, so it reads anchored.
func TestDeletingAPendingInsertSuggestionClearsTheQuoteOfItsWords(t *testing.T) {
	t.Parallel()
	const words = "zarquoninserted"
	for _, test := range []struct {
		name               string
		insert, suggestion bool
		quoteBefore, quote string
		orphaned           bool
	}{
		{name: "a pending insert suggestion", insert: true, suggestion: true, quoteBefore: words + " ", quote: "", orphaned: true},
		{name: "a pending replace suggestion", suggestion: true, quoteBefore: words, quote: words, orphaned: true},
		{name: "a comment with no suggestion", quoteBefore: words, quote: words},
	} {
		t.Run(test.name, func(t *testing.T) {
			documentService, handler, database := browserDocumentService(t)
			issue := createInteractionIssue(t, handler, "TEST", "Delete "+test.name, "The "+words+" brown fox")
			input := map[string]any{"body": "Say this instead.", "anchor": map[string]any{"artifact": "spec", "quote": words}}
			if test.suggestion {
				input["suggestion"] = map[string]string{"replace_with": "slow"}
			}
			if test.insert {
				writeBrowserMark(t, database, documentService, issue.PrimaryArtifactID, docs.MarkSpec{
					Kind:  docs.MarkSuggestion,
					ID:    "m-insert",
					Attrs: pmdoc.Attrs{"id": "m-insert", "by": "user:alice", "kind": "insert"},
				}, words+" ")
				input["anchor"] = map[string]any{"artifact": "spec", "mark_id": "m-insert"}
				input["suggestion"] = map[string]string{"replace_with": words + " "}
			}
			comment := createThreadComment(t, handler, issue.Key, input, "alice")
			// A standing comment whose anchor the edit after the delete moves: its refresh event
			// is what shows that the edit refreshed the document's anchors.
			witness := createThreadComment(t, handler, issue.Key, map[string]any{
				"body": "Is it a fox?", "anchor": map[string]any{"artifact": "spec", "quote": "fox"},
			}, "alice")
			// The positive control: before the delete the read quotes the words, so each check of
			// the quote below can fail.
			before := readThreadComment(t, handler, comment.ID)
			if before.Anchor == nil || before.Anchor.Quote != test.quoteBefore || before.Anchor.Orphaned ||
				before.Anchor.BlockID == nil || before.AnchorBlock == nil {
				t.Fatalf("the comment before the delete = %#v (anchor_block %#v), want the quote %q on a placed block",
					before.Anchor, before.AnchorBlock, test.quoteBefore)
			}
			// requireKept fails t unless anchor is the one the comment had before the delete, on
			// the same document, mark and block, with the quote the delete leaves.
			requireKept := func(what string, anchor *model.Anchor) {
				t.Helper()
				if anchor == nil {
					t.Errorf("%s carries no anchor, want the one the comment had", what)
					return
				}
				if anchor.ArtifactID != before.Anchor.ArtifactID || anchor.MarkID != before.Anchor.MarkID ||
					anchor.BlockID == nil || *anchor.BlockID != *before.Anchor.BlockID {
					t.Errorf("%s anchor = %#v, want the document, mark and block of %#v", what, anchor, before.Anchor)
				}
				if anchor.Quote != test.quote {
					t.Errorf("%s anchor quote = %q, want %q", what, anchor.Quote, test.quote)
				}
			}
			// requireRead fails t unless a read of the deleted comment gives the anchor it had
			// (requireKept), with the orphaned flag the delete leaves, and places it where it stood
			// before.
			requireRead := func(what string, read model.Comment) {
				t.Helper()
				requireKept(what, read.Anchor)
				if read.Anchor != nil && read.Anchor.Orphaned != test.orphaned {
					t.Errorf("%s anchor orphaned = %t, want %t", what, read.Anchor.Orphaned, test.orphaned)
				}
				if read.AnchorBlock == nil || !reflect.DeepEqual(read.AnchorBlock, before.AnchorBlock) {
					t.Errorf("%s anchor_block = %#v, want %#v", what, read.AnchorBlock, before.AnchorBlock)
				}
			}
			// requireNoWords fails t when an insert's own words are in text.
			requireNoWords := func(what, text string) {
				t.Helper()
				if test.insert && strings.Contains(text, words) {
					t.Errorf("%s gives the deleted insert suggestion's words %q: %s", what, words, text)
				}
			}
			readRaw := func() string {
				t.Helper()
				response := dispatchRequest(t, handler, http.MethodGet, "/api/v1/comments/"+comment.ID, nil, "alice")
				if response.Code != http.StatusOK {
					t.Fatalf("read comment: status=%d body=%s", response.Code, response.Body.String())
				}
				return response.Body.String()
			}

			beforeDelete := lastEventID(t, database)
			requireKept("the delete's answer", deleteThreadComment(t, handler, comment.ID, "alice").Anchor)
			requireRead("the read after the delete", readThreadComment(t, handler, comment.ID))
			requireNoWords("the read after the delete", readRaw())

			stored := storedCommentEvents(t, database, comment.ID, beforeDelete)
			deletes := 0
			for _, event := range stored {
				requireNoWords("the stored "+event.eventType+" the delete wrote", event.raw)
				if event.eventType != "comment.deleted" {
					continue
				}
				deletes++
				if quote, ok := payloadAnchorQuote(event.payload); !ok || quote != test.quote {
					t.Errorf("the stored comment.deleted anchor quote = %q (carried %t), want %q", quote, ok, test.quote)
				}
			}
			if deletes != 1 {
				t.Errorf("the delete stored %d comment.deleted events, want one", deletes)
			}
			// The event log's read of every event of the comment, its creation's among them. The
			// creation's stored payload still holds the quote it was written with, as no delete
			// rewrites a stored event, so for the insert it holds the words and the read is what
			// takes them out (attachPostMarks).
			for _, event := range storedCommentEvents(t, database, comment.ID, 0) {
				if event.eventType != "comment.created" {
					continue
				}
				if quote, _ := payloadAnchorQuote(event.payload); quote != test.quoteBefore {
					t.Fatalf("the stored comment.created anchor quote = %q, want the %q it was written with", quote, test.quoteBefore)
				}
			}
			read := 0
			for _, event := range decodedIssueEvents(t, handler, issue.Key) {
				if event.Payload["id"] != comment.ID {
					continue
				}
				quote, ok := payloadAnchorQuote(event.Payload)
				if !ok {
					continue
				}
				read++
				if quote != test.quote {
					t.Errorf("the event log's %s anchor quote = %q, want %q", event.Type, quote, test.quote)
				}
				encoded, err := json.Marshal(event.Payload)
				if err != nil {
					t.Fatalf("encode the event log's %s: %v", event.Type, err)
				}
				requireNoWords("the event log's "+event.Type, string(encoded))
			}
			if read < 2 {
				t.Errorf("the event log reads %d anchored events of the comment, want its creation and its delete", read)
			}

			// A document edit after the delete, away from every quote of the comment, refreshes
			// every open anchor of the document.
			beforeEdit := lastEventID(t, database)
			if response := editDocument(t, handler, issue.PrimaryArtifactID, map[string]any{
				"ops": []map[string]any{{"op": "replace", "find": "fox", "with": "dog"}},
			}); response.Code != http.StatusOK {
				t.Fatalf("edit the document after the delete: status=%d body=%s", response.Code, response.Body.String())
			}
			refreshed := 0
			for _, event := range storedCommentEvents(t, database, witness.ID, beforeEdit) {
				if event.eventType == "comment.anchor_refreshed" {
					refreshed++
				}
			}
			if refreshed != 1 {
				t.Fatalf("the edit stored %d comment.anchor_refreshed events of the standing comment, want one: no refresh ran", refreshed)
			}
			requireRead("the read after the refresh", readThreadComment(t, handler, comment.ID))
			requireNoWords("the read after the refresh", readRaw())
			for _, event := range storedCommentEvents(t, database, comment.ID, beforeEdit) {
				requireNoWords("the stored "+event.eventType+" the refresh wrote", event.raw)
			}
		})
	}
}

// An anchor refresh written after a comment's delete is built from the stored row, and the
// events table keeps it as built, where a read of the log masks it. It carries the comment's
// delete mark, as every read of the comment does, so its stored payload never reads as a comment
// that stands with an empty body.
func TestAnAnchorRefreshAfterTheDeleteStoresTheDeleteMark(t *testing.T) {
	t.Parallel()
	handler, database := newTestHandlerWithStore(t)
	issue := createInteractionIssue(t, handler, "TEST", "Refresh a deleted comment", "The quick brown fox")
	comment := createThreadComment(t, handler, issue.Key, map[string]any{
		"body": "On the fox.", "anchor": map[string]any{"artifact": "spec", "quote": "fox"},
	}, "alice")
	deleteThreadComment(t, handler, comment.ID, "alice")

	beforeEdit := lastEventID(t, database)
	if response := editDocument(t, handler, issue.PrimaryArtifactID, map[string]any{
		"ops": []map[string]any{{"op": "replace", "find": "fox", "with": "dog"}},
	}); response.Code != http.StatusOK {
		t.Fatalf("edit the document after the delete: status=%d body=%s", response.Code, response.Body.String())
	}
	var refreshes []storedCommentEvent
	for _, event := range storedCommentEvents(t, database, comment.ID, beforeEdit) {
		if event.eventType == "comment.anchor_refreshed" {
			refreshes = append(refreshes, event)
		}
	}
	if len(refreshes) != 1 {
		t.Fatalf("the edit stored %d comment.anchor_refreshed events of the deleted comment, want one", len(refreshes))
	}
	payload := refreshes[0].payload
	deletedBy, _ := payload["deleted_by"].(map[string]any)
	if payload["deleted_at"] == nil || deletedBy == nil || deletedBy["id"] != "alice" || payload["body"] != "" {
		t.Fatalf("the stored comment.anchor_refreshed of the deleted comment = %s, want an empty body with the delete mark", refreshes[0].raw)
	}
}

// An orphaned suggestion's mark is already out of the document, so the delete has nothing to
// reject: it records the orphaning on the anchor, as the accept and the reject arms do, and
// stands with 200 where those two answer 409. A delete nothing can undo is what a withdrawn post
// needs, and the orphaned mark is the one case the reject itself refuses.
func TestDeleteCommentWithAnOrphanedSuggestionStands(t *testing.T) {
	t.Parallel()
	documentService, handler, _ := browserDocumentService(t)
	issue := createInteractionIssue(t, handler, "TEST", "Delete an orphaned suggestion", "The quick brown fox")
	comment := createThreadComment(t, handler, issue.Key, map[string]any{
		"body": "Use red.", "anchor": map[string]any{"artifact": "spec", "quote": "brown"},
		"suggestion": map[string]string{"replace_with": "red"},
	}, "alice")
	// A browser edit takes the marked text out of the live document, which orphans the mark;
	// the service settles no version here (Settle is an hour), so nothing has refreshed the
	// comment's own anchor and the row still reads as anchored, which the delete is left to
	// find out for itself.
	writeBrowserDocument(t, handler, documentService, issue.PrimaryArtifactID, "The quick fox\n")
	if before := readThreadComment(t, handler, comment.ID); before.Anchor == nil || before.Anchor.Orphaned {
		t.Fatalf("the comment before the delete = %#v, want an anchor not yet known to be orphaned", before.Anchor)
	}

	deleted := deleteThreadComment(t, handler, comment.ID, "alice")
	if deleted.Body != "" || deleted.DeletedAt == nil || deleted.Anchor == nil || !deleted.Anchor.Orphaned {
		t.Fatalf("the delete answered %#v, want an empty body with the mark and an orphaned anchor", deleted)
	}
	stored := readThreadComment(t, handler, comment.ID)
	if stored.Body != "" || stored.DeletedAt == nil || stored.Suggestion != nil {
		t.Fatalf("the comment reads back as %#v, want an empty body with the mark and no suggestion", stored)
	}
	if stored.Anchor == nil || !stored.Anchor.Orphaned {
		t.Fatalf("the deleted comment's anchor = %#v, want the orphaning recorded", stored.Anchor)
	}
}

// R3 of dispatch://INFRA-1523: every read that quotes a deleted post gives no part of its body.
// An accept names the document version it makes after the suggestion's body
// (Docs.NamedVersion), so that name is a copy of the body; the tester found it served by the
// document read after the delete (test round at 55e4dfdf). The red row of that round.
func TestADeletedAcceptedSuggestionLeavesNoPartOfItsBodyInTheDocumentRead(t *testing.T) {
	t.Parallel()
	_, handler, _ := browserDocumentService(t)
	issue := createInteractionIssue(t, handler, "TEST", "Delete an accepted suggestion", "The quick brown fox")
	const phrase = "zarquonacceptedrationale"
	comment := createThreadComment(t, handler, issue.Key, map[string]any{
		"body": "Say this instead, " + phrase + ".", "anchor": map[string]any{"artifact": "spec", "quote": "quick"},
		"suggestion": map[string]string{"replace_with": "slow"},
	}, "alice")
	accepted := dispatchRequest(t, handler, http.MethodPost, "/api/v1/comments/"+comment.ID+"/accept", map[string]any{}, "alice")
	if accepted.Code != http.StatusOK {
		t.Fatalf("accept: status=%d body=%s", accepted.Code, accepted.Body.String())
	}
	// The positive control: before the delete the document read does carry the body, so the
	// absence below is the delete's doing and not a read that never held it.
	if before := dispatchRequest(t, handler, http.MethodGet, "/api/v1/artifacts/"+issue.PrimaryArtifactID, nil, "alice"); !strings.Contains(before.Body.String(), phrase) {
		t.Fatalf("the document read before the delete holds no %q, so this row cannot fail: %s", phrase, before.Body.String())
	}

	deleteThreadComment(t, handler, comment.ID, "alice")

	if after := dispatchRequest(t, handler, http.MethodGet, "/api/v1/artifacts/"+issue.PrimaryArtifactID, nil, "alice"); strings.Contains(after.Body.String(), phrase) {
		t.Fatalf("GET /api/v1/artifacts/%s after the delete still gives the deleted comment's text %q: %s",
			issue.PrimaryArtifactID, phrase, after.Body.String())
	}
}

// R3 of dispatch://INFRA-1523 again: a resolve sent under an Idempotency-Key keeps its first
// answer, which holds the comment's body, and a repeat of that key replays it. After the author
// deletes the comment, that replay is a read of the post that gives its body and no mark; the
// tester found it within the key's lifetime (test round at 55e4dfdf). The red row of that round.
func TestAKeyedReplayAfterTheDeleteGivesNoPartOfTheBody(t *testing.T) {
	t.Parallel()
	handler, _ := newTestHandlerWithStore(t)
	issue := createInteractionIssue(t, handler, "TEST", "Replay a resolve of a deleted comment", "before")
	const phrase = "zarquonkeyedresolve"
	comment := createThreadComment(t, handler, issue.Key, map[string]any{"body": "Resolve me, " + phrase + "."}, "alice")
	target := "/api/v1/comments/" + comment.ID + "/resolve"
	first := keyedRequest(t, handler, http.MethodPost, target, map[string]any{}, "alice", "resolve-then-delete")
	if first.Code != http.StatusOK || !strings.Contains(first.Body.String(), phrase) {
		t.Fatalf("the first resolve: status=%d body=%s, want 200 with the body, so this row can fail", first.Code, first.Body.String())
	}

	deleteThreadComment(t, handler, comment.ID, "alice")

	again := keyedRequest(t, handler, http.MethodPost, target, map[string]any{}, "alice", "resolve-then-delete")
	if strings.Contains(again.Body.String(), phrase) {
		t.Fatalf("the repeat of the key after the delete (status %d) gives the deleted comment's text %q: %s",
			again.Code, phrase, again.Body.String())
	}
}

// R3 of dispatch://INFRA-1523 and the keyed-repeat rule: a related-item check kept under a key
// keeps its snippet of each post it found, and a repeat of that key replays the stored bytes.
// After the post's author deletes it, that replay gives part of the deleted body, where the
// same check under a new key no longer finds the post. The tester found it at 286089e7 (POST
// /api/v1/projects/{key}/related; the issue and ask creates and the decision-block edits carry
// the same check in their advice). The red row of that round.
func TestAKeyedRelatedCheckRepeatedAfterTheDeleteGivesNoSnippetOfThePost(t *testing.T) {
	t.Parallel()
	handler, database := newTestHandlerWithStore(t)
	issue := createInteractionIssue(t, handler, "TEST", "A related check of a deleted comment", "before")
	const phrase = "zarquonquokkatelemetry"
	comment := createThreadComment(t, handler, issue.Key, map[string]any{"body": phrase + " calibration drifted overnight."}, "alice")
	refreshTermStats(t, database, "TEST")
	check := map[string]any{"title": phrase + " calibration", "keywords_only": true}
	first := keyedRequest(t, handler, http.MethodPost, "/api/v1/projects/TEST/related", check, "alice", "related-then-delete")
	if first.Code != http.StatusOK || !strings.Contains(first.Body.String(), phrase) {
		t.Fatalf("the first check: status=%d body=%s, want 200 with a snippet of the comment, so this row can fail", first.Code, first.Body.String())
	}

	deleteThreadComment(t, handler, comment.ID, "alice")

	again := keyedRequest(t, handler, http.MethodPost, "/api/v1/projects/TEST/related", check, "alice", "related-then-delete")
	if strings.Contains(again.Body.String(), phrase) {
		t.Fatalf("the repeat of the check's key after the delete (status %d) gives the deleted comment's text %q: %s",
			again.Code, phrase, again.Body.String())
	}
}

// R3 of dispatch://INFRA-1523 again: a document write kept under a key keeps the document as it
// answered it, every version's name included, and an accept named one version after the
// suggestion's body. After the author deletes the suggestion, the delete renames that version for
// every fresh read, but a repeat of the rename's key replays the name the key kept. The tester
// found it at 286089e7 for PATCH /api/v1/artifacts/{id} and for an upload to an existing
// document. The red row of that round.
func TestAKeyedDocumentRenameRepeatedAfterTheDeleteGivesNoVersionNamedAfterTheBody(t *testing.T) {
	t.Parallel()
	_, handler, _ := browserDocumentService(t)
	issue := createInteractionIssue(t, handler, "TEST", "Rename a document an accepted suggestion named", "The quick brown fox")
	const phrase = "zarquonrenamedversion"
	// The issue's spec keeps its name (ARTIFACT_PRIMARY), so the suggestion is made on a second
	// document of the issue, which a rename reaches.
	uploaded := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/artifacts",
		map[string]any{"name": "notes.md", "content": "alpha bravo charlie"}, "alice")
	if uploaded.Code != http.StatusCreated {
		t.Fatalf("upload notes.md: status=%d body=%s", uploaded.Code, uploaded.Body.String())
	}
	var notes struct {
		Artifact struct {
			ID   string `json:"id"`
			Slug string `json:"slug"`
		} `json:"artifact"`
	}
	if err := json.Unmarshal(uploaded.Body.Bytes(), &notes); err != nil || notes.Artifact.ID == "" {
		t.Fatalf("decode the upload's answer (%v): %s", err, uploaded.Body.String())
	}
	comment := createThreadComment(t, handler, issue.Key, map[string]any{
		"body": "Say this instead, " + phrase + ".", "anchor": map[string]any{"artifact": notes.Artifact.Slug, "quote": "alpha"},
		"suggestion": map[string]string{"replace_with": "omega"},
	}, "alice")
	if accepted := dispatchRequest(t, handler, http.MethodPost, "/api/v1/comments/"+comment.ID+"/accept", map[string]any{}, "alice"); accepted.Code != http.StatusOK {
		t.Fatalf("accept: status=%d body=%s", accepted.Code, accepted.Body.String())
	}
	target := "/api/v1/artifacts/" + notes.Artifact.ID
	rename := map[string]any{"name": "renamed-notes.md"}
	first := keyedRequest(t, handler, http.MethodPatch, target, rename, "alice", "rename-then-delete")
	if first.Code != http.StatusOK || !strings.Contains(first.Body.String(), phrase) {
		t.Fatalf("the first rename: status=%d body=%s, want 200 with the version the accept named, so this row can fail", first.Code, first.Body.String())
	}

	deleteThreadComment(t, handler, comment.ID, "alice")

	again := keyedRequest(t, handler, http.MethodPatch, target, rename, "alice", "rename-then-delete")
	if strings.Contains(again.Body.String(), phrase) {
		t.Fatalf("the repeat of the rename's key after the delete (status %d) gives the deleted suggestion's text %q: %s",
			again.Code, phrase, again.Body.String())
	}
}

// A refusal kept under a key holds no part of any post's body, only the id of the post it names,
// so a delete of that post gives the repeat of the key nothing to withhold. A reply naming a
// block its parent does not have is refused 400 INVALID_COMMENT, naming the parent; after the
// parent is deleted, the head's repeat answers 400 ANSWER_NOT_KEPT, "Dispatch ran this request
// ... read what the request wrote to see it", for a request that wrote nothing, and the refusal's
// own code is lost (test round at 5bd5c11d). The repeat keeps the refusal it was given.
func TestAKeptRefusalRepeatedAfterTheDeleteOfThePostItNamesIsTheSameRefusal(t *testing.T) {
	t.Parallel()
	handler, _ := newTestHandlerWithStore(t)
	issue := createInteractionIssue(t, handler, "TEST", "A refusal naming a parent that is then deleted", "before")
	parent := createThreadComment(t, handler, issue.Key, map[string]any{"body": "A parent of one paragraph."}, "alice")
	target := "/api/v1/issues/" + issue.Key + "/comments"
	reply := map[string]any{"body": "A reply to a block the parent lacks.", "reply_to": parent.ID, "reply_to_block": "b9"}
	first := keyedRequest(t, handler, http.MethodPost, target, reply, "alice", "refusal-then-delete")
	if first.Code != http.StatusBadRequest || !strings.Contains(first.Body.String(), "INVALID_COMMENT") {
		t.Fatalf("the first reply: status=%d body=%s, want 400 INVALID_COMMENT, so this row can fail", first.Code, first.Body.String())
	}

	deleteThreadComment(t, handler, parent.ID, "alice")

	again := keyedRequest(t, handler, http.MethodPost, target, reply, "alice", "refusal-then-delete")
	if again.Code != first.Code || again.Body.String() != first.Body.String() {
		t.Fatalf("the repeat of the refused reply's key after the parent's delete answered %d %s, want the refusal it was given, %d %s",
			again.Code, again.Body.String(), first.Code, first.Body.String())
	}
}
